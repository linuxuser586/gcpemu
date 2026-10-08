// Package gcs emulates Cloud Storage (SRS 5.4): the JSON API v1 (buckets,
// objects, uploads, notifications, IAM, HMAC keys), the XML API subset used
// by the official clients for reads and by S3-style tools, V4 signed URLs
// and POST policies, Pub/Sub notifications and lifecycle rules. Object data
// is stored in files under the service data directory.
package gcs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/gateway"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Name is the service name.
const Name = "gcs"

// lifecycleInterval is how often the background lifecycle evaluator runs
// (FR-GCS-008). Tests and `gcpemu time advance` can call EvaluateLifecycle
// directly.
var lifecycleInterval = 10 * time.Second

// Service is the Cloud Storage service module.
type Service struct {
	env   *emu.Env
	log   *slog.Logger
	blobs *blobStore

	srv    *http.Server
	cancel context.CancelFunc
	wg     sync.WaitGroup
	ready  atomic.Bool

	// sessions serialises chunk writes per resumable upload and caches the
	// running checksums of each session.
	sessMu   sync.Mutex
	sessions map[string]*session
}

// New returns the service.
func New(env *emu.Env) emu.Service {
	return &Service{env: env, log: env.Log.With("service", Name), sessions: map[string]*session{}}
}

// Name implements emu.Service.
func (s *Service) Name() string { return Name }

// Register mounts the JSON API paths at the gateway root (the official
// clients use "/storage/v1/", "/upload/storage/v1/" and
// "/download/storage/v1/"), plus "/storage/" for prefix routing and
// storage.googleapis.com for host mode.
func (s *Service) Register(r emu.Router) error {
	for _, p := range []string{"/storage/v1/", "/upload/storage/v1/", "/download/storage/v1/", "/batch/storage/v1", "/batch/storage/v1/"} {
		r.Handle(p, s)
	}
	r.Mount("storage", []string{"storage.googleapis.com"}, s)
	// XML-style /BUCKET/OBJECT reads from clients pointed at the gateway root.
	r.Fallback(s)
	return nil
}

// ClockAdvanced re-evaluates lifecycle rules after `gcpemu time advance`
// (FR-GCS-008).
func (s *Service) ClockAdvanced(ctx context.Context) error { return s.EvaluateLifecycle(ctx) }

// Start opens the per-service port (default 4443, for STORAGE_EMULATOR_HOST)
// and starts the lifecycle evaluator.
func (s *Service) Start(ctx context.Context) error {
	dir, err := s.env.ServiceDir(Name)
	if err != nil {
		return err
	}
	s.blobs = &blobStore{dir: dir, noSync: s.env.Config.Ephemeral}
	if err := s.blobs.init(); err != nil {
		return err
	}
	l, err := s.env.Listen(Name)
	if err != nil {
		return fmt.Errorf("gcs listen: %w", err)
	}
	s.srv = gateway.NewServer(s.env.Middleware(Name, s), s.log)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := s.srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("gcs listener stopped", "err", err)
		}
	}()
	bg, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.wg.Add(1)
	go s.lifecycleLoop(bg)
	s.ready.Store(true)
	return nil
}

// Stop shuts the listener and background work down.
func (s *Service) Stop(ctx context.Context) error {
	s.ready.Store(false)
	if s.cancel != nil {
		s.cancel()
	}
	var err error
	if s.srv != nil {
		err = s.srv.Shutdown(ctx)
	}
	s.wg.Wait()
	return err
}

// Ready implements emu.Service.
func (s *Service) Ready() error {
	if !s.ready.Load() {
		return errors.New("gcs: starting")
	}
	return nil
}

// ResourceCounts implements emu.ResourceCounter.
func (s *Service) ResourceCounts() map[string]int {
	return store.Counts(s.env.Store, map[string]string{
		"buckets": nsBuckets, "objects": nsObjects, "notificationConfigs": nsNotifs, "hmacKeys": nsHMAC,
	})
}

// Reset implements emu.Resetter: object data files are deleted; the core
// clears the metadata store.
func (s *Service) Reset(ctx context.Context) error {
	s.sessMu.Lock()
	s.sessions = map[string]*session{}
	s.sessMu.Unlock()
	if s.blobs == nil {
		return nil
	}
	return s.blobs.reset()
}

// EnvVars implements emu.EnvVarer (FR-CORE-004).
func (s *Service) EnvVars(gw string, endpoints map[string]string) map[string]string {
	out := map[string]string{}
	if ep := endpoints[Name]; ep != "" {
		out["STORAGE_EMULATOR_HOST"] = ep
	}
	if gw != "" {
		out["CLOUDSDK_API_ENDPOINT_OVERRIDES_STORAGE"] = "http://" + gw + "/storage/v1/"
	}
	return out
}

func (s *Service) lifecycleLoop(ctx context.Context) {
	defer s.wg.Done()
	t := time.NewTicker(lifecycleInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.EvaluateLifecycle(ctx); err != nil {
				s.log.Warn("lifecycle evaluation failed", "err", err)
			}
		}
	}
}
