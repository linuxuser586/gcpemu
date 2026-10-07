// Package sql emulates Cloud SQL for PostgreSQL (SRS 5.6). The control
// plane is the sqladmin REST API (v1beta4 and v1) with its own Operation
// resource; each instance's data plane is a real PostgreSQL server in a
// container whose entrypoint is the gcpemu sql-proxy agent (package proxy),
// which enforces authorized networks, SSL modes and IAM database
// authentication and terminates the Cloud SQL connector's TLS on port 3307.
package sql

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linuxuser586/gcpemu/internal/agent"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/internal/store"
	"github.com/linuxuser586/gcpemu/services/sql/proxy"
)

func init() { agent.Register(proxy.Name, proxy.Main) }

// Service is the Cloud SQL service module.
type Service struct {
	env *emu.Env
	mux *http.ServeMux

	bgCtx    context.Context
	bgCancel context.CancelFunc
	wg       sync.WaitGroup

	locksMu sync.Mutex
	locks   map[string]*sync.Mutex

	portMu sync.Mutex

	recovering atomic.Int32
}

// New returns the service.
func New(env *emu.Env) emu.Service {
	s := &Service{env: env, locks: map[string]*sync.Mutex{}}
	s.mux = s.routes()
	s.bgCtx, s.bgCancel = context.WithCancel(context.Background())
	return s
}

func (s *Service) Name() string { return "sql" }

// Register mounts the API: "/sql/v1beta4/..." at the gateway root (gcloud,
// OpenTofu and the Cloud SQL connector address it there) and everything
// (v1beta4, v1 and the agent endpoint) under "/sqladmin/" and in host mode
// for sqladmin.googleapis.com.
func (s *Service) Register(r emu.Router) error {
	r.Handle("/sql/v1beta4/", s)
	r.Mount("sqladmin", []string{apiHost}, s)
	return nil
}

// Start marks operations interrupted by a previous shutdown as failed and
// brings back the containers of running instances (FR-CORE-031) in the
// background; Ready reports not-ready until they are up.
func (s *Service) Start(ctx context.Context) error {
	if s.bgCtx.Err() != nil {
		s.bgCtx, s.bgCancel = context.WithCancel(context.Background())
	}
	s.failInterruptedOps()
	var recs []*instanceRecord
	_ = s.env.Store.View(func(tx store.Tx) error {
		scanJSON(tx, nsInstances, "", func(_ string, rec *instanceRecord) {
			if rec.Instance != nil {
				recs = append(recs, rec)
			}
		})
		return nil
	})
	for _, rec := range recs {
		in := rec.Instance
		switch {
		case in.State == "PENDING_DELETE":
			s.goBackground(func(ctx context.Context) { _ = s.destroyInstance(ctx, in.Project, in.Name) })
		case in.Settings != nil && in.Settings.ActivationPolicy == "NEVER":
		case in.State == "PENDING_CREATE" || in.State == "FAILED":
			_ = s.setState(in.Project, in.Name, "FAILED")
		default:
			s.recovering.Add(1)
			p, n := in.Project, in.Name
			_ = s.setState(p, n, "MAINTENANCE")
			s.goBackground(func(ctx context.Context) {
				defer s.recovering.Add(-1)
				mu := s.lock(p, n)
				defer mu.Unlock()
				if err := s.startContainer(ctx, p, n); err != nil {
					s.env.Log.Error("sql: recovering instance failed", "instance", p+":"+n, "err", err)
					_ = s.setState(p, n, "FAILED")
					return
				}
				_ = s.setState(p, n, "RUNNABLE")
			})
		}
	}
	return nil
}

// Stop cancels background work and waits for it.
func (s *Service) Stop(ctx context.Context) error {
	s.bgCancel()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Ready reports not-ready while instances are being recovered after a
// restart.
func (s *Service) Ready() error {
	if n := s.recovering.Load(); n > 0 {
		return fmt.Errorf("starting %d Cloud SQL instance(s)", n)
	}
	return nil
}

// Reset removes every instance container and volume (`gcpemu reset`).
func (s *Service) Reset(ctx context.Context) error {
	var any bool
	_ = s.env.Store.View(func(tx store.Tx) error {
		any = store.HasPrefix(tx, nsInstances, "")
		return nil
	})
	if !any {
		return nil
	}
	rt, err := s.env.Containers.Runtime(ctx)
	if err != nil {
		return err
	}
	return removeLabelled(ctx, rt, map[string]string{runtime.LabelInstance: rt.InstanceID, runtime.LabelService: "sql"})
}

// removeLabelled removes the containers and volumes carrying labels.
func removeLabelled(ctx context.Context, rt *runtime.Manager, labels map[string]string) error {
	var errs []error
	cs, err := rt.ListContainers(ctx, labels)
	errs = append(errs, err)
	for _, c := range cs {
		errs = append(errs, rt.RemoveContainer(ctx, c.ID, true))
	}
	vols, err := rt.ListVolumes(ctx, labels)
	errs = append(errs, err)
	for _, v := range vols {
		errs = append(errs, rt.RemoveVolume(ctx, v))
	}
	return errors.Join(errs...)
}

// EnvVars implements emu.EnvVarer. gcloud's sql API client uses base URL
// https://sqladmin.googleapis.com/ with paths "sql/v1beta4/...", so the
// override is the "/sqladmin/" mount (which also accepts the version-less
// form). Each instance's published PostgreSQL port is exported as
// GCPEMU_SQL_<INSTANCE> (upper-cased, '-' → '_') = host:port.
func (s *Service) EnvVars(gateway string, _ map[string]string) map[string]string {
	out := map[string]string{
		"CLOUDSDK_API_ENDPOINT_OVERRIDES_SQL": "http://" + gateway + "/sqladmin/",
	}
	for name, addr := range s.HostAddrs() {
		_, inst := splitKey(name)
		out["GCPEMU_SQL_"+strings.ToUpper(strings.ReplaceAll(inst, "-", "_"))] = addr
	}
	return out
}

// HostAddrs returns "P/I" → host:port of every instance's published
// PostgreSQL port.
func (s *Service) HostAddrs() map[string]string {
	out := map[string]string{}
	_ = s.env.Store.View(func(tx store.Tx) error {
		scanJSON(tx, nsInstances, "", func(k string, rec *instanceRecord) {
			if rec.HostPort > 0 {
				out[k] = fmt.Sprintf("%s:%d", s.hostIP(), rec.HostPort)
			}
		})
		return nil
	})
	return out
}

// lock returns the held per-instance mutex serializing data-plane work.
func (s *Service) lock(project, name string) *sync.Mutex {
	s.locksMu.Lock()
	mu, ok := s.locks[instKey(project, name)]
	if !ok {
		mu = &sync.Mutex{}
		s.locks[instKey(project, name)] = mu
	}
	s.locksMu.Unlock()
	mu.Lock()
	return mu
}

// goBackground runs fn on the service's background context.
func (s *Service) goBackground(fn func(ctx context.Context)) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		fn(s.bgCtx)
	}()
}

// setState updates an instance's state.
func (s *Service) setState(project, name, state string) error {
	return s.updateRecord(project, name, func(rec *instanceRecord) error {
		rec.Instance.State = state
		return nil
	})
}

// updateRecord applies fn to a stored instance record.
func (s *Service) updateRecord(project, name string, fn func(rec *instanceRecord) error) error {
	return s.env.Store.Update(func(tx store.Tx) error {
		rec, ok := getInstance(tx, project, name)
		if !ok {
			return errInstanceNotFound()
		}
		if err := fn(rec); err != nil {
			return err
		}
		return store.PutJSON(tx, nsInstances, instKey(project, name), rec)
	})
}

// loadRecord reads an instance record.
func (s *Service) loadRecord(project, name string) (*instanceRecord, error) {
	var rec *instanceRecord
	var ok bool
	_ = s.env.Store.View(func(tx store.Tx) error {
		rec, ok = getInstance(tx, project, name)
		return nil
	})
	if !ok {
		return nil, errInstanceNotFound()
	}
	return rec, nil
}

type systemKey struct{}

// system marks a context as an internal caller that skips IAM checks.
func system(ctx context.Context) context.Context { return context.WithValue(ctx, systemKey{}, true) }

// check authorizes the caller (FR-INT-012).
func (s *Service) check(ctx context.Context, permission, resource string) error {
	if v, _ := ctx.Value(systemKey{}).(bool); v {
		return nil
	}
	return s.env.Auth.Check(ctx, permission, resource)
}

// now returns the emulator clock as RFC3339.
func (s *Service) now() string { return s.env.Clock.Now().UTC().Format(time.RFC3339Nano) }

// sortedKeys returns map keys in order.
func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
