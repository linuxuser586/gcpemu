// Package instance assembles and runs one emulator instance: state store,
// gateway, admin API and the selected services.
package instance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/linuxuser586/gcpemu/internal/ca"
	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/fault"
	"github.com/linuxuser586/gcpemu/internal/gateway"
	"github.com/linuxuser586/gcpemu/internal/reqlog"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Factory builds a service for an environment.
type Factory func(env *emu.Env) emu.Service

// Version is set at build time.
var Version = "dev"

// Files written in the instance directory.
const (
	EndpointsFile = "endpoints.json"
	PIDFile       = "gcpemu.pid"
	LogFile       = "gcpemu.log"
	StateFile     = "state.db"
	IDFile        = "instance-id"
)

// Instance is a running emulator.
type Instance struct {
	Config *config.Config
	Env    *emu.Env
	Log    *reqlog.Log
	ID     string
	Dir    string

	gw       *gateway.Gateway
	services []emu.Service
	byName   map[string]emu.Service
	failedMu sync.RWMutex
	failed   map[string]error
	tmpDir   string
	clock    *clock.Offset

	containers   *containers
	shutdownOnce sync.Once
	done         chan struct{}
}

// New builds an instance from cfg using factories for available services.
func New(cfg *config.Config, factories map[string]Factory, logOut io.Writer) (*Instance, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	dir := cfg.InstanceDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	logger := newLogger(cfg, logOut)

	in := &Instance{Config: cfg, Dir: dir, byName: map[string]emu.Service{}, failed: map[string]error{}, done: make(chan struct{})}
	in.ID = instanceID(dir, cfg.Ephemeral)

	var st store.Store
	dataDir := filepath.Join(dir, "data")
	if cfg.Ephemeral {
		st = store.NewMemory()
		tmp, err := os.MkdirTemp("", "gcpemu-"+cfg.Instance+"-")
		if err != nil {
			return nil, err
		}
		in.tmpDir, dataDir = tmp, tmp
	} else {
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			return nil, err
		}
		b, err := store.OpenBolt(filepath.Join(dir, StateFile))
		if err != nil {
			return nil, fmt.Errorf("open state (is another instance using %s?): %w", dir, err)
		}
		st = b
	}

	var base clock.Clock = clock.Real{}
	if cfg.Deterministic {
		base = clock.NewFake()
	}
	in.clock = clock.NewOffset(base)

	in.Log = reqlog.New(2000, logger.With("component", "request"))
	env := &emu.Env{
		Config:    cfg,
		Store:     st,
		Clock:     in.clock,
		IDs:       emu.NewIDs(cfg.Deterministic),
		Log:       logger,
		DataDir:   dataDir,
		Auth:      emu.NewPolicyAuthorizer(cfg.IAMMode, logger.With("component", "iam")),
		Endpoints: emu.NewEndpoints(),
	}
	authority, err := ca.Load(dir, cfg.Instance)
	if err != nil {
		return nil, fmt.Errorf("certificate authority: %w", err)
	}
	env.CA = authority
	in.Env = env
	in.containers = &containers{in: in}
	env.Containers = in.containers
	in.gw = gateway.New(env, in.Log)
	env.Middleware = in.gw.Middleware
	env.GRPCOptions = in.gw.GRPCOptions

	explicit := map[string]bool{}
	for _, s := range cfg.Services {
		explicit[s] = true
	}
	for _, name := range config.ResolveServices(cfg.Services) {
		f, ok := factories[name]
		if !ok {
			if explicit[name] {
				return nil, fmt.Errorf("service %q is not implemented in this build", name)
			}
			logger.Debug("service not implemented in this build; skipping", "service", name)
			continue
		}
		s := f(env)
		in.services = append(in.services, s)
		in.byName[name] = s
	}
	env.SetServices(in.byName)
	return in, nil
}

func newLogger(cfg *config.Config, out io.Writer) *slog.Logger {
	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(cfg.LogLevel))
	opts := &slog.HandlerOptions{Level: lvl}
	if out == nil {
		out = os.Stderr
	}
	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(out, opts))
	}
	return slog.New(slog.NewTextHandler(out, opts))
}

func instanceID(dir string, ephemeral bool) string {
	p := filepath.Join(dir, IDFile)
	if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
		return string(b)
	}
	id := emu.NewIDs(false).Hex(6)
	_ = ephemeral // persisted in both modes so a crashed run's containers are reclaimed
	_ = os.WriteFile(p, []byte(id), 0o600)
	return id
}

// Services returns the configured services in start order.
func (in *Instance) Services() []emu.Service { return in.services }

// Start registers routes, starts the gateway and every service, and writes
// endpoints.json. A failing service is reported not-ready rather than
// stopping the others (NFR-REL-003).
func (in *Instance) Start(ctx context.Context) error {
	for _, s := range in.services {
		if err := in.gw.Register(s); err != nil {
			in.setFailed(s.Name(), err)
			in.Env.Log.Error("service registration failed", "service", s.Name(), "err", err)
		}
	}
	in.gw.HandleAdmin(newAdmin(in))

	addr := net.JoinHostPort(in.Config.Bind, strconv.Itoa(in.Config.Port("gateway")))
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("gateway listen %s: %w", addr, err)
	}
	in.Env.Endpoints.Set("gateway", l.Addr().String())
	go func() {
		if err := in.gw.Serve(l); err != nil {
			in.Env.Log.Error("gateway stopped", "err", err)
		}
	}()

	for _, s := range in.services {
		if in.failure(s.Name()) != nil {
			continue
		}
		if err := s.Start(ctx); err != nil {
			in.setFailed(s.Name(), err)
			in.Env.Log.Error("service failed to start", "service", s.Name(), "err", err)
		}
	}
	if err := in.writeRuntimeFiles(); err != nil {
		return err
	}
	in.Env.Log.Info("gcpemu started", "instance", in.Config.Instance, "id", in.ID, "gateway", in.Env.Endpoints.Get("gateway"), "dir", in.Dir)
	return nil
}

func (in *Instance) writeRuntimeFiles() error {
	b, _ := json.MarshalIndent(in.Env.Endpoints.All(), "", "  ")
	if err := writeAtomic(filepath.Join(in.Dir, EndpointsFile), b); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(in.Dir, PIDFile), []byte(strconv.Itoa(os.Getpid())))
}

func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (in *Instance) setFailed(name string, err error) {
	in.failedMu.Lock()
	in.failed[name] = err
	in.failedMu.Unlock()
}

func (in *Instance) failure(name string) error {
	in.failedMu.RLock()
	defer in.failedMu.RUnlock()
	return in.failed[name]
}

// Readiness reports each service's readiness: nil error means ready.
func (in *Instance) Readiness() map[string]error {
	out := map[string]error{}
	for _, s := range in.services {
		if err := in.failure(s.Name()); err != nil {
			out[s.Name()] = err
			continue
		}
		out[s.Name()] = s.Ready()
	}
	return out
}

// Ready reports whether every service is ready.
func (in *Instance) Ready() bool {
	for _, err := range in.Readiness() {
		if err != nil {
			return false
		}
	}
	return true
}

// WaitReady blocks until all services are ready or ctx ends.
func (in *Instance) WaitReady(ctx context.Context) error {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		if in.Ready() {
			return nil
		}
		select {
		case <-ctx.Done():
			var names []string
			for n, err := range in.Readiness() {
				if err != nil {
					names = append(names, fmt.Sprintf("%s: %v", n, err))
				}
			}
			sort.Strings(names)
			return fmt.Errorf("services not ready: %v", names)
		case <-t.C:
		}
	}
}

// Reset wipes all state but keeps configuration (FR-CORE-002).
func (in *Instance) Reset(ctx context.Context) error {
	var errs []error
	for _, s := range in.services {
		if r, ok := s.(emu.Resetter); ok {
			errs = append(errs, r.Reset(ctx))
		}
	}
	errs = append(errs, in.Env.Store.Reset())
	in.Log.Clear()
	return errors.Join(errs...)
}

// Faults returns the fault-injection rule set.
func (in *Instance) Faults() *fault.Set { return &in.gw.Faults }

// AdvanceClock moves the emulator clock forward (FR-GCS-008).
func (in *Instance) AdvanceClock(ctx context.Context, d time.Duration) error {
	in.clock.Advance(d)
	var errs []error
	for _, s := range in.services {
		if o, ok := s.(emu.ClockObserver); ok {
			errs = append(errs, o.ClockAdvanced(ctx))
		}
	}
	return errors.Join(errs...)
}

// Done is closed when Shutdown completes.
func (in *Instance) Done() <-chan struct{} { return in.done }

// Shutdown stops services in reverse order, closes the store and removes
// runtime files (FR-CORE-006).
func (in *Instance) Shutdown(ctx context.Context) error {
	var errs []error
	in.shutdownOnce.Do(func() {
		for i := len(in.services) - 1; i >= 0; i-- {
			s := in.services[i]
			if err := s.Stop(ctx); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", s.Name(), err))
			}
		}
		errs = append(errs, in.containers.shutdown())
		errs = append(errs, in.gw.Shutdown(ctx))
		errs = append(errs, in.Env.Store.Close())
		_ = os.Remove(filepath.Join(in.Dir, PIDFile))
		_ = os.Remove(filepath.Join(in.Dir, EndpointsFile))
		if in.tmpDir != "" {
			_ = os.RemoveAll(in.tmpDir)
		}
		close(in.done)
	})
	return errors.Join(errs...)
}

// RequestShutdown triggers Shutdown asynchronously (used by the admin API).
func (in *Instance) RequestShutdown() {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = in.Shutdown(ctx)
	}()
}
