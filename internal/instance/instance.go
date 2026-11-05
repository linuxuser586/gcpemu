// Package instance assembles and runs one emulator instance: state store,
// gateway, admin API and the selected services.
package instance

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linuxuser586/gcpemu/console"
	"github.com/linuxuser586/gcpemu/internal/ca"
	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/events"
	"github.com/linuxuser586/gcpemu/internal/fault"
	"github.com/linuxuser586/gcpemu/internal/frontend"
	"github.com/linuxuser586/gcpemu/internal/gateway"
	"github.com/linuxuser586/gcpemu/internal/hostmode"
	"github.com/linuxuser586/gcpemu/internal/lanbind"
	"github.com/linuxuser586/gcpemu/internal/link"
	"github.com/linuxuser586/gcpemu/internal/loopback"
	"github.com/linuxuser586/gcpemu/internal/reqlog"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/internal/store"
	"github.com/linuxuser586/gcpemu/internal/vmrelay"
	"github.com/linuxuser586/gcpemu/internal/webconsole"
)

// Factory builds a service for an environment.
type Factory func(env *emu.Env) emu.Service

// Version is set at build time.
var Version = "dev"

// ErrShutDown means shutdown has begun, so what was asked of the Instance
// will not happen: it will not become ready, or Seeding will not finish.
var ErrShutDown = errors.New("Instance shut down")

// Files written in the instance directory.
const (
	EndpointsFile = "endpoints.json"
	PIDFile       = "gcpemu.pid"
	LogFile       = "gcpemu.log"
	StateFile     = "state.db"
	IDFile        = "instance-id"
	// AdminTokenFile holds the Admin token while an Instance bound beyond
	// loopback runs (FR-UI-009); CLI commands read it from there.
	AdminTokenFile = "admin-token"
)

// Instance is a running emulator.
type Instance struct {
	Config *config.Config
	Env    *emu.Env
	Log    *reqlog.Log
	// Events is the change feed served at /_emu/v1/events.
	Events *events.Hub
	ID     string
	Dir    string

	gw       *gateway.Gateway
	services []emu.Service
	byName   map[string]emu.Service
	failedMu sync.RWMutex
	failed   map[string]error
	holds    map[string]error // readiness holds (HoldReady), under failedMu
	tmpDir   string
	clock    *clock.Offset

	containers   *containers
	fe           *frontend.Frontend // Google frontend (FR-INT-007, FR-CORE-043)
	hm           *hostmode.Manager  // nil unless --host-mode, and until Start sets it; under startMu
	link         *link.Manager      // links to other Instances (ADR 0027)
	adcMu        sync.Mutex         // serialises serveADCKey
	stateMu      sync.Mutex         // serialises Reset and State snapshots
	shutdownOnce sync.Once
	// stopping is cancelled when shutdown begins, well before done closes:
	// shutdown goes on to remove containers.
	stopping  context.Context
	beginStop context.CancelFunc
	done      chan struct{}
	// startMu decides whether Start or the beginning of shutdown came first
	// (ADR 0065). It guards startReturned, closed when Start returns and nil
	// until it is called, and hm, which Start sets while the Admin API reads
	// it.
	startMu       sync.Mutex
	startReturned chan struct{}

	// managed are the Managed emulators by API (ADR 0029), which start
	// under managedCtx.
	managed       map[string]*managedEmulator
	managedCtx    context.Context
	managedCancel context.CancelFunc
	managedWG     sync.WaitGroup

	// registryRelay starts the Registry relay (ADR 0043).
	registryRelay *registryRelay

	// consoleServed reports whether the gateway serves the Web console.
	consoleServed bool
	// adminToken is the Admin token, "" on a loopback bind (FR-UI-009).
	adminToken string
}

// New builds an instance from cfg using factories for available services.
// When it fails, it has closed the state store and removed the temp dir and
// Admin token file it made (#872).
func New(cfg *config.Config, factories map[string]Factory, logOut io.Writer) (_ *Instance, err error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := refuseHostMode(cfg, goruntime.GOOS, func() (hostModeRuntime, bool) { return hostModeProbe(cfg.VMRuntime) }); err != nil {
		return nil, err
	}
	dir := cfg.InstanceDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	logger := newLogger(cfg, logOut)
	if !config.IsLoopback(cfg.Bind) {
		logger.Warn("listeners are reachable beyond this host; every emulated API, credential and data plane is exposed (FR-CORE-044)",
			"bind", cfg.Bind, "iamMode", cfg.IAMMode)
	}
	if len(cfg.LBBind) > 0 {
		logger.Info("forwarding rules and GKE LoadBalancer Services are also served beyond loopback; nothing else is", "lbBind", cfg.LBBind)
	}
	if len(cfg.GKEBind) > 0 {
		logger.Info("GKE clusters' Kubernetes APIs are also served beyond loopback, to real credentials only", "gkeBind", cfg.GKEBind)
	}
	if len(cfg.ARBind) > 0 {
		logger.Info("the Artifact Registry registry is also served beyond loopback, over TLS to authenticated callers only", "arBind", cfg.ARBind)
	}
	if len(cfg.GFEBind) > 0 {
		logger.Info("the Google front end is also served beyond loopback, over TLS to authenticated callers only", "gfeBind", cfg.GFEBind)
	}
	if len(cfg.SQLBind) > 0 {
		logger.Info("Cloud SQL instances are also served beyond loopback, to their users' credentials only", "sqlBind", cfg.SQLBind)
	}
	if len(cfg.LinkBind) > 0 {
		logger.Info("the link protocol is served beyond loopback, over mutual TLS to paired Instances and to Pairing requests", "linkBind", cfg.LinkBind)
	}

	in := &Instance{Config: cfg, Dir: dir, byName: map[string]emu.Service{}, failed: map[string]error{}, holds: map[string]error{}, done: make(chan struct{})}
	in.stopping, in.beginStop = context.WithCancel(context.Background())
	in.ID = instanceID(dir, cfg.Ephemeral)

	var st store.Store
	defer func() {
		if err == nil {
			return
		}
		if rerr := in.release(st); rerr != nil {
			err = errors.Join(err, rerr)
		}
	}()
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

	in.Events = events.NewHub(4096)
	in.Log = reqlog.New(2000, logger.With("component", "request"))
	in.Log.OnAdd(func(e reqlog.Entry) { in.Events.Publish(events.Request, e) })
	authz := emu.NewPolicyAuthorizer(cfg.IAMMode, logger.With("component", "iam"))
	authz.OnDeny(func(d emu.Denial) { in.Events.Publish(events.IAMDenial, d) })
	env := &emu.Env{
		Config:    cfg,
		Store:     store.Observe(st, events.StoreObserver(in.Events)),
		Clock:     in.clock,
		IDs:       emu.NewIDs(cfg.Deterministic),
		Log:       logger,
		DataDir:   dataDir,
		Auth:      authz,
		Endpoints: emu.NewEndpoints(),
	}
	env.RequestLog = in.Log
	authority, err := ca.Load(dir, cfg.Instance)
	if err != nil {
		return nil, fmt.Errorf("certificate authority: %w", err)
	}
	env.CA = authority
	in.Env = env
	if in.link, err = in.newLink(); err != nil {
		return nil, fmt.Errorf("links: %w", err)
	}
	in.containers = &containers{in: in}
	env.Containers = in.containers
	in.managed = map[string]*managedEmulator{}
	in.managedCtx, in.managedCancel = context.WithCancel(context.Background())
	in.registryRelay = newRegistryRelay()
	in.gw = gateway.New(env, in.Log)
	in.gw.SetLinks(in.link)
	env.Delegated = in.link.DelegatedTo
	env.LinkedIDToken = in.link.LinkedIDToken
	env.LinkedAccessToken = in.link.Authenticate
	env.LinkedSigningKeys = func() []emu.SigningKey {
		ks := in.link.LinkedSigningKeys()
		out := make([]emu.SigningKey, len(ks))
		for i, k := range ks {
			out[i] = emu.SigningKey(k)
		}
		return out
	}
	if err := in.setupAdminToken(); err != nil {
		return nil, err
	}
	env.Middleware = in.gw.Middleware
	env.GRPCOptions = in.gw.GRPCOptions
	in.fe = frontend.New(env, in.gw.Hosts)
	in.fe.SetGateway(in.gw.ServeFrontend)

	explicit := map[string]bool{}
	for _, s := range cfg.Services {
		explicit[s] = true
	}
	selected := config.ResolveServices(cfg.Services)
	if len(cfg.Services) == 0 {
		var skipped []string
		var why error
		selected, skipped, why = defaultServices(func() error { _, err := runtime.Detect(); return err })
		if len(skipped) > 0 {
			logger.Info("no usable container runtime; not starting the Services that need one", "skipped", strings.Join(skipped, ","), "reason", why.Error())
		}
	}
	for _, name := range selected {
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

// hostModeRuntime is what host mode's start-time refusal needs from the
// runtime: whether this is a Containerized Instance on a VM runtime, and
// the VM relay decision of an emulator on the host.
type hostModeRuntime struct {
	VM    runtime.VMDecision
	Relay vmrelay.Decision
}

// refuseHostMode fails when cfg asks for host mode on an OS other than
// Linux (#175), or on Linux in a Containerized Instance on a VM runtime
// (#492) or on the host with the VM relay on because the runtime is a VM
// runtime (#560), before anything starts. probe returns the runtime's
// decisions, or false when the runtime cannot be reached; host mode then
// fails later, saying it needs a container runtime, as it would anyway.
func refuseHostMode(cfg *config.Config, goos string, probe func() (hostModeRuntime, bool)) error {
	if !cfg.HostMode {
		return nil
	}
	if err := hostmode.UnsupportedOn(goos); err != nil {
		return fmt.Errorf("--host-mode: %w", err)
	}
	d, ok := probe()
	if !ok {
		return nil
	}
	if err := hostmode.UnsupportedVM(d.VM); err != nil {
		return fmt.Errorf("--host-mode: %w", err)
	}
	if err := hostmode.UnsupportedRelay(d.Relay); err != nil {
		return fmt.Errorf("--host-mode: %w", err)
	}
	return nil
}

// hostModeProbe decides, as the runtime manager and the netplane will,
// whether this Instance is a Containerized Instance on a VM runtime and
// whether it uses the VM relay, or reports false when the runtime cannot
// be reached within a few seconds. The services network does not exist
// yet, so the default bridge network stands in for the services network,
// as in gcpemu doctor; without one the relay is decided off. A variable
// for tests.
var hostModeProbe = func(setting string) (hostModeRuntime, bool) {
	cl, err := runtime.Detect()
	if err != nil {
		return hostModeRuntime{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := cl.Info(ctx)
	if err != nil {
		return hostModeRuntime{}, false
	}
	r := hostModeRuntime{VM: runtime.DecideVM(setting, cl.Self() != "", info)}
	if cl.Self() == "" {
		if d, ok := vmrelay.DecideDefault(ctx, goruntime.GOOS, false, cl, info, vmrelay.HostHolder); ok {
			r.Relay = d
		}
	}
	return r, true
}

// defaultServices is every Service, less the Runtime Services when detect
// finds no usable container runtime (e.g. the OCI image without the Docker
// socket, or Docker Desktop stopped); why says what detect found.
func defaultServices(detect func() error) (selected, skipped []string, why error) {
	selected = config.ResolveServices(nil)
	why = detect()
	if why == nil {
		return selected, nil, nil
	}
	selected = slices.DeleteFunc(selected, func(s string) bool {
		if config.RuntimeServices[s] {
			skipped = append(skipped, s)
			return true
		}
		return false
	})
	return selected, skipped, why
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
// stopping the others (NFR-REL-003). Once shutdown has begun it returns
// ErrShutDown, however starting ended: it does nothing if shutdown began
// first, and otherwise starts nothing more from the next check on. Teardown
// waits for it to return (ADR 0065).
func (in *Instance) Start(ctx context.Context) error {
	in.startMu.Lock()
	if in.stopping.Err() != nil {
		in.startMu.Unlock()
		return ErrShutDown
	}
	returned := make(chan struct{})
	in.startReturned = returned
	in.startMu.Unlock()
	defer close(returned)
	return in.unlessStopping(in.start(ctx))
}

// unlessStopping returns err, or ErrShutDown once shutdown has begun.
func (in *Instance) unlessStopping(err error) error {
	if in.stopping.Err() != nil {
		return ErrShutDown
	}
	return err
}

// start is Start, without regard for how a shutdown changes its result. It
// returns ErrShutDown at the first check after shutdown begins.
func (in *Instance) start(ctx context.Context) error {
	// A Service may keep ctx for background work, so returning does not
	// cancel it: only shutdown beginning, or the caller, does.
	ctx, cancel := context.WithCancel(ctx)
	context.AfterFunc(in.stopping, cancel)
	// The gateway listens first: every leaf the Instance CA issues names
	// the CRL on its port (ADR 0046), so the CA learns it before anything
	// can issue one. Connections wait until Serve.
	l, err := in.Env.ListenBind(in.Config.Port("gateway"), true)
	if err != nil {
		return fmt.Errorf("gateway listen %s: %w", net.JoinHostPort(in.Config.Bind, strconv.Itoa(in.Config.Port("gateway"))), err)
	}
	in.Env.Endpoints.Set("gateway", l.Addr().String())
	in.Env.CA.SetCRLURL(ca.CRLURL(emu.Dialable(l.Addr().String())))
	for _, s := range in.services {
		if err := in.gw.Register(s); err != nil {
			in.setFailed(s.Name(), err)
			in.Env.Log.Error("service registration failed", "service", s.Name(), "err", err)
		}
	}
	if err := in.delegate(); err != nil {
		_ = l.Close()
		return err
	}
	in.startManaged()
	in.gw.HandleAdmin(newAdmin(in))
	in.mountConsole()

	go func() {
		if err := in.gw.Serve(l); err != nil {
			in.Env.Log.Error("gateway stopped", "err", err)
		}
	}()
	if err := in.fe.Start(); err != nil {
		return err
	}

	if in.stopping.Err() != nil {
		return ErrShutDown
	}
	in.removeLeftoverRegistryRelay(ctx)
	for _, s := range in.services {
		if in.stopping.Err() != nil {
			return ErrShutDown
		}
		if in.failure(s.Name()) != nil {
			continue
		}
		if err := s.Start(ctx); err != nil {
			in.setFailed(s.Name(), err)
			in.Env.Log.Error("service failed to start", "service", s.Name(), "err", err)
		}
	}
	for _, step := range []func(){
		in.startGFELAN,
		in.startLink,
		in.startRegistryRelay,
		func() { in.warnContainerBind(ctx) },
		func() { warnLoopbackNetmask(in.Env.Log, goruntime.GOOS, loopback.Lo0Aliases) },
		func() { in.warnContainerized(ctx) },
	} {
		if in.stopping.Err() != nil {
			return ErrShutDown
		}
		step()
	}
	if in.stopping.Err() != nil {
		return ErrShutDown
	}
	if err := in.writeRuntimeFiles(); err != nil {
		return err
	}
	if in.stopping.Err() != nil {
		return ErrShutDown
	}
	in.startHostNames()
	if in.stopping.Err() != nil {
		return ErrShutDown
	}
	in.Env.Log.Info("gcpemu started", "instance", in.Config.Instance, "id", in.ID, "gateway", in.Env.Endpoints.Get("gateway"), "dir", in.Dir)
	return nil
}

// warnContainerBind warns, in a Containerized Instance on a bridge
// network, that the --*-bind addresses set reach only container networks
// (#497, ADR 0011).
func (in *Instance) warnContainerBind(ctx context.Context) {
	var flags []string
	for _, b := range in.Config.LANBinds() {
		if len(b.Addrs) > 0 {
			flags = append(flags, b.Flag)
		}
	}
	if len(flags) > 0 && in.Env.LANBind(ctx) == lanbind.Bridge {
		in.Env.Log.Warn(lanbind.Warning(flags))
	}
}

// warnLoopbackNetmask warns, on macOS, when aliases finds a loopback
// alias that is not a /32, as earlier versions of `gcpemu loopback
// install` added (#677). It never fails the start.
func warnLoopbackNetmask(log *slog.Logger, goos string, aliases func() loopback.AliasState) {
	if goos == "darwin" && aliases() == loopback.AliasesWide {
		log.Warn("loopback aliases are not all /32; "+loopback.WideHint, "fix", loopback.InstallHint)
	}
}

// warnContainerized logs, under --containerized=auto in a container with
// no runtime socket, that the Instance assumes it is a Containerized
// Instance (#502). It asks in the background, so Start doesn't wait.
func (in *Instance) warnContainerized(ctx context.Context) {
	if s := strings.ToLower(in.Config.Containerized); s != "auto" && s != "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), emu.ContainerizedWait)
		defer cancel()
		in.Env.Containerized(ctx)
	}()
}

// mountConsole serves the Web console on the gateway unless it is turned
// off (--console=false, which the CLI implies under CI=true; FR-CI-003).
func (in *Instance) mountConsole() {
	if !in.Config.ConsoleEnabled() {
		return
	}
	if !console.Built {
		in.Env.Log.Warn("this binary was built without the Web console; /console shows a placeholder (build with `task build` and pnpm on PATH)")
	}
	in.gw.HandleConsole(webconsole.Handler(console.FS))
	in.consoleServed = true
}

// setupAdminToken guards the Admin API and the Web console with an Admin
// token when the gateway is bound beyond loopback (FR-UI-009, ADR 0006):
// GCPEMU_ADMIN_TOKEN, or a random one, written 0600 to the instance dir.
// On loopback it removes any token file a previous run left behind.
func (in *Instance) setupAdminToken() error {
	path := filepath.Join(in.Dir, AdminTokenFile)
	if config.IsLoopback(in.Config.Bind) {
		_ = os.Remove(path)
		return nil
	}
	tok := in.Config.AdminToken
	if tok == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return fmt.Errorf("admin token: %w", err)
		}
		tok = base64.RawURLEncoding.EncodeToString(b)
	}
	if err := writeAtomic(path, []byte(tok)); err != nil {
		return fmt.Errorf("admin token: %w", err)
	}
	in.adminToken = tok
	in.gw.SetAdminToken(tok)
	return nil
}

// AdminToken returns the Admin token, or "" when the gateway is bound to
// loopback and none is needed (FR-UI-009).
func (in *Instance) AdminToken() string { return in.adminToken }

// ConsoleServed reports whether the gateway serves the Web console.
func (in *Instance) ConsoleServed() bool { return in.consoleServed }

// writeRuntimeFiles writes the PID file, then endpoints.json, which is what
// tells clients the Instance is up: `gcpemu stop` waits for the process
// that the PID file names, so that file has to be there first (#990).
func (in *Instance) writeRuntimeFiles() error {
	if err := writeAtomic(filepath.Join(in.Dir, PIDFile), []byte(strconv.Itoa(os.Getpid()))); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(in.Env.Endpoints.All(), "", "  ")
	return writeAtomic(filepath.Join(in.Dir, EndpointsFile), b)
}

func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// hasService reports whether the Instance runs the Service named name.
func (in *Instance) hasService(name string) bool {
	for _, s := range in.services {
		if s.Name() == name {
			return true
		}
	}
	return false
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

// HoldReady keeps the instance not ready, with name and reason reported
// by Readiness, until release is called. The CLI holds it while the
// startup seed is applied, so that "ready" means seeded (FR-CI-001).
func (in *Instance) HoldReady(name, reason string) (release func()) {
	in.failedMu.Lock()
	in.holds[name] = errors.New(reason)
	in.failedMu.Unlock()
	return func() {
		in.failedMu.Lock()
		delete(in.holds, name)
		in.failedMu.Unlock()
	}
}

// Readiness reports each service's readiness, and any readiness holds:
// nil error means ready.
func (in *Instance) Readiness() map[string]error {
	out := map[string]error{}
	in.failedMu.RLock()
	for name, err := range in.holds {
		out[name] = err
	}
	in.failedMu.RUnlock()
	for _, s := range in.services {
		if err := in.failure(s.Name()); err != nil {
			out[s.Name()] = err
			continue
		}
		out[s.Name()] = s.Ready()
	}
	for api, m := range in.managed {
		out["managed-emulator/"+api] = m.readiness()
	}
	return out
}

// Operations returns the Operations of every service, or of service and
// project when they are set, newest first.
func (in *Instance) Operations(service, project string) []emu.OperationInfo {
	out := []emu.OperationInfo{}
	for _, s := range in.services {
		l, ok := s.(emu.OperationLister)
		if !ok || (service != "" && s.Name() != service) {
			continue
		}
		for _, op := range l.Operations() {
			if project != "" && op.Project != project {
				continue
			}
			op.Service = s.Name()
			out = append(out, op)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := out[i].StartTime, out[j].StartTime; !a.Equal(b) {
			return a.After(b)
		}
		return out[i].Name > out[j].Name
	})
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

// WaitReady blocks until all services are ready or ctx ends. It returns
// ErrShutDown once shutdown has begun, as the Instance can no longer become
// ready, whatever its services report and however ctx ended.
func (in *Instance) WaitReady(ctx context.Context) error {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		if in.stopping.Err() != nil {
			return ErrShutDown
		}
		if in.Ready() {
			return nil
		}
		select {
		case <-in.stopping.Done():
			return ErrShutDown
		case <-ctx.Done():
			var names []string
			for n, err := range in.Readiness() {
				if err != nil {
					names = append(names, fmt.Sprintf("%s: %v", n, err))
				}
			}
			sort.Strings(names)
			return in.unlessStopping(fmt.Errorf("services not ready: %v", names))
		case <-t.C:
		}
	}
}

// Reset wipes all state but keeps configuration (FR-CORE-002).
func (in *Instance) Reset(ctx context.Context) error {
	in.stateMu.Lock()
	defer in.stateMu.Unlock()
	return in.gated(ctx, "reset", "reset in progress", func() error { return in.reset(ctx) })
}

// gated runs change, a Reset or State snapshot restore, with the Instance
// not ready (hold, with reason) and the API gateway's gate closed: every request
// but the Admin API's is refused as UNAVAILABLE, and change starts only
// once those in progress, cut off, have returned (#302). Callers hold
// stateMu.
func (in *Instance) gated(ctx context.Context, hold, reason string, change func() error) error {
	release := in.HoldReady(hold, reason)
	defer release()
	defer in.gw.OpenGate()
	if err := in.gw.CloseGate(ctx); err != nil {
		return fmt.Errorf("cutting off requests in progress: %w", err)
	}
	return change()
}

func (in *Instance) reset(ctx context.Context) error {
	var errs []error
	for _, s := range in.services {
		if r, ok := s.(emu.Resetter); ok {
			errs = append(errs, r.Reset(ctx))
		}
	}
	errs = append(errs, in.containers.reset(ctx))
	in.startRegistryRelay()
	errs = append(errs, in.resetManaged())
	errs = append(errs, in.Env.Store.Reset())
	for _, s := range in.services {
		if f, ok := s.(emu.ResetFinisher); ok {
			f.ResetDone()
		}
	}
	in.Log.Clear()
	if a, ok := in.Env.Auth.(*emu.PolicyAuthorizer); ok {
		a.ClearDenials()
	}
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

// Shutdown begins shutdown at once, waits for a Start in progress to return,
// then stops services in reverse order, closes the store and removes runtime
// files (FR-CORE-006). ctx also limits that wait: when ctx ends first, it
// logs a warning and carries on with Start still running (ADR 0065).
func (in *Instance) Shutdown(ctx context.Context) error { return in.shutdown(ctx, false) }

// shutdown is Shutdown. When logErrs, it also logs each error, such as a
// container it could not remove, before Done closes: a shutdown the admin
// API requested has no caller to return them to, and the process may exit
// as soon as Done closes (#920).
func (in *Instance) shutdown(ctx context.Context, logErrs bool) error {
	var errs []error
	in.shutdownOnce.Do(func() {
		in.waitForStart(ctx, in.beginShutdown())
		for i := len(in.services) - 1; i >= 0; i-- {
			s := in.services[i]
			if err := s.Stop(ctx); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", s.Name(), err))
			}
		}
		in.hostManager().Stop()
		in.fe.StopLAN()
		errs = append(errs, in.link.Close())
		in.stopManaged(ctx)
		in.registryRelay.stop()
		errs = append(errs, in.containers.shutdown())
		errs = append(errs, in.fe.Close())
		in.Events.Close() // end event streams so the gateway can drain
		errs = append(errs, in.gw.Shutdown(ctx))
		errs = append(errs, in.Env.Store.Close())
		_ = os.Remove(filepath.Join(in.Dir, PIDFile))
		_ = os.Remove(filepath.Join(in.Dir, EndpointsFile))
		if in.adminToken != "" {
			_ = os.Remove(filepath.Join(in.Dir, AdminTokenFile))
		}
		if in.tmpDir != "" {
			_ = os.RemoveAll(in.tmpDir)
		}
		if logErrs {
			for _, err := range leafErrors(errors.Join(errs...)) {
				in.Env.Log.Warn("cleaning up on shutdown", "err", err)
			}
		}
		close(in.done)
	})
	return errors.Join(errs...)
}

// beginShutdown marks shutdown as begun and returns what closes when Start
// returns, nil if it was never called. From here on Start does nothing.
func (in *Instance) beginShutdown() <-chan struct{} {
	in.startMu.Lock()
	defer in.startMu.Unlock()
	in.beginStop()
	return in.startReturned
}

// waitForStart waits until returned closes, if it is not nil, or ctx ends.
func (in *Instance) waitForStart(ctx context.Context, returned <-chan struct{}) {
	if returned == nil {
		return
	}
	select {
	case <-returned:
	case <-ctx.Done():
		select {
		case <-returned:
		default:
			in.Env.Log.Warn("shutting down while Start is still running: its Services may be stopped while they start")
		}
	}
}

// release undoes what a failed New took before it failed: it closes st,
// so the data directory is unlocked, and removes the temp dir and the
// Admin token file New wrote. The data directory and its CA stay.
func (in *Instance) release(st store.Store) error {
	var errs []error
	if in.managedCancel != nil {
		in.managedCancel()
	}
	if in.fe != nil {
		errs = append(errs, in.fe.Close())
	}
	if st != nil {
		errs = append(errs, st.Close())
	}
	if in.adminToken != "" {
		if err := os.Remove(filepath.Join(in.Dir, AdminTokenFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if in.tmpDir != "" {
		errs = append(errs, os.RemoveAll(in.tmpDir))
	}
	return errors.Join(errs...)
}

// RequestShutdown begins shutdown at once and does the rest of Shutdown
// asynchronously (used by the admin API).
func (in *Instance) RequestShutdown() {
	in.beginShutdown()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = in.shutdown(ctx, true)
	}()
}

// leafErrors flattens err, and every multi-error (Unwrap() []error) in it,
// into the errors they hold.
func leafErrors(err error) []error {
	if err == nil {
		return nil
	}
	j, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return []error{err}
	}
	var out []error
	for _, e := range j.Unwrap() {
		out = append(out, leafErrors(e)...)
	}
	return out
}
