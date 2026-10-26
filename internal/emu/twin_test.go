package emu

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/lanbind"
	"github.com/linuxuser586/gcpemu/internal/porttest"
)

// needIPv6Loopback skips a test on a host without [::1].
func needIPv6Loopback(t *testing.T) {
	t.Helper()
	l, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	l.Close()
}

func bindEnv(bind string) (*Env, *config.Config) {
	cfg := config.Defaults()
	cfg.Bind = bind
	return &Env{Config: &cfg}, &cfg
}

// serveEcho answers every connection on l with "ok".
func serveEcho(l net.Listener) {
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_, _ = io.WriteString(c, "ok")
			c.Close()
		}
	}()
}

func dialOK(t *testing.T, addr string) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer c.Close()
	b, _ := io.ReadAll(c)
	if string(b) != "ok" {
		t.Fatalf("dial %s: read %q, want ok", addr, b)
	}
}

func portOf(l net.Listener) int { return l.Addr().(*net.TCPAddr).Port }

// TestListenBindTwin: on a loopback --bind, a listener on Config.Bind also
// serves [::1] at the same port, so a client resolving localhost to ::1
// cannot reach another process holding the port on all interfaces (#641).
// Addr stays IPv4, and Close closes both families.
func TestListenBindTwin(t *testing.T) {
	needIPv6Loopback(t)
	for _, bind := range []string{"127.0.0.1", "localhost"} {
		t.Run(bind, func(t *testing.T) {
			e, _ := bindEnv(bind)
			l, err := e.ListenBind(0, true)
			if err != nil {
				t.Fatal(err)
			}
			serveEcho(l)
			p := strconv.Itoa(portOf(l))
			if got, want := l.Addr().String(), "127.0.0.1:"+p; got != want {
				t.Errorf("Addr = %s, want %s", got, want)
			}
			dialOK(t, "127.0.0.1:"+p)
			dialOK(t, "[::1]:"+p)
			if err := l.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := l.Accept(); !errors.Is(err, net.ErrClosed) {
				t.Errorf("Accept after Close = %v, want net.ErrClosed", err)
			}
			for _, a := range []string{"127.0.0.1:" + p, "[::1]:" + p} {
				r, err := net.Listen("tcp", a)
				if err != nil {
					t.Errorf("%s still held after Close: %v", a, err)
					continue
				}
				r.Close()
			}
		})
	}
}

// TestListenBindNoTwin: an unspecified, other 127.x or LAN --bind gets no
// [::1] twin (#641).
func TestListenBindNoTwin(t *testing.T) {
	needIPv6Loopback(t)
	for _, bind := range []string{"0.0.0.0", "::", "127.0.0.2", "192.0.2.1"} {
		if Twinned(bind) {
			t.Errorf("Twinned(%q) = true, want false", bind)
		}
	}
	e, _ := bindEnv("127.0.0.2")
	l, err := e.ListenBind(0, true)
	if err != nil {
		if lanbind.NotLocal(err) {
			t.Skipf("no 127.0.0.2 on this host: %v", err)
		}
		t.Fatal(err)
	}
	defer l.Close()
	h, err := net.Listen("tcp", net.JoinHostPort("::1", strconv.Itoa(portOf(l))))
	if err != nil {
		t.Fatalf("--bind 127.0.0.2 also took [::1]: %v", err)
	}
	h.Close()
}

// TestListenBindTwinTaken: a process holding [::1]:port specifically
// stops the listener, with an error naming the port and IPv6 (#641).
func TestListenBindTwinTaken(t *testing.T) {
	needIPv6Loopback(t)
	held, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	p := portOf(held)
	e, _ := bindEnv("127.0.0.1")
	l, err := e.ListenBind(p, true)
	if err == nil {
		l.Close()
		t.Fatalf("ListenBind(%d) succeeded with [::1]:%d held", p, p)
	}
	if msg := err.Error(); !strings.Contains(msg, strconv.Itoa(p)) || !strings.Contains(msg, "IPv6") {
		t.Errorf("error %q does not name port %d and IPv6", msg, p)
	}
	// The IPv4 side is released again.
	r, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
	if err != nil {
		t.Fatalf("127.0.0.1:%d still held after the failed twin: %v", p, err)
	}
	r.Close()
}

// TestListenBindNoIPv6: on a host without IPv6 loopback, the listener is
// IPv4-only and starting does not fail (#641).
func TestListenBindNoIPv6(t *testing.T) {
	for _, errno := range []error{syscall.EADDRNOTAVAIL, syscall.EAFNOSUPPORT} {
		t.Run(errno.Error(), func(t *testing.T) {
			listenTCP = func(network, addr string) (net.Listener, error) {
				if strings.HasPrefix(addr, "[::1]") {
					return nil, &net.OpError{Op: "listen", Net: network, Err: os.NewSyscallError("bind", errno)}
				}
				return net.Listen(network, addr)
			}
			t.Cleanup(func() { listenTCP = net.Listen })
			e, _ := bindEnv("127.0.0.1")
			l, err := e.ListenBind(0, true)
			if err != nil {
				t.Fatalf("ListenBind without IPv6 loopback: %v", err)
			}
			serveEcho(l)
			dialOK(t, l.Addr().String())
			l.Close()
		})
	}
}

// TestListenBindTwinRange: with --port-range, both families get the same
// port, and a candidate taken on [::1] moves the pair to the next one
// (#641).
func TestListenBindTwinRange(t *testing.T) {
	needIPv6Loopback(t)
	lo := porttest.Window(t, 2)
	held, err := net.Listen("tcp", fmt.Sprintf("[::1]:%d", lo))
	if err != nil {
		t.Skipf("[::1]:%d not free: %v", lo, err)
	}
	defer held.Close()
	e, cfg := bindEnv("127.0.0.1")
	cfg.PortRange = fmt.Sprintf("%d-%d", lo, lo+1)
	for range 5 { // the range starts at a random offset
		l, err := e.ListenBind(0, true)
		if err != nil {
			t.Fatal(err)
		}
		serveEcho(l)
		if p := portOf(l); p != lo+1 {
			l.Close()
			t.Fatalf("port %d, want %d (%d is taken on [::1])", p, lo+1, lo)
		}
		dialOK(t, fmt.Sprintf("[::1]:%d", lo+1))
		l.Close()
	}
	// Port 0 without a range retries until both families are free.
	cfg.PortRange = ""
	l, err := e.ListenBind(0, true)
	if err != nil {
		t.Fatal(err)
	}
	serveEcho(l)
	dialOK(t, fmt.Sprintf("[::1]:%d", portOf(l)))
	l.Close()
}
