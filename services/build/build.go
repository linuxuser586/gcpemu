// Package build emulates Cloud Build's Builds API (cloudbuild.googleapis.com
// v1) over gRPC and REST on the API gateway, on both projects/*/builds and
// projects/*/locations/*/builds. Build steps run in order or by waitFor as
// Managed containers that share a per-Build /workspace volume (steps.go),
// at most build.concurrency Builds at a time while the rest wait QUEUED
// (runner.go). timeout and queueTtl run on the Emulator clock. Step logs
// are kept in memory and served at /_emu/v1/build/<build name>/log
// (logs.go). Cloud Build v2 answers UNIMPLEMENTED.
package build

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/inflight"
	"github.com/linuxuser586/gcpemu/internal/lro"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// sweepInterval is how often queueTtl and timeout are checked without a
// clock advance.
const sweepInterval = time.Second

// Service is the Cloud Build Service.
type Service struct {
	env  *emu.Env
	api  *api
	ops  *lro.Manager
	logs logs

	kickC chan struct{}
	// loops runs the scheduler; Reset restarts it (#307).
	loops inflight.Loops

	// mu guards queue, live and paused, and every change of a Build's
	// status away from QUEUED.
	mu sync.Mutex
	// queue lists the QUEUED Builds this process may start, oldest
	// first. Builds loaded from a State snapshot are never in it.
	queue []buildRef
	// live are the WORKING Builds, by key.
	live map[string]*liveBuild
	// paused stops Builds starting between Reset (or Stop) and ResetDone.
	paused bool
	// work counts running Builds, which Reset and Stop wait for.
	work inflight.Tracker

	// netMu serializes creating the Build network.
	netMu sync.Mutex

	probeMu sync.Mutex
	checked time.Time
	probed  error
}

var (
	_ emu.Service         = (*Service)(nil)
	_ emu.ClockObserver   = (*Service)(nil)
	_ emu.Resetter        = (*Service)(nil)
	_ emu.ResetFinisher   = (*Service)(nil)
	_ emu.Snapshotter     = (*Service)(nil)
	_ emu.ResourceCounter = (*Service)(nil)
	_ emu.EnvVarer        = (*Service)(nil)
)

// New returns the service.
func New(env *emu.Env) emu.Service {
	s := &Service{env: env, kickC: make(chan struct{}, 1), live: map[string]*liveBuild{}}
	s.api = &api{s: s}
	s.ops = lro.NewManager(env, "build")
	s.ops.OnCancel = s.cancelOp
	return s
}

func (s *Service) Name() string { return "build" }

// Register installs the gRPC service, the REST surface and the log
// endpoint. The Build Operations are not listed with the Instance's other
// Operations: a State snapshot waits for those to finish, but must be
// possible while a Build runs.
func (s *Service) Register(r emu.Router) error {
	cloudbuildpb.RegisterCloudBuildServer(r.GRPC(), s.api)
	s.ops.Register(r)
	r.Mount("cloudbuild", []string{apiHost}, s.restHandler())
	r.Handle("GET /_emu/v1/build/projects/{project}/locations/{location}/builds/{id}/log", http.HandlerFunc(s.serveLog))
	return nil
}

// Start ends the Builds a previous process left unfinished and runs the
// scheduler.
func (s *Service) Start(ctx context.Context) error {
	s.interrupt("The Build was interrupted by an emulator restart.")
	s.loops.Start(func(ctx context.Context) { s.loop(ctx) })
	s.kick()
	return nil
}

// Stop stops the scheduler and the running Builds, removing their
// containers; the next Start ends them INTERNAL_ERROR.
func (s *Service) Stop(ctx context.Context) error {
	_ = s.loops.Stop(ctx)
	s.halt(errShutdown)
	err := s.work.Wait(ctx)
	s.ops.Close()
	return err
}

// Reset stops the scheduler and every Build, waiting (bounded by ctx)
// until their containers and volumes are removed, so that none of them
// writes to the store after the Instance resets it (#307).
func (s *Service) Reset(ctx context.Context) error {
	select {
	case <-s.kickC:
	default:
	}
	if err := s.loops.Reset(ctx); err != nil {
		return fmt.Errorf("build: waiting for the scheduler: %w", err)
	}
	s.halt(inflight.ErrReset)
	if err := s.work.Wait(ctx); err != nil {
		return fmt.Errorf("build: waiting for running Builds: %w", err)
	}
	s.logs.clear()
	return s.ops.Reset(ctx)
}

// ResetDone runs the scheduler again once the store is reset too.
func (s *Service) ResetDone() {
	s.mu.Lock()
	s.paused = false
	s.mu.Unlock()
	s.loops.Resume()
}

// halt stops Builds starting and stops the running ones with cause.
func (s *Service) halt(cause error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paused = true
	s.queue = nil
	for _, r := range s.live {
		r.stop(cause)
	}
}

// Snapshot implements emu.Snapshotter: a Build is all in the store.
func (s *Service) Snapshot(ctx context.Context, dir string) error { return nil }

// Restore implements emu.Snapshotter. A Build that was QUEUED or WORKING
// when the State snapshot was saved cannot go on, since its containers
// are gone: it ends INTERNAL_ERROR.
func (s *Service) Restore(ctx context.Context, dir string) error {
	s.interrupt("The Build was running when the State snapshot it was restored from was saved.")
	return nil
}

// Ready reports whether Build steps can run: a container runtime must be
// reachable. The result is cached for a few seconds.
func (s *Service) Ready() error {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	if !s.checked.IsZero() && time.Since(s.checked) < 5*time.Second {
		return s.probed
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var err error
	if s.env.Containers == nil {
		err = fmt.Errorf("build: no container runtime support in this build")
	} else if _, rerr := s.env.Containers.Runtime(ctx); rerr != nil {
		err = fmt.Errorf("build: Build steps need a container runtime: %w", rerr)
	}
	s.checked, s.probed = time.Now(), err
	return err
}

// ClockAdvanced expires QUEUED Builds and times out WORKING ones that the
// advance made due, before returning.
func (s *Service) ClockAdvanced(ctx context.Context) error {
	s.schedule(ctx)
	return nil
}

// ResourceCounts implements emu.ResourceCounter.
func (s *Service) ResourceCounts() map[string]int {
	return store.Counts(s.env.Store, map[string]string{"builds": nsBuilds})
}

// EnvVars implements emu.EnvVarer (FR-CORE-004): the gcloud endpoint
// override for `gcloud builds`.
func (s *Service) EnvVars(gw string, endpoints map[string]string) map[string]string {
	return map[string]string{"CLOUDSDK_API_ENDPOINT_OVERRIDES_CLOUDBUILD": "http://" + gw + "/cloudbuild/"}
}

// check verifies the caller holds perm on resource.
func (s *Service) check(ctx context.Context, perm, resource string) error {
	return s.env.Auth.Check(ctx, perm, resource)
}
