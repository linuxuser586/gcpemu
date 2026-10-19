package vmrelay

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

// startInProcess runs the agent on loopback and returns the emulator's
// side connected to it, with endpoints resolved from targets.
func startInProcess(t *testing.T, targets map[string]string) *Relay {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return startOn(t, l, newRelayAgent(), targets)
}

// startOn runs a on l and returns the emulator's side connected to it.
func startOn(t *testing.T, l net.Listener, a *relayAgent, targets map[string]string) *Relay {
	t.Helper()
	go a.serve(l)
	t.Cleanup(func() { l.Close() })
	r := &Relay{
		log: slog.New(slog.DiscardHandler), ctrl: l.Addr().String(), ip: "127.0.0.1",
		target:  func(name string) string { return targets[name] },
		nets:    map[string]netip.Prefix{"lo": netip.MustParsePrefix("127.0.0.0/8")},
		streams: map[string]int64{},
	}
	t.Cleanup(func() {
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
		r.dropSession()
	})
	if err := r.waitReady(context.Background(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	return r
}

// echoTCP serves "re:" + what it reads until EOF on each connection.
func echoTCP(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				b, _ := io.ReadAll(c)
				_, _ = c.Write(append([]byte("re:"), b...))
			}()
		}
	}()
	return l.Addr().String()
}

func echoUDP(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(append([]byte("re:"), buf[:n]...), from)
		}
	}()
	return pc.LocalAddr().String()
}

// roundTrip writes msg, half-closes and reads the whole answer, which
// needs the half-close to cross the relay.
func roundTrip(t *testing.T, c net.Conn, msg string) string {
	t.Helper()
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, msg); err != nil {
		t.Fatal(err)
	}
	c.(interface{ CloseWrite() error }).CloseWrite()
	b, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDialThroughRelay(t *testing.T) {
	echo := echoTCP(t)
	r := startInProcess(t, nil)
	c, err := r.DialContext(context.Background(), "tcp", echo)
	if err != nil {
		t.Fatal(err)
	}
	if got := roundTrip(t, c, "hello"); got != "re:hello" {
		t.Fatalf("answer %q", got)
	}
	if n := r.Streams()[echo]; n != 1 {
		t.Fatalf("streams to %s = %d, want 1", echo, n)
	}

	// A refused dial fails the dial itself, as a direct one would.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := l.Addr().String()
	l.Close()
	if _, err := r.DialContext(context.Background(), "tcp", closed); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("dial to a closed port: %v", err)
	}
	if !r.Covers("127.0.0.5") || r.Covers("10.0.0.1") {
		t.Fatal("Covers does not follow the joined subnets")
	}
}

func TestInwardThroughRelay(t *testing.T) {
	gw, dns := echoTCP(t), echoUDP(t)
	r := startInProcess(t, map[string]string{"gateway": gw, "dns": dns})
	ctx := context.Background()
	addr, err := r.Listen(ctx, "gateway", "127.0.0.1:0", false)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := r.Listen(ctx, "gateway", "127.0.0.1:0", false); err != nil || again != addr {
		t.Fatalf("second listen = %q, %v; want %q", again, err, addr)
	}
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	if got := roundTrip(t, c, "in"); got != "re:in" {
		t.Fatalf("answer %q", got)
	}

	daddr, err := r.Listen(ctx, "dns", "127.0.0.1:0", true)
	if err != nil {
		t.Fatal(err)
	}
	u, err := net.Dial("udp", daddr)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	_ = u.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := u.Write([]byte("q")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	if n, err := u.Read(buf); err != nil || string(buf[:n]) != "re:q" {
		t.Fatalf("datagram answer %q %v", buf[:n], err)
	}
	st := r.Streams()
	if st["gateway"] != 1 || st["dns/udp"] != 1 {
		t.Fatalf("streams %v", st)
	}
}

// TestInwardUDPTwinRefused: on Windows the UDP twin of every port TCP
// picks can sit in one excluded port range, since the kernel picks nearby
// ports (#459). The relay still finds a port both protocols hold.
func TestInwardUDPTwinRefused(t *testing.T) {
	a := newRelayAgent()
	a.listenPacket = func(network, addr string) (net.PacketConn, error) {
		if _, port, _ := net.SplitHostPort(addr); port != "0" {
			return nil, &net.OpError{Op: "listen", Net: network, Err: errors.New("bind: forbidden by its access permissions")}
		}
		return net.ListenPacket(network, addr)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := startOn(t, l, a, map[string]string{"dns": echoUDP(t)})
	daddr, err := r.Listen(context.Background(), "dns", "127.0.0.1:0", true)
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("tcp", daddr)
	if err != nil {
		t.Fatalf("no TCP listener on %s: %v", daddr, err)
	}
	c.Close()
	u, err := net.Dial("udp", daddr)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	_ = u.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := u.Write([]byte("q")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	if n, err := u.Read(buf); err != nil || string(buf[:n]) != "re:q" {
		t.Fatalf("datagram answer %q %v", buf[:n], err)
	}
}

// TestInwardKeepsFreePort: a free port asked for over TCP and UDP is the
// one the relay listens on.
func TestInwardKeepsFreePort(t *testing.T) {
	r := startInProcess(t, map[string]string{"dns": echoUDP(t)})
	want := freePort(t, "udp")
	got, err := r.Listen(context.Background(), "dns", want, true)
	if err != nil || got != want {
		t.Fatalf("listen = %q, %v; want %q", got, err, want)
	}
}

// TestInwardNoUsablePort: when no port takes UDP, the error names the
// port tried last and the UDP bind that failed.
func TestInwardNoUsablePort(t *testing.T) {
	a := newRelayAgent()
	a.listenPacket = func(network, addr string) (net.PacketConn, error) {
		return nil, &net.OpError{Op: "listen", Net: network, Err: errors.New("bind: forbidden by its access permissions")}
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := startOn(t, l, a, map[string]string{"dns": echoUDP(t)})
	_, err = r.Listen(context.Background(), "dns", "127.0.0.1:0", true)
	if err == nil || !strings.Contains(err.Error(), "udp on 127.0.0.1:") || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("listen error = %v; want the port and the refused UDP bind", err)
	}
}

// TestReconnect: after the control connection drops, the emulator
// reconnects by itself, so containers reach it again, even straight
// away, and new dials work.
func TestReconnect(t *testing.T) {
	gw := echoTCP(t)
	r := startInProcess(t, map[string]string{"gateway": gw})
	ctx := context.Background()
	addr, err := r.Listen(ctx, "gateway", "127.0.0.1:0", false)
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	old := r.sess
	r.mu.Unlock()
	old.Close()

	// No waiting for the reconnect: the agent holds the connection, or
	// retries it, until its next session is in place.
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	if got := roundTrip(t, c, "after"); got != "re:after" {
		t.Fatalf("inward after reconnect: %q", got)
	}
	d, err := r.DialContext(ctx, "tcp", gw)
	if err != nil {
		t.Fatal(err)
	}
	if got := roundTrip(t, d, "dial"); got != "re:dial" {
		t.Fatalf("dial after reconnect: %q", got)
	}
}

func TestDecide(t *testing.T) {
	for _, tc := range []struct {
		env         string
		inContainer bool
		gateway     string
		on, vm      bool
		reason      string
	}{
		{"", false, "127.0.0.1", false, false, "gateway 127.0.0.1 bindable"},
		{"", false, "192.0.2.1", true, true, "gateway 192.0.2.1 not bindable"},
		{"1", false, "127.0.0.1", true, false, "forced by GCPEMU_VM_RELAY"},
		{"1", false, "192.0.2.1", true, true, "forced by GCPEMU_VM_RELAY"},
		{"0", false, "192.0.2.1", false, true, "disabled by GCPEMU_VM_RELAY"},
		{"1", true, "192.0.2.1", false, false, "the emulator runs in a container"},
	} {
		t.Setenv(Env, tc.env)
		if d := Decide(tc.inContainer, tc.gateway); d.On != tc.on || d.VM != tc.vm || d.Reason != tc.reason {
			t.Errorf("Decide(%v, %s) with %s=%q = %+v, want on=%v vm=%v %q", tc.inContainer, tc.gateway, Env, tc.env, d, tc.on, tc.vm, tc.reason)
		}
	}
}

// freePort returns a loopback address with a port that was free a moment
// ago.
func freePort(t *testing.T, network string) string {
	t.Helper()
	if network == "udp" {
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer pc.Close()
		return pc.LocalAddr().String()
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// TestOfferThroughRelay is the relay side of an Offered port (ADR 0014):
// the relay listens on exactly the address asked for, over TCP or UDP,
// refuses a taken port rather than moving, and stops listening when the
// offer is withdrawn.
func TestOfferThroughRelay(t *testing.T) {
	host, hostUDP := echoTCP(t), echoUDP(t)
	r := startInProcess(t, map[string]string{"offer-tcp": host, "offer-udp": hostUDP})
	ctx := context.Background()

	addr := freePort(t, "tcp")
	if err := r.Offer(ctx, "offer-tcp", addr, false); err != nil {
		t.Fatal(err)
	}
	if err := r.Offer(ctx, "offer-tcp", addr, false); err != nil {
		t.Fatalf("offering again: %v", err)
	}
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	if got := roundTrip(t, c, "in"); got != "re:in" {
		t.Fatalf("answer %q", got)
	}

	taken := echoTCP(t)
	if err := r.Offer(ctx, "taken", taken, false); err == nil {
		t.Fatalf("offering the taken %s succeeded", taken)
	}

	uaddr := freePort(t, "udp")
	if err := r.Offer(ctx, "offer-udp", uaddr, true); err != nil {
		t.Fatal(err)
	}
	u, err := net.Dial("udp", uaddr)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	_ = u.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := u.Write([]byte("q")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	if n, err := u.Read(buf); err != nil || string(buf[:n]) != "re:q" {
		t.Fatalf("datagram answer %q %v", buf[:n], err)
	}

	if err := r.Withdraw(ctx, "offer-tcp"); err != nil {
		t.Fatal(err)
	}
	if c, err := net.Dial("tcp", addr); err == nil {
		c.Close()
		t.Fatalf("%s still accepts after the withdraw", addr)
	}
	if err := r.Withdraw(ctx, "offer-tcp"); err != nil {
		t.Fatalf("withdrawing again: %v", err)
	}
	// The port is free to offer again.
	if err := r.Offer(ctx, "offer-tcp", addr, false); err != nil {
		t.Fatalf("offering after the withdraw: %v", err)
	}
}

// gatedListener holds each accepted control connection while held, so the
// agent stays without a session until release.
type gatedListener struct {
	net.Listener
	mu   sync.Mutex
	gate chan struct{}
}

func (l *gatedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	l.mu.Lock()
	g := l.gate
	l.mu.Unlock()
	if g != nil {
		<-g
	}
	return c, err
}

// hold makes later accepts wait; the returned func releases them.
func (l *gatedListener) hold() (release func()) {
	g := make(chan struct{})
	l.mu.Lock()
	l.gate = g
	l.mu.Unlock()
	return sync.OnceFunc(func() {
		l.mu.Lock()
		l.gate = nil
		l.mu.Unlock()
		close(g)
	})
}

// dropAgentSession closes the emulator's control session and waits until
// the agent has seen its own end close.
func dropAgentSession(t *testing.T, r *Relay, a *relayAgent) {
	t.Helper()
	a.mu.Lock()
	as := a.sess
	a.mu.Unlock()
	r.mu.Lock()
	old := r.sess
	r.mu.Unlock()
	old.Close()
	select {
	case <-as.CloseChan():
	case <-time.After(5 * time.Second):
		t.Fatal("the agent did not see its session close")
	}
}

// TestInwardDuringReconnect: a connection or datagram that reaches the
// agent between losing its session and accepting the next one waits for
// the next one and goes through.
func TestInwardDuringReconnect(t *testing.T) {
	gw, dns := echoTCP(t), echoUDP(t)
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := &gatedListener{Listener: inner}
	a := newRelayAgent()
	r := startOn(t, l, a, map[string]string{"gateway": gw, "dns": dns})
	ctx := context.Background()
	addr, err := r.Listen(ctx, "gateway", "127.0.0.1:0", false)
	if err != nil {
		t.Fatal(err)
	}
	daddr, err := r.Listen(ctx, "dns", "127.0.0.1:0", true)
	if err != nil {
		t.Fatal(err)
	}

	release := l.hold()
	defer release()
	dropAgentSession(t, r, a)

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	u, err := net.Dial("udp", daddr)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	_ = u.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := u.Write([]byte("q")); err != nil {
		t.Fatal(err)
	}
	// Let both reach the agent while it has no session.
	time.Sleep(200 * time.Millisecond)
	release()

	if got := roundTrip(t, c, "gap"); got != "re:gap" {
		t.Fatalf("inward during the gap: %q", got)
	}
	buf := make([]byte, 64)
	if n, err := u.Read(buf); err != nil || string(buf[:n]) != "re:q" {
		t.Fatalf("datagram during the gap: %q %v", buf[:n], err)
	}
}

// freezeListener lets a test stall the agent's writes on its latest
// control connection and then cut it, so a stream the agent opens there
// never reaches the emulator.
type freezeListener struct {
	net.Listener
	mu   sync.Mutex
	last *freezeConn
}

func (l *freezeListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	fc := &freezeConn{Conn: c}
	l.mu.Lock()
	l.last = fc
	l.mu.Unlock()
	return fc, nil
}

// freeze stalls writes on the latest connection; the returned func cuts it.
func (l *freezeListener) freeze() (cut func()) {
	l.mu.Lock()
	fc := l.last
	l.mu.Unlock()
	g := make(chan struct{})
	fc.mu.Lock()
	fc.frozen = g
	fc.stalled = make(chan struct{})
	fc.mu.Unlock()
	return sync.OnceFunc(func() {
		close(g)
		fc.Conn.Close()
	})
}

// stalled is closed once a write on the latest connection is held.
func (l *freezeListener) stalled() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.last.mu.Lock()
	defer l.last.mu.Unlock()
	return l.last.stalled
}

type freezeConn struct {
	net.Conn
	mu      sync.Mutex
	frozen  chan struct{}
	stalled chan struct{}
	once    sync.Once
}

func (c *freezeConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	g, stalled := c.frozen, c.stalled
	c.mu.Unlock()
	if g != nil {
		c.once.Do(func() { close(stalled) })
		<-g
		return 0, net.ErrClosed
	}
	return c.Conn.Write(p)
}

// TestInwardRetriedOnNextSession: a connection the agent sends on a
// session that dies before the emulator answers is sent again on the next
// session.
func TestInwardRetriedOnNextSession(t *testing.T) {
	gw := echoTCP(t)
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := &freezeListener{Listener: inner}
	r := startOn(t, l, newRelayAgent(), map[string]string{"gateway": gw})
	addr, err := r.Listen(context.Background(), "gateway", "127.0.0.1:0", false)
	if err != nil {
		t.Fatal(err)
	}

	cut := l.freeze()
	defer cut()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	// The agent opens its stream on the frozen session.
	select {
	case <-l.stalled():
	case <-time.After(5 * time.Second):
		t.Fatal("the agent sent nothing on its session")
	}
	cut()
	if got := roundTrip(t, c, "retry"); got != "re:retry" {
		t.Fatalf("inward across a dead session: %q", got)
	}
}

// closedWithin fails unless the peer closes c within d.
func closedWithin(t *testing.T, c net.Conn, d time.Duration) {
	t.Helper()
	defer c.Close()
	start := time.Now()
	_ = c.SetDeadline(start.Add(d))
	if _, err := io.ReadAll(c); err != nil {
		t.Fatalf("not closed within %v: %v", d, err)
	}
	if time.Since(start) > d {
		t.Fatalf("closed after %v, want within %v", time.Since(start), d)
	}
}

// TestInwardWithoutSession: a connection that arrives while no session
// comes back is closed once the agent's wait runs out.
func TestInwardWithoutSession(t *testing.T) {
	gw := echoTCP(t)
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := &gatedListener{Listener: inner}
	a := newRelayAgent()
	a.wait = 300 * time.Millisecond
	r := startOn(t, l, a, map[string]string{"gateway": gw})
	addr, err := r.Listen(context.Background(), "gateway", "127.0.0.1:0", false)
	if err != nil {
		t.Fatal(err)
	}
	release := l.hold()
	defer release()
	dropAgentSession(t, r, a)

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	closedWithin(t, c, 3*time.Second)
}

// TestInwardEndpointDown: when the endpoint is not running, or refuses,
// the emulator says so and the agent closes the connection at once,
// without waiting for another session.
func TestInwardEndpointDown(t *testing.T) {
	// Not a port freePort found: the agent's listeners below may be given
	// that one, and the endpoint would relay to itself.
	const refused = "127.0.0.1:1"
	r := startInProcess(t, map[string]string{"refused": refused})
	ctx := context.Background()
	for _, name := range []string{"ghost", "refused"} {
		addr, err := r.Listen(ctx, name, "127.0.0.1:0", false)
		if err != nil {
			t.Fatal(err)
		}
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		// Well inside the agent's wait, which a retry would sit out.
		closedWithin(t, c, time.Second)
		if n := r.Streams()[name]; n != 0 {
			t.Fatalf("streams to %s = %d, want 0", name, n)
		}
	}
}
