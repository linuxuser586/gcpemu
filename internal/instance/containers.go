package instance

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linuxuser586/gcpemu/internal/agent"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/netplane"
	"github.com/linuxuser586/gcpemu/internal/registryrelay"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// containers lazily connects to the container runtime the first time a
// service needs it, reclaiming orphans of a previous run (FR-CORE-006).
type containers struct {
	in *Instance

	mu    sync.Mutex
	rt    *runtime.Manager
	plane *netplane.Plane
	err   error
	// conn is the connection in progress; nil while none runs.
	conn *connection
	// connected is rt, for the runtime probe, which must never wait for mu
	// (#1025).
	connected atomic.Pointer[runtime.Manager]

	statusMu sync.Mutex
	status   RuntimeStatus
	statusAt time.Time
	// statusProbe closes when the running probe ends; nil while none runs.
	statusProbe chan struct{}

	disruptedMu  sync.Mutex
	disruptedErr error
	disruptedAt  time.Time
}

// connection is one attempt to connect to the runtime, which every caller
// that comes while it runs waits for.
type connection struct {
	done chan struct{} // closes when it ends, after rt and err are set
	rt   *runtime.Manager
	err  error
}

// reclaimLimit limits the Reclaim of a connection: several times the slowest
// measured, and short enough that a runtime that hangs during one leaves the
// Instance not ready for no longer (ADR 0068).
const reclaimLimit = 2 * time.Minute

// reclaimTime is reclaimLimit, a variable for tests.
var reclaimTime = reclaimLimit

// infoTime limits asking a runtime that detection found for its Info, which
// no caller's context limits any more (#1033): a runtime that hangs there
// would otherwise hold the connection until shutdown. A connection that
// reaches it ends with a transient error, and the next caller connects
// again. Info answers as fast as detection's 3s ping; 10s leaves room.
const infoTime = 10 * time.Second

// Connected implements emu.ConnectedRuntime.
func (c *containers) Connected() *runtime.Manager {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rt
}

// Runtime implements emu.Containers. The connection is the Instance's and not
// the caller's (ADR 0068): one runs at a time, on a context only shutdown ends,
// so a runtime that hangs while connecting never holds Teardown (#1028). ctx
// limits only how long the caller waits for it; a caller that stops waiting
// gets a transient error, and the connection goes on. Once shutdown has begun
// no connection starts.
func (c *containers) Runtime(ctx context.Context) (*runtime.Manager, error) {
	c.mu.Lock()
	if c.rt != nil || c.err != nil {
		defer c.mu.Unlock()
		return c.rt, c.err
	}
	if c.in.stopping.Err() != nil {
		c.mu.Unlock()
		return nil, ErrShutDown
	}
	conn := c.conn
	if conn == nil {
		conn = &connection{done: make(chan struct{})}
		c.conn = conn
		go c.connect(conn)
	}
	c.mu.Unlock()
	select {
	case <-conn.done:
		return conn.rt, conn.err
	case <-ctx.Done():
		select { // a connection that has just ended is the answer
		case <-conn.done:
			return conn.rt, conn.err
		default:
		}
		return nil, fmt.Errorf("the container runtime connection is still in progress: %w", ctx.Err())
	}
}

// connect runs conn and records what it ends with. A connection shutdown cuts
// short leaves nothing behind: no error to repeat, and no runtime for Teardown
// to clean up on.
func (c *containers) connect(conn *connection) {
	rt, err := c.dial(c.in.stopping)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.in.stopping.Err() != nil {
		rt, err = nil, ErrShutDown
	}
	switch {
	case rt != nil:
		c.rt = rt
		c.connected.Store(rt)
	case errors.Is(err, emu.ErrNoRuntime):
		c.err = err
	}
	conn.rt, conn.err = rt, err
	c.conn = nil
	close(conn.done)
}

// dial detects the runtime, reclaims the Instance's orphans and tells
// services the Self addresses, on ctx, which shutdown ends.
func (c *containers) dial(ctx context.Context) (*runtime.Manager, error) {
	cl, err := runtime.DetectContext(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ErrShutDown
		}
		return nil, emu.NoRuntime(err)
	}
	ictx, cancel := context.WithTimeout(ctx, infoTime)
	info, err := cl.Info(ictx)
	cancel()
	if err != nil {
		return nil, err // transient; retry next time
	}
	dir, err := filepath.Abs(c.in.Dir)
	if err != nil {
		dir = c.in.Dir
	}
	m := &runtime.Manager{
		Client: cl, Info: info, InstanceID: c.in.ID, Instance: c.in.Config.Instance, Dir: dir,
		Offline: c.in.Config.Offline, Ephemeral: c.in.Config.Ephemeral,
		Log: c.in.Env.Log.With("component", "runtime"), Reserved: c.reservedAddrs,
		Agent: &agent.Resolver{
			Arch: info.Arch, Version: Version, CacheDir: filepath.Join(c.in.Dir, "agent"),
			Offline: c.in.Config.Offline, Images: cl,
		},
	}
	cl.SetMemberAddr(c.relayAddr)
	cl.SetReserved(c.reservedAddrs)
	m.VM = runtime.DecideVM(c.in.Config.VMRuntime, cl.Self() != "", info)
	m.LoopbackAliases = runtime.DecideLoopbackAliases(c.in.Config.LoopbackAliases, info)
	// A Reclaim runs once and is not tried again in this run: it removes every
	// container the Instance ID tags, the run's own too once connected.
	rctx, cancel := context.WithTimeout(ctx, reclaimTime)
	err = m.Reclaim(rctx)
	cancel()
	if ctx.Err() != nil {
		return nil, ErrShutDown
	}
	if err != nil {
		m.Log.Warn("reclaiming orphaned containers", "err", err)
	}
	c.selfJoined(ctx, m)
	if ctx.Err() != nil {
		return nil, ErrShutDown
	}
	m.Log.Info("container runtime connected", "runtime", info.Name, "version", info.Version, "endpoint", cl.Endpoint, "selfContainer", cl.Self())
	if cl.Self() != "" {
		m.Log.Info("VM runtime " + m.VM.String())
		if m.VM.On {
			m.Log.Info("loopback aliases " + m.LoopbackAliases.String())
		}
	}
	return m, nil
}

// selfJoined tells services the Self address on each network the
// Instance's container has joined (emu.SelfJoiner); on the host there is
// none. It runs within the connection and SelfJoined writes the store, so
// neither SelfJoined nor a store transaction may wait for Runtime.
func (c *containers) selfJoined(ctx context.Context, m *runtime.Manager) {
	addrs := map[string]string{}
	if m.Self() != "" {
		ct, err := m.InspectContainer(ctx, m.Self())
		if err != nil {
			m.Log.Warn("inspecting the Instance's container for its Self addresses", "err", err)
			return
		}
		addrs = ct.IPs
	}
	for _, s := range c.in.services {
		if j, ok := s.(emu.SelfJoiner); ok {
			j.SelfJoined(addrs)
		}
	}
}

// reservedAddrs collects the addresses services' stored resources have
// recorded (emu.AddressReserver).
func (c *containers) reservedAddrs() []string {
	var out []string
	for _, s := range c.in.services {
		if r, ok := s.(emu.AddressReserver); ok {
			out = append(out, r.ReservedAddrs()...)
		}
	}
	return out
}

// relayAddr is the address a service pins for the VM relay on network
// (emu.RelayAddresser).
func (c *containers) relayAddr(network string) string {
	for _, s := range c.in.services {
		if r, ok := s.(emu.RelayAddresser); ok {
			if a := r.RelayAddr(network); a != "" {
				return a
			}
		}
	}
	return ""
}

func (c *containers) Netplane(ctx context.Context) (*netplane.Plane, error) {
	rt, err := c.Runtime(ctx)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.plane == nil {
		c.plane = netplane.New(rt, c.in.Env.Endpoints.All, c.in.Config.Bind, c.in.Env.Log.With("component", "netplane"))
	}
	return c.plane, nil
}

// DialContext implements emu.Containers.
func (c *containers) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	c.mu.Lock()
	plane := c.plane
	c.mu.Unlock()
	if plane != nil {
		return plane.DialContext(ctx, network, addr)
	}
	d := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return d.DialContext(ctx, network, addr)
}

// RelayIP implements emu.Containers.
func (c *containers) RelayIP() string {
	c.mu.Lock()
	plane := c.plane
	c.mu.Unlock()
	if plane == nil {
		return ""
	}
	if r := plane.Relay(); r != nil {
		return r.IP()
	}
	return ""
}

// reset removes the Registry relay and the VM relay (Instance.Reset); the
// Instance starts the Registry relay again, and the VM relay starts when
// next needed.
func (c *containers) reset(ctx context.Context) error {
	c.mu.Lock()
	rt, plane := c.rt, c.plane
	c.mu.Unlock()
	var errs []error
	if rt != nil {
		errs = append(errs, registryrelay.Remove(ctx, rt))
	}
	if plane != nil {
		errs = append(errs, plane.Reset(ctx))
	}
	return errors.Join(errs...)
}

// shutdown removes the instance's containers and probe networks (and, for
// ephemeral instances, its other networks and volumes) after services have
// stopped. Removing the VM relay and the runtime's Cleanup share one limit
// of their own, not derived from the shutdown's ctx: cleanupTime, ending
// no later than cleanupBy unless it is zero (ADR 0078). If Cleanup could
// not finish, it warns once that containers may be left for Reclaim.
func (c *containers) shutdown(cleanupBy time.Time) error {
	c.statusMu.Lock()
	probe := c.statusProbe
	c.statusMu.Unlock()
	if probe != nil { // in.stopping has cancelled it, and no other starts
		<-probe
	}
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn != nil { // in.stopping ends it, leaving nothing
		<-conn.done
	}
	c.mu.Lock()
	rt, plane := c.rt, c.plane
	c.mu.Unlock()
	deadline := time.Now().Add(cleanupTime)
	if !cleanupBy.IsZero() && cleanupBy.Before(deadline) {
		deadline = cleanupBy
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	if plane != nil {
		plane.Close(ctx)
	}
	if rt == nil {
		return nil
	}
	err := rt.Cleanup(ctx, c.in.Config.Ephemeral)
	if err != nil {
		c.in.Env.Log.Warn("shutting down: could not confirm its containers were removed; they may still be running, and the next start with this Instance ID tries to remove them (Reclaim)")
	}
	return err
}

// ContainerInfo describes one container the instance owns (admin API,
// `gcpemu status`).
type ContainerInfo struct {
	Name     string `json:"name"`
	Service  string `json:"service,omitempty"`
	Resource string `json:"resource,omitempty"`
	Role     string `json:"role,omitempty"`
	Image    string `json:"image"`
	State    string `json:"state"`
}

// list returns the instance's labelled containers, sorted by name. It does
// not connect to the runtime if no service has used it yet (nothing can
// be running then).
func (c *containers) list(ctx context.Context) ([]ContainerInfo, error) {
	c.mu.Lock()
	rt := c.rt
	c.mu.Unlock()
	out := []ContainerInfo{}
	if rt == nil {
		return out, nil
	}
	cts, err := rt.ListContainers(ctx, map[string]string{runtime.LabelInstance: rt.InstanceID})
	if err != nil {
		return nil, err
	}
	for _, ct := range cts {
		out = append(out, ContainerInfo{Name: ct.Name, Service: ct.Labels[runtime.LabelService],
			Resource: ct.Labels[runtime.LabelResource], Role: ct.Labels[runtime.LabelRole], Image: ct.Image, State: ct.Status})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// disruptedTTL bounds how often /_emu/v1/ready lists the containers;
// health checks poll it.
const disruptedTTL = 2 * time.Second

// disrupted reports the instance's containers that the runtime stopped or
// started again behind its back, e.g. in a daemon restart (#318), so
// that /_emu/v1/ready fails and names them. It does not connect to the
// runtime if the instance has not. The answer is cached for every caller,
// so it is not cut short by one caller's request going away.
func (c *containers) disrupted(ctx context.Context) error {
	c.disruptedMu.Lock()
	defer c.disruptedMu.Unlock()
	if !c.disruptedAt.IsZero() && time.Since(c.disruptedAt) < disruptedTTL {
		return c.disruptedErr
	}
	c.mu.Lock()
	rt := c.rt
	c.mu.Unlock()
	if rt == nil {
		return nil
	}
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var err error
	if cts, lerr := rt.ListContainers(lctx, map[string]string{runtime.LabelInstance: rt.InstanceID}); lerr != nil {
		err = fmt.Errorf("listing containers: %w", lerr)
	} else if names := rt.Disrupted(lctx, cts); len(names) > 0 {
		err = fmt.Errorf("%s; stop and start the instance to bring them back", strings.Join(names, ", "))
	}
	c.disruptedErr, c.disruptedAt = err, time.Now()
	return err
}

// Containers lists the instance's containers (see containers.list).
func (in *Instance) Containers(ctx context.Context) ([]ContainerInfo, error) {
	return in.containers.list(ctx)
}

// RuntimeStatus describes the container runtime (admin API /info).
type RuntimeStatus struct {
	Kind      string `json:"kind"` // docker, podman, none or unknown
	Version   string `json:"version,omitempty"`
	Reachable bool   `json:"reachable"`
	Error     string `json:"error,omitempty"`
}

const (
	// runtimeUnknown is the Kind before the first probe has finished.
	runtimeUnknown = "unknown"
	// runtimeNone is the Kind when a probe found no usable runtime.
	runtimeNone = "none"
)

// runtimeStatusTTL bounds how often RuntimeStatus probes the runtime;
// the Web console dashboard polls /info (a variable for tests).
var runtimeStatusTTL = 5 * time.Second

// runtimeStatusWait is how long RuntimeStatus waits for a probe. A healthy
// runtime answers well within it, so a caller that asks once is told the
// truth; a slow one must not hold up /info, which every CLI command asks
// first (ADR 0066, #1025).
const runtimeStatusWait = time.Second

// RuntimeStatus reports the container runtime the instance uses, or would
// use. It starts a probe if its last answer is old, waits at most
// runtimeStatusWait for it, and returns the latest answer: the runtime
// cannot hold it up, and the probe is every caller's, so one going away
// does not cut it short.
func (in *Instance) RuntimeStatus(ctx context.Context) RuntimeStatus {
	c := in.containers
	c.statusMu.Lock()
	stale := c.statusAt.IsZero() || time.Since(c.statusAt) >= runtimeStatusTTL
	if stale && c.statusProbe == nil && in.stopping.Err() == nil {
		probe := make(chan struct{})
		c.statusProbe = probe
		go func() {
			defer close(probe)
			st := c.probeStatus(in.stopping)
			c.statusMu.Lock()
			defer c.statusMu.Unlock()
			c.statusProbe = nil
			// A probe shutdown cut short says nothing about the runtime.
			if in.stopping.Err() == nil {
				c.status, c.statusAt = st, time.Now()
			}
		}()
	}
	probe := c.statusProbe
	c.statusMu.Unlock()
	if stale && probe != nil {
		wait := time.NewTimer(runtimeStatusWait)
		defer wait.Stop()
		select {
		case <-probe:
		case <-wait.C:
		case <-ctx.Done():
		}
	}
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	return c.status
}

// probeStatus asks the runtime how it is, without connecting: no orphans
// are reclaimed until a service needs the runtime.
func (c *containers) probeStatus(ctx context.Context) RuntimeStatus {
	if rt := c.connected.Load(); rt != nil {
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		info, err := rt.Client.Info(pctx)
		st := RuntimeStatus{Kind: rt.Info.Name, Version: rt.Info.Version, Reachable: err == nil}
		if err != nil {
			st.Error = err.Error()
		} else {
			st.Version = info.Version
		}
		return st
	}
	cl, err := runtime.DetectContext(ctx)
	if err != nil {
		return RuntimeStatus{Kind: runtimeNone, Error: err.Error()}
	}
	pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	info, err := cl.Info(pctx)
	if err != nil {
		return RuntimeStatus{Kind: runtimeNone, Error: err.Error()}
	}
	return RuntimeStatus{Kind: info.Name, Version: info.Version, Reachable: true}
}
