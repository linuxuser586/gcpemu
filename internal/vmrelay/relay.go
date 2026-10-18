// Package vmrelay implements the VM relay (ADR 0005). On a VM runtime
// (Docker Desktop, Colima, OrbStack, Podman machine) the container
// networks live inside a virtual machine: the host can neither route to
// container IPs nor bind the services network's gateway. The relay is a
// Managed container joined to every network the Instance uses, which
// publishes one control port on the host's 127.0.0.1. The emulator keeps
// one yamux session on it, over which it dials container addresses, and
// over which the relay hands it the connections and datagrams that
// Managed containers send to the emulator endpoints it listens for.
package vmrelay

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/linuxuser586/gcpemu/internal/agent"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// Image runs the relay agent (any image works: the agent is a static
// binary).
const Image = "busybox:1.37"

// Env forces the relay on ("1") or off ("0").
const Env = "GCPEMU_VM_RELAY"

// Decision says whether an Instance uses the relay, and why.
type Decision struct {
	On bool
	// VM: the gateway cannot be bound, so the runtime runs in a VM, whose
	// host network is not the host's.
	VM     bool
	Reason string
}

func (d Decision) String() string {
	if d.On {
		return "on (" + d.Reason + ")"
	}
	return "off (" + d.Reason + ")"
}

// Decide chooses: never when the emulator runs in a container (ADR 0003),
// since it is already on every network; otherwise as GCPEMU_VM_RELAY says;
// otherwise when the services network's gateway cannot be bound on the
// host, which is how a VM runtime shows.
func Decide(inContainer bool, gateway string) Decision {
	if inContainer {
		return Decision{Reason: "the emulator runs in a container"}
	}
	vm := true
	if l, err := net.Listen("tcp", net.JoinHostPort(gateway, "0")); err == nil {
		l.Close()
		vm = false
	}
	switch strings.ToLower(os.Getenv(Env)) {
	case "1", "true", "on":
		return Decision{On: true, VM: vm, Reason: "forced by " + Env}
	case "0", "false", "off":
		return Decision{VM: vm, Reason: "disabled by " + Env}
	}
	if vm {
		return Decision{On: true, VM: true, Reason: "gateway " + gateway + " not bindable"}
	}
	return Decision{Reason: "gateway " + gateway + " bindable"}
}

// Options configure Start.
type Options struct {
	// Services and External are the Instance's services and external
	// networks. The relay publishes its control port through External,
	// since a runtime publishes no ports of a container that is only on
	// internal networks.
	Services, External string
	// Target resolves an endpoint name to the host address it is served
	// at, for connections the relay hands in.
	Target func(name string) string
	Log    *slog.Logger
}

// Relay is the emulator's side of a running relay.
type Relay struct {
	rt     *runtime.Manager
	log    *slog.Logger
	target func(string) string
	id     string
	ip     string // on the services network
	ctrl   string // the control port on the host

	mu      sync.Mutex
	sess    *yamux.Session
	nets    map[string]netip.Prefix // joined network → subnet
	closed  bool
	streams map[string]int64
	// ready is set once Start has reached the agent; only then do
	// dropped sessions reconnect.
	ready bool
	// reconnecting is set while a reconnect loop runs.
	reconnecting bool
}

// Start creates the relay container, joins it to every network the
// Instance has and creates, and connects to it.
func Start(ctx context.Context, rt *runtime.Manager, o Options) (*Relay, error) {
	if err := rt.EnsureImage(ctx, Image, rt.Offline); err != nil {
		return nil, fmt.Errorf("VM relay image %s: %w", Image, err)
	}
	bin, err := rt.AgentBinary(ctx)
	if err != nil {
		return nil, err
	}
	name := rt.Name("vmrelay")
	if err := rt.RemoveContainer(ctx, name, true); err != nil {
		return nil, err
	}
	id, err := rt.CreateContainer(ctx, runtime.ContainerSpec{
		Name:       name,
		Image:      Image,
		Entrypoint: []string{agent.ContainerPath},
		Env:        []string{agent.EnvVar + "=" + agentName},
		Labels:     rt.Labels("core", "vmrelay", "relay"),
		Hostname:   "gcpemu-vmrelay",
		Networks:   []runtime.Attachment{{Network: o.External}},
		Ports:      []runtime.Port{{HostIP: "127.0.0.1", ContainerPort: ControlPort}},
		Init:       true,
	})
	if err != nil {
		return nil, fmt.Errorf("VM relay: %w", err)
	}
	r := &Relay{rt: rt, log: o.Log, target: o.Target, id: id, nets: map[string]netip.Prefix{}, streams: map[string]int64{}}
	fail := func(err error) (*Relay, error) {
		rt.SetMember("", nil)
		_ = rt.RemoveContainer(context.Background(), id, true)
		return nil, fmt.Errorf("VM relay: %w", err)
	}
	// Become a member before listing, so no network created meanwhile is
	// missed; joining one twice is harmless.
	rt.SetMember(id, r.joined)
	nets, err := rt.ListNetworks(ctx, map[string]string{runtime.LabelInstance: rt.InstanceID})
	if err != nil {
		return fail(err)
	}
	// On the services network the relay takes the reserved address, so a
	// relay started again after Reset listens on the same addresses.
	if err := rt.AttachMember(ctx, o.Services, true); err != nil {
		return fail(fmt.Errorf("join %s: %w", o.Services, err))
	}
	for _, n := range nets {
		if err := rt.AttachMember(ctx, n.Name, false); err != nil {
			return fail(fmt.Errorf("join %s: %w", n.Name, err))
		}
	}
	if err := rt.CopyTo(ctx, id, "/", []runtime.File{{Name: strings.TrimPrefix(agent.ContainerPath, "/"), Mode: 0o755, Path: bin}}); err != nil {
		return fail(err)
	}
	if err := rt.StartContainer(ctx, id); err != nil {
		return fail(err)
	}
	if err := rt.WaitRunning(ctx, id); err != nil {
		return fail(err)
	}
	ct, err := rt.InspectContainer(ctx, id)
	if err != nil {
		return fail(err)
	}
	r.ip = ct.IPs[o.Services]
	r.ctrl = ct.Ports[strconv.Itoa(ControlPort)+"/tcp"]
	if r.ip == "" || r.ctrl == "" {
		return fail(fmt.Errorf("no address on %s or no published control port (%v)", o.Services, ct.Ports))
	}
	if err := r.waitReady(ctx, 20*time.Second); err != nil {
		logs, _ := rt.Logs(context.Background(), id, 20)
		return fail(fmt.Errorf("control port %s did not answer: %w\n%s", r.ctrl, err, logs))
	}
	r.log.Info("VM relay started", "container", name, "control", r.ctrl, "ip", r.ip)
	return r, nil
}

// joined tracks the subnets the relay can reach (runtime.SetMember).
func (r *Relay) joined(n runtime.Network, in bool) {
	p, err := netip.ParsePrefix(n.Subnet)
	r.mu.Lock()
	defer r.mu.Unlock()
	if !in {
		delete(r.nets, n.Name)
	} else if err == nil {
		r.nets[n.Name] = p.Masked()
	}
}

// waitReady pings the agent until it answers. A published port may
// accept connections before the agent listens behind it.
func (r *Relay) waitReady(ctx context.Context, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		err := r.ping(ctx)
		if err == nil {
			r.mu.Lock()
			r.ready = true
			r.mu.Unlock()
			return nil
		}
		r.dropSession()
		select {
		case <-ctx.Done():
			return err
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (r *Relay) ping(ctx context.Context) error {
	c, err := r.open(ctx, "ping")
	if err != nil {
		return err
	}
	c.Close()
	return nil
}

// IP is the relay's address on the services network.
func (r *Relay) IP() string { return r.ip }

// ID is the relay container's ID.
func (r *Relay) ID() string { return r.id }

// Covers reports whether ip lies on a network the relay has joined.
func (r *Relay) Covers(ip string) bool {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	a = a.Unmap()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.nets {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// DialContext connects to addr on a container network through the relay.
func (r *Relay) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	c, err := r.open(ctx, "dial", network, addr)
	if err != nil {
		return nil, &net.OpError{Op: "dial", Net: network, Err: err}
	}
	r.count(addr)
	return c, nil
}

// Listen has the relay listen at addr (its own address on a network,
// or another port there when that one is taken) for the named emulator
// endpoint, with UDP as well when withUDP. It returns the address bound,
// and is idempotent per name.
func (r *Relay) Listen(ctx context.Context, name, addr string, withUDP bool) (string, error) {
	proto := "tcp"
	if withUDP {
		proto = "udp"
	}
	c, err := r.open(ctx, "listen", name, addr, proto)
	if err != nil {
		return "", err
	}
	defer c.Close()
	return c.reply, nil
}

// Offer has the relay listen on exactly addr, over UDP when udp and TCP
// otherwise, for the named emulator endpoint: an Offered port (ADR 0014).
// Unlike Listen it fails when the port is taken. It is idempotent per
// name.
func (r *Relay) Offer(ctx context.Context, name, addr string, udp bool) error {
	proto := "tcp"
	if udp {
		proto = "udp"
	}
	c, err := r.open(ctx, "offer", name, addr, proto)
	if err != nil {
		return err
	}
	c.Close()
	return nil
}

// Withdraw has the relay stop listening for the named endpoint.
func (r *Relay) Withdraw(ctx context.Context, name string) error {
	c, err := r.open(ctx, "withdraw", name)
	if err != nil {
		return err
	}
	c.Close()
	return nil
}

// replyConn is an open stream with the argument of its "OK" reply.
type replyConn struct {
	*bufConn
	reply string
}

// open sends a request on a new stream and reads its reply.
func (r *Relay) open(ctx context.Context, req ...string) (*replyConn, error) {
	s, err := r.session(ctx)
	if err != nil {
		return nil, err
	}
	st, err := s.OpenStream()
	if err != nil {
		return nil, err
	}
	c := newBufConn(st)
	dl := time.Now().Add(handshakeTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(dl) {
		dl = d
	}
	_ = c.SetDeadline(dl)
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	if err := writeLine(c, req...); err != nil {
		c.Close()
		return nil, err
	}
	line, err := c.readLine()
	if err != nil {
		c.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	ok, arg, _ := strings.Cut(line, " ")
	if line != "OK" && ok != "OK" {
		c.Close()
		return nil, errors.New(strings.TrimPrefix(line, "ERR "))
	}
	if !stop() {
		return nil, ctx.Err()
	}
	_ = c.SetDeadline(time.Time{})
	return &replyConn{bufConn: c, reply: arg}, nil
}

// session returns the control session, connecting if there is none.
func (r *Relay) session(ctx context.Context) (*yamux.Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, errors.New("VM relay closed")
	}
	if r.sess != nil && !r.sess.IsClosed() {
		return r.sess, nil
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", r.ctrl)
	if err != nil {
		return nil, err
	}
	s, err := yamux.Client(c, yamuxConfig())
	if err != nil {
		c.Close()
		return nil, err
	}
	r.sess = s
	go r.serve(s)
	return s, nil
}

// dropSession closes the control connection (the relay notices and
// waits for the next one).
func (r *Relay) dropSession() {
	r.mu.Lock()
	s := r.sess
	r.mu.Unlock()
	if s != nil {
		s.Close()
	}
}

// serve handles streams the relay opens, then reconnects once the
// session ends: Managed containers reach the emulator only while one is up.
func (r *Relay) serve(s *yamux.Session) {
	for {
		st, err := s.AcceptStream()
		if err != nil {
			break
		}
		go r.inbound(st)
	}
	r.reconnect()
}

// reconnect restores the control session; one loop runs at a time.
func (r *Relay) reconnect() {
	r.mu.Lock()
	if r.closed || !r.ready || r.reconnecting {
		r.mu.Unlock()
		return
	}
	r.reconnecting = true
	r.mu.Unlock()
	for delay := 50 * time.Millisecond; ; delay = min(2*delay, 2*time.Second) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := r.ping(ctx)
		cancel()
		r.mu.Lock()
		if r.closed || err == nil && !r.sess.IsClosed() {
			r.reconnecting = false
			r.mu.Unlock()
			if err == nil {
				r.log.Info("VM relay reconnected", "control", r.ctrl)
			}
			return
		}
		r.mu.Unlock()
		r.log.Debug("VM relay reconnect failed", "control", r.ctrl, "err", err)
		r.dropSession()
		time.Sleep(delay)
	}
}

// inbound connects a stream the relay opened to the named endpoint. It
// answers "OK" once it holds what the stream goes to, or "ERR <reason>";
// only after "OK" may the relay have sent bytes it cannot send again.
func (r *Relay) inbound(st net.Conn) {
	c := newBufConn(st)
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(handshakeTimeout))
	line, err := c.readLine()
	f := strings.Fields(line)
	if err != nil || len(f) != 2 || f[0] != "tcp" && f[0] != "udp" {
		_ = writeLine(c, "ERR", fmt.Sprintf("unknown request %q", line))
		return
	}
	target := r.target(f[1])
	if target == "" {
		r.log.Debug("VM relay: endpoint not running", "endpoint", f[1])
		_ = writeLine(c, "ERR", "endpoint "+f[1]+" not running")
		return
	}
	switch f[0] {
	case "tcp":
		u, err := net.DialTimeout("tcp", target, 5*time.Second)
		if err != nil {
			r.log.Debug("VM relay: endpoint dial failed", "endpoint", f[1], "target", target, "err", err)
			_ = writeLine(c, "ERR", err.Error())
			return
		}
		if err := writeLine(c, "OK"); err != nil {
			u.Close()
			return
		}
		r.count(f[1])
		_ = c.SetDeadline(time.Time{})
		pipe(c, u)
	case "udp":
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		q, err := readFrame(c)
		if err != nil {
			return
		}
		u, err := net.Dial("udp", target)
		if err != nil {
			_ = writeLine(c, "ERR", err.Error())
			return
		}
		defer u.Close()
		if err := writeLine(c, "OK"); err != nil {
			return
		}
		_ = u.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := u.Write(q); err != nil {
			return
		}
		r.count(f[1] + "/udp")
		resp := make([]byte, 0xffff)
		n, err := u.Read(resp)
		if err != nil {
			return
		}
		_ = writeFrame(c, resp[:n])
	}
}

func (r *Relay) count(key string) {
	r.mu.Lock()
	r.streams[key]++
	r.mu.Unlock()
}

// Streams returns how many streams the relay has carried per target: the
// container address for dials, the endpoint name for connections handed
// in, and the endpoint name with "/udp" for datagrams.
func (r *Relay) Streams() map[string]int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int64, len(r.streams))
	for k, v := range r.streams {
		out[k] = v
	}
	return out
}

// Close disconnects and removes the relay container.
func (r *Relay) Close(ctx context.Context) error {
	r.mu.Lock()
	r.closed = true
	s := r.sess
	r.mu.Unlock()
	if s != nil {
		s.Close()
	}
	r.rt.SetMember("", nil)
	return r.rt.RemoveContainer(ctx, r.id, true)
}
