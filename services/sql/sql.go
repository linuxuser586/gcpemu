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
	"github.com/linuxuser586/gcpemu/internal/loopback"
	"github.com/linuxuser586/gcpemu/internal/netplane"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/internal/store"
	"github.com/linuxuser586/gcpemu/services/sql/proxy"
)

func init() {
	agent.Register(proxy.Name, proxy.Main)
	agent.Register(proxy.QueryName, proxy.QueryMain)
}

// Service is the Cloud SQL service module.
type Service struct {
	env *emu.Env
	mux *http.ServeMux

	// bgMu guards bg, the current generation of background work
	// (operations, recovery, the expiry loop). Reset cancels it, waits for
	// it and starts the next; Stop cancels it until Start starts the next.
	// A generation's own fields are safe to use once read.
	bgMu sync.Mutex
	bg   *background

	locksMu sync.Mutex
	locks   map[string]*sync.Mutex

	portMu sync.Mutex

	// loopMu guards exposed and the allocation of loopback addresses.
	loopMu  sync.Mutex
	exposed map[string][]*netplane.Exposed // "P/I" → its loopback listeners
	// aliases reports whether the host has loopback aliases.
	aliases func() bool
	// aliasRefused says the runtime refused to publish on a loopback
	// alias, so a Containerized Instance uses only 127.0.0.1 (ADR 0012).
	aliasRefused atomic.Bool

	recovering atomic.Int32

	// sweepMu serialises backup expiry sweeps (and Reset with them).
	sweepMu sync.Mutex
	// inUseMu guards inUse: the backups ("P/I/backupKey") restores are
	// about to read, which a sweep must not delete yet.
	inUseMu sync.Mutex
	inUse   map[string]int
}

// New returns the service.
func New(env *emu.Env) emu.Service {
	s := &Service{env: env, locks: map[string]*sync.Mutex{}, exposed: map[string][]*netplane.Exposed{}, aliases: loopback.Aliases,
		inUse: map[string]int{}}
	s.mux = s.routes()
	s.bg = newBackground()
	return s
}

// errReset is the cause of a background context that Reset cancelled.
// Work stopped by it records no failure: the store is reset after it.
var errReset = errors.New("sql: background work cancelled by Reset")

// background is one generation of the service's background work.
type background struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	wg     sync.WaitGroup
	// expiry is set once the generation runs the backup expiry loop, which
	// Reset starts again in the next one. Guarded by Service.bgMu.
	expiry bool
}

func newBackground() *background {
	g := &background{}
	g.ctx, g.cancel = context.WithCancelCause(context.Background())
	return g
}

// wait waits for the generation's goroutines, or for ctx to end.
func (g *background) wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() { g.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// track counts one goroutine into the current background generation and
// returns it, or nil if Stop cancelled it; the goroutine calls g.wg.Done
// when it returns. Reset and Stop cancel a generation under bgMu, so a
// cancelled generation gains no goroutines while it is waited for.
func (s *Service) track() *background {
	s.bgMu.Lock()
	defer s.bgMu.Unlock()
	if s.bg.ctx.Err() != nil {
		return nil
	}
	s.bg.wg.Add(1)
	return s.bg
}

// wasReset reports whether ctx was cancelled by Reset.
func wasReset(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errReset)
}

func (s *Service) Name() string { return "sql" }

// Register mounts the API: "/sql/v1beta4/..." at the gateway root (gcloud,
// OpenTofu and the Cloud SQL connector address it there) and everything
// (v1beta4, v1 and the agent endpoint) under "/sqladmin/" and in host mode
// for sqladmin.googleapis.com.
func (s *Service) Register(r emu.Router) error {
	r.Handle("/sql/v1beta4/", s)
	r.Mount("sqladmin", []string{apiHost}, s)
	r.Handle("GET /_emu/v1/sql/hosts", http.HandlerFunc(s.serveHosts))
	return nil
}

// Start marks operations interrupted by a previous shutdown as failed and
// brings back the containers of running instances (FR-CORE-031) in the
// background; Ready reports not-ready until they are up.
func (s *Service) Start(ctx context.Context) error {
	s.bgMu.Lock()
	if s.bg.ctx.Err() != nil {
		s.bg = newBackground()
	}
	s.bg.expiry = true
	s.bgMu.Unlock()
	s.failInterruptedOps()
	s.goBackground(s.expiryLoop)
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
		case in.State == "PENDING_CREATE" || in.State == "FAILED" && rec.HostPort == 0:
			// Never started: there is no data to bring back.
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
					s.markFailed(ctx, p, n)
					return
				}
				_ = s.setState(p, n, "RUNNABLE")
			})
		}
	}
	return nil
}

// ReservedAddrs implements emu.AddressReserver: the public and private IPs
// instances ask for again when their containers are recreated.
func (s *Service) ReservedAddrs() []string {
	var out []string
	_ = s.env.Store.View(func(tx store.Tx) error {
		scanJSON(tx, nsInstances, "", func(_ string, rec *instanceRecord) {
			for _, ip := range []string{rec.PublicIP, rec.PrivateIP} {
				if ip != "" {
					out = append(out, ip)
				}
			}
		})
		return nil
	})
	return out
}

// Stop cancels background work and waits for it.
func (s *Service) Stop(ctx context.Context) error {
	s.bgMu.Lock()
	g := s.bg
	g.cancel(context.Canceled)
	s.bgMu.Unlock()
	defer s.closeAllLoopback()
	return g.wait(ctx)
}

// Ready reports not-ready while instances are being recovered after a
// restart.
func (s *Service) Ready() error {
	if n := s.recovering.Load(); n > 0 {
		return fmt.Errorf("starting %d Cloud SQL instance(s)", n)
	}
	return nil
}

// ResourceCounts implements emu.ResourceCounter.
func (s *Service) ResourceCounts() map[string]int {
	return store.Counts(s.env.Store, map[string]string{
		"instances": nsInstances, "databases": nsDatabases, "users": nsUsers, "backupRuns": nsBackups, "sslCerts": nsSSLCerts,
	})
}

// Reset cancels the background work started before it and waits for it
// (bounded by ctx), so none of it creates a container or writes a record
// afterwards (#306). It then removes every instance container and volume
// and the backup files (`gcpemu reset`). Work started after Reset runs on
// a fresh background context, unless the service is stopped.
func (s *Service) Reset(ctx context.Context) error {
	s.bgMu.Lock()
	old := s.bg
	expiry := false
	if old.ctx.Err() == nil {
		s.bg = newBackground()
		s.bg.expiry, expiry = old.expiry, old.expiry
	}
	old.cancel(errReset)
	s.bgMu.Unlock()
	err := old.wait(ctx)
	if expiry {
		s.goBackground(s.expiryLoop)
	}
	if err != nil {
		return fmt.Errorf("sql: waiting for background work: %w", err)
	}
	s.closeAllLoopback()
	s.sweepMu.Lock()
	s.removeAllBackupFiles()
	s.sweepMu.Unlock()
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

// goBackground runs fn on the background context. It reports false, and
// does not run fn, if the service is stopped.
func (s *Service) goBackground(fn func(ctx context.Context)) bool {
	g := s.track()
	if g == nil {
		return false
	}
	go func() {
		defer g.wg.Done()
		fn(g.ctx)
	}()
	return true
}

// clearFailed marks a FAILED instance RUNNABLE once its container has been
// started or stopped again.
func (s *Service) clearFailed(project, name string) error {
	return s.updateRecord(project, name, func(rec *instanceRecord) error {
		if rec.Instance.State == "FAILED" {
			rec.Instance.State = "RUNNABLE"
		}
		return nil
	})
}

// markFailed marks an instance FAILED after its work on ctx failed,
// unless Reset stopped that work.
func (s *Service) markFailed(ctx context.Context, project, name string) {
	if wasReset(ctx) {
		return
	}
	_ = s.setState(project, name, "FAILED")
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
