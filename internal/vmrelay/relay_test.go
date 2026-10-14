package vmrelay

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
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
	a := newRelayAgent()
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

// TestReconnect: after the control connection drops, the emulator
// reconnects by itself, so containers reach it again, and new dials work.
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

	deadline := time.Now().Add(5 * time.Second)
	for {
		r.mu.Lock()
		s := r.sess
		r.mu.Unlock()
		if s != old && s != nil && !s.IsClosed() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no reconnect")
		}
		time.Sleep(20 * time.Millisecond)
	}
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
