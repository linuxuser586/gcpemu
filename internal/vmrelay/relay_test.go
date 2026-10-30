package vmrelay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/linuxuser586/gcpemu/internal/runtime"
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
	return connect(t, l.Addr().String(), nil, targets)
}

// connect returns the emulator's side connected to an agent at ctrl,
// which it finds again with lookup when not nil.
func connect(t *testing.T, ctrl string, lookup func(context.Context) (string, error), targets map[string]string) *Relay {
	t.Helper()
	r := &Relay{
		log: slog.New(slog.DiscardHandler), ctrl: ctrl, lookup: lookup, ip: "127.0.0.1",
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
	if _, err := r.waitReady(context.Background(), 5*time.Second, time.Second); err != nil {
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
// one the relay listens on. The test holds the pair until the relay binds
// it, so no other process can take the port in between (#643).
func TestInwardKeepsFreePort(t *testing.T) {
	var (
		l   net.Listener
		pc  net.PacketConn
		err error
	)
	for i := range udpPairAttempts {
		if l, pc, err = newRelayAgent().listenPair("127.0.0.1:0", true, i%2 == 0); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close(); pc.Close() })
	want := l.Addr().String()

	a := newRelayAgent()
	a.listenTCP = func(network, addr string) (net.Listener, error) {
		if addr == want {
			return l, nil
		}
		return net.Listen(network, addr)
	}
	a.listenPacket = func(network, addr string) (net.PacketConn, error) {
		if addr == want {
			return pc, nil
		}
		return net.ListenPacket(network, addr)
	}
	ctl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := startOn(t, ctl, a, map[string]string{"dns": echoUDP(t)})
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

// silentTCP accepts connections and holds them open without a word, as a
// stalled port forward does (#807).
func silentTCP(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var held []net.Conn
	t.Cleanup(func() {
		l.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			c.Close()
		}
	})
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	return l.Addr().String()
}

// TestWaitReadySilentControlPort: a control port that accepts connections
// but never answers fails the wait in time, retrying each silent
// connection, and says how many pings failed and the last error (#807).
func TestWaitReadySilentControlPort(t *testing.T) {
	r := &Relay{log: slog.New(slog.DiscardHandler), ctrl: silentTCP(t), streams: map[string]int64{}}
	t.Cleanup(r.dropSession)
	start := time.Now()
	_, err := r.waitReady(context.Background(), 2*time.Second, 300*time.Millisecond)
	if err == nil {
		t.Fatal("waitReady on a silent control port succeeded")
	}
	if took := time.Since(start); took > 4*time.Second {
		t.Fatalf("waitReady took %v, want about 2s", took)
	}
	var n int
	if _, scan := fmt.Sscanf(err.Error(), "%d pings failed, the last with: ", &n); scan != nil || n < 3 {
		t.Fatalf("err = %q, want at least 3 pings failed, the last with: …", err)
	}
}

// TestWaitReadyAfterStall: when the first connection to the control port
// stalls and later ones reach the agent, the wait gives up on the stalled
// one and is ready on a later ping, well before its timeout (#807).
func TestWaitReadyAfterStall(t *testing.T) {
	al, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go newRelayAgent().serve(al)
	t.Cleanup(func() { al.Close() })
	stall := silentTCP(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for first := true; ; first = false {
			c, err := l.Accept()
			if err != nil {
				return
			}
			to := al.Addr().String()
			if first {
				to = stall
			}
			u, err := net.Dial("tcp", to)
			if err != nil {
				c.Close()
				continue
			}
			go pipe(c, u)
		}
	}()

	r := &Relay{log: slog.New(slog.DiscardHandler), ctrl: l.Addr().String(), streams: map[string]int64{}}
	t.Cleanup(func() {
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
		r.dropSession()
	})
	start := time.Now()
	pings, err := r.waitReady(context.Background(), 10*time.Second, 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if pings != 2 {
		t.Fatalf("ready after %d pings, want 2", pings)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("waitReady took %v, want well under its 10s timeout", took)
	}
}

// TestProbeControl: the probe that runs beside a relay whose control port
// does not answer on the host gets the agent's answer when it listens, and
// an error when its port is silent (#807).
func TestProbeControl(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go newRelayAgent().serve(l)
	t.Cleanup(func() { l.Close() })
	if err := probeControl(context.Background(), l.Addr().String(), time.Second); err != nil {
		t.Fatalf("probe of a listening agent: %v", err)
	}
	if err := probeControl(context.Background(), silentTCP(t), 300*time.Millisecond); err == nil {
		t.Fatal("probe of a silent port succeeded")
	}
}

// TestRecreateOnStall: a start whose control port stalled in the
// runtime's port forward, while the agent answered inside the VM, is
// tried once more; any other outcome is final (#807).
func TestRecreateOnStall(t *testing.T) {
	stalled := fmt.Errorf("VM relay: control port did not answer: %w", errForwardStalled)
	other := errors.New("VM relay: control port did not answer: a probe got no answer either")
	ok := &Relay{}
	for _, tc := range []struct {
		name     string
		attempts []error
		calls    int
		want     []error // in the error, nil for success
	}{
		{"started", []error{nil}, 1, nil},
		{"stalled then started", []error{stalled, nil}, 2, nil},
		{"not the port forward", []error{other}, 1, []error{other}},
		{"stalled twice", []error{stalled, stalled}, 2, []error{stalled}},
		{"stalled then other", []error{stalled, other}, 2, []error{stalled, other}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			r, err := recreateOnStall(slog.New(slog.DiscardHandler), func() (*Relay, error) {
				err := tc.attempts[calls]
				calls++
				if err != nil {
					return nil, err
				}
				return ok, nil
			})
			if calls != tc.calls {
				t.Errorf("%d attempts, want %d", calls, tc.calls)
			}
			if tc.want == nil {
				if err != nil || r != ok {
					t.Fatalf("got %v, %v; want the started relay", r, err)
				}
				return
			}
			if err == nil {
				t.Fatal("succeeded, want an error")
			}
			for _, w := range tc.want {
				if !errors.Is(err, w) {
					t.Errorf("err = %q, want it to hold %q", err, w)
				}
			}
			if calls == 2 && !strings.Contains(err.Error(), "after recreating the relay") {
				t.Errorf("err = %q, want both attempts named", err)
			}
		})
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

// TestOfferThroughRelay is the relay side of an Offered port (ADR 0014):
// the relay listens on exactly the address asked for, over TCP or UDP,
// refuses a taken port rather than moving, and stops listening when the
// offer is withdrawn.
func TestOfferThroughRelay(t *testing.T) {
	host, hostUDP := echoTCP(t), echoUDP(t)
	// The test holds its TCP port and its UDP port until the relay binds
	// them, so no other process can take them in between.
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { held.Close() })
	heldUDP, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { heldUDP.Close() })
	addr, uaddr := held.Addr().String(), heldUDP.LocalAddr().String()

	// Each held socket is handed over once: binding the address again,
	// after the withdraw, binds it anew. offer holds the agent's lock, so
	// the hooks run one at a time.
	a := newRelayAgent()
	var handed, handedUDP bool
	a.listenTCP = func(network, bind string) (net.Listener, error) {
		if bind == addr && !handed {
			handed = true
			return held, nil
		}
		return net.Listen(network, bind)
	}
	a.listenPacket = func(network, bind string) (net.PacketConn, error) {
		if bind == uaddr && !handedUDP {
			handedUDP = true
			return heldUDP, nil
		}
		return net.ListenPacket(network, bind)
	}
	ctl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := startOn(t, ctl, a, map[string]string{"offer-tcp": host, "offer-udp": hostUDP})
	ctx := context.Background()

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
	// The port is free to offer again. The withdraw closed the held
	// socket, so this bind is fresh, and another process could take the
	// port first: the step checks that the withdraw released it.
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
	// Not a port the kernel picks: the agent's listeners below may be
	// given that one, and the endpoint would relay to itself.
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

// published forwards a host port to target, as a runtime publishes a
// container port; unpublish closes the port and the connections on it.
func published(t *testing.T, target string) (addr string, unpublish func()) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			u, err := net.Dial("tcp", target)
			if err != nil {
				c.Close()
				continue
			}
			mu.Lock()
			conns = append(conns, c, u)
			mu.Unlock()
			go func() { _, _ = io.Copy(u, c); u.Close() }()
			go func() { _, _ = io.Copy(c, u); c.Close() }()
		}
	}()
	unpublish = sync.OnceFunc(func() {
		l.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	})
	t.Cleanup(unpublish)
	return l.Addr().String(), unpublish
}

// TestReconnectMovedControlPort: when the runtime publishes the control
// port on a new host port (a network the relay joins becomes its gateway
// endpoint, #555), the emulator finds the new port and reconnects, so
// dials and inward connections work again.
func TestReconnectMovedControlPort(t *testing.T) {
	gw := echoTCP(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	first, unpublish := published(t, l.Addr().String())
	var mu sync.Mutex
	current := first
	go newRelayAgent().serve(l)
	t.Cleanup(func() { l.Close() })
	r := connect(t, first, func(context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		return current, nil
	}, map[string]string{"gateway": gw})
	ctx := context.Background()
	addr, err := r.Listen(ctx, "gateway", "127.0.0.1:0", false)
	if err != nil {
		t.Fatal(err)
	}

	moved, _ := published(t, l.Addr().String())
	mu.Lock()
	current = moved
	mu.Unlock()
	unpublish()

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	if got := roundTrip(t, c, "moved"); got != "re:moved" {
		t.Fatalf("inward after the port moved: %q", got)
	}
	d, err := r.DialContext(ctx, "tcp", gw)
	if err != nil {
		t.Fatal(err)
	}
	if got := roundTrip(t, d, "dial"); got != "re:dial" {
		t.Fatalf("dial after the port moved: %q", got)
	}
	if got := r.Control(); got != moved {
		t.Fatalf("Control() = %s, want %s", got, moved)
	}
}

// TestReconnectLookupFails: while the relay's control port cannot be
// found, reconnect keeps backing off, and recovers once it can be.
func TestReconnectLookupFails(t *testing.T) {
	gw := echoTCP(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := newRelayAgent()
	go a.serve(l)
	t.Cleanup(func() { l.Close() })
	ctrl := l.Addr().String()
	var mu sync.Mutex
	gone, lookups := false, 0
	r := connect(t, ctrl, func(context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		lookups++
		if gone {
			return "", errors.New("no such container")
		}
		return ctrl, nil
	}, map[string]string{"gateway": gw})
	mu.Lock()
	gone, lookups = true, 0
	mu.Unlock()
	dropAgentSession(t, r, a)
	time.Sleep(500 * time.Millisecond)
	mu.Lock()
	n := lookups
	gone = false
	mu.Unlock()
	// Backing off from 50ms: a handful of tries in half a second, not a spin.
	if n == 0 || n > 10 {
		t.Fatalf("%d lookups in 500ms, want a few", n)
	}
	d, err := r.DialContext(context.Background(), "tcp", gw)
	if err != nil {
		t.Fatal(err)
	}
	if got := roundTrip(t, d, "back"); got != "re:back" {
		t.Fatalf("dial after the lookup recovered: %q", got)
	}
}

// TestJoinRefreshesControl: when a network the relay joins makes the
// runtime publish the control port elsewhere without dropping the
// session (OrbStack, #808), Control() follows the port while the
// session stays up.
func TestJoinRefreshesControl(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go newRelayAgent().serve(l)
	t.Cleanup(func() { l.Close() })
	ctrl := l.Addr().String()
	// Nothing listens at moved: the join must not redial.
	const moved = "127.0.0.1:1"
	var mu sync.Mutex
	current := ctrl
	r := connect(t, ctrl, func(context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		return current, nil
	}, nil)
	s := currentSession(r)
	mu.Lock()
	current = moved
	mu.Unlock()
	r.joined(runtime.Network{Name: "build", Subnet: "10.9.0.0/24"}, true)
	if got := r.Control(); got != moved {
		t.Fatalf("Control() = %s, want %s", got, moved)
	}
	if currentSession(r) != s || s.IsClosed() {
		t.Fatal("the join replaced or closed the control session")
	}
	if err := r.ping(context.Background()); err != nil {
		t.Fatalf("ping over the session after the join: %v", err)
	}
}

// TestJoinLookupFails: a join whose lookup fails keeps the control
// address and the session.
func TestJoinLookupFails(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go newRelayAgent().serve(l)
	t.Cleanup(func() { l.Close() })
	ctrl := l.Addr().String()
	r := connect(t, ctrl, func(context.Context) (string, error) {
		return "", errors.New("no such container")
	}, nil)
	s := currentSession(r)
	r.joined(runtime.Network{Name: "build", Subnet: "10.9.0.0/24"}, true)
	if got := r.Control(); got != ctrl {
		t.Fatalf("Control() = %s, want %s", got, ctrl)
	}
	if currentSession(r) != s || s.IsClosed() {
		t.Fatal("the join replaced or closed the control session")
	}
	if !r.Covers("10.9.0.5") {
		t.Fatal("the join was not recorded")
	}
}

// TestJoinWithoutLookup: leaving a network, and joining one before the
// relay is ready (Start, before the control port is published), do not
// look the control port up.
func TestJoinWithoutLookup(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go newRelayAgent().serve(l)
	t.Cleanup(func() { l.Close() })
	lookups := 0
	lookup := func(context.Context) (string, error) {
		lookups++
		return "127.0.0.1:1", nil
	}
	n := runtime.Network{Name: "build", Subnet: "10.9.0.0/24"}
	r := connect(t, l.Addr().String(), lookup, nil)
	r.joined(n, false)
	if lookups != 0 {
		t.Fatalf("%d lookups on leaving a network", lookups)
	}

	// Not through connect, which waits for the relay to be ready.
	starting := &Relay{log: slog.New(slog.DiscardHandler), lookup: lookup, nets: map[string]netip.Prefix{}}
	starting.joined(n, true)
	if lookups != 0 {
		t.Fatalf("%d lookups on a join before the relay is ready", lookups)
	}
	if !starting.Covers("10.9.0.5") {
		t.Fatal("the join was not recorded")
	}
}

func currentSession(r *Relay) *yamux.Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sess
}
