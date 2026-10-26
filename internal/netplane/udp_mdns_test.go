//go:build unix || windows

package netplane

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/porttest"
)

// mdnsHolder binds 0.0.0.0:port and, where the host has IPv6, [::]:port,
// each with SO_REUSEADDR, as avahi-daemon, systemd-resolved, mDNSResponder
// and browsers do for mDNS on 5353. It returns the IPv4 socket. The IPv6
// socket matters on macOS, which keeps the IPv4 and IPv6 port spaces
// apart: without it a dual-stack wildcard bind would succeed beside the
// IPv4 socket (issue #262).
func mdnsHolder(t *testing.T, port string) net.PacketConn {
	t.Helper()
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) { serr = setReuseAddr(fd) }); err != nil {
			return err
		}
		return serr
	}}
	pc, err := lc.ListenPacket(context.Background(), "udp4", "0.0.0.0:"+port)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	pc6, err := lc.ListenPacket(context.Background(), "udp6", "[::]:"+port)
	switch {
	case err == nil:
		t.Cleanup(func() { pc6.Close() })
	case isAny(err, noIPv6Errs):
		// No IPv6 (as in some CI containers): a wildcard bind is IPv4-only
		// and clashes with the IPv4 socket.
	default:
		t.Fatal(err)
	}
	return pc
}

// freeUDPPort returns a port free on 127.0.0.1 for both UDP and TCP: the
// forwarder binds TCP on the port first and falls back to another port
// when it is taken.
func freeUDPPort(t *testing.T) string {
	t.Helper()
	for range 100 {
		port := strconv.Itoa(porttest.Free(t))
		if pc, err := net.ListenPacket("udp4", "127.0.0.1:"+port); err == nil {
			pc.Close()
			return port
		}
	}
	t.Fatal("no port free for both TCP and UDP")
	return ""
}

// TestListenUDPBesideMDNS: with a wildcard mDNS socket on the port, a
// plain bind of a specific address fails, ListenUDP succeeds, and unicast
// queries to that address reach the emulator rather than the mDNS daemon.
// A wildcard bind does not use SO_REUSEADDR, so it can't take over the
// daemon's traffic. The plain binds are only checked where the test's
// holder refuses them (see holderRefusesPlainBinds).
func TestListenUDPBesideMDNS(t *testing.T) {
	port := freeUDPPort(t)
	holder := mdnsHolder(t, port)
	ctx := context.Background()

	if pc, err := net.ListenPacket("udp4", "127.0.0.1:"+port); err == nil {
		pc.Close()
		if holderRefusesPlainBinds {
			t.Fatal("a plain bind beside the mDNS socket succeeded; the scenario is not reproduced")
		}
	} else if !isAny(err, plainBindErrs) {
		t.Fatalf("plain bind: %v", err)
	}
	ours, err := ListenUDP(ctx, "127.0.0.1", port)
	if err != nil {
		t.Fatalf("ListenUDP beside mDNS: %v", err)
	}
	defer ours.Close()

	c, err := net.Dial("udp4", "127.0.0.1:"+port)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("query")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	_ = ours.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, _, err := ours.ReadFrom(buf); err != nil || string(buf[:n]) != "query" {
		t.Fatalf("our socket: %q %v", buf[:n], err)
	}
	_ = holder.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if n, _, err := holder.ReadFrom(buf); err == nil {
		t.Errorf("the mDNS socket also received %q", buf[:n])
	}

	if !holderRefusesPlainBinds {
		return
	}
	for _, host := range []string{"0.0.0.0", ""} {
		if pc, err := ListenUDP(ctx, host, port); err == nil {
			pc.Close()
			t.Errorf("wildcard bind %q shared the mDNS port", host)
		}
	}
}

// TestForwarderBesideMDNS: the DNS forwarder binds its UDP side on the
// endpoint's port while an mDNS daemon holds that port on the wildcard
// address (issue #114), and relays queries to the target.
func TestForwarderBesideMDNS(t *testing.T) {
	port := freeUDPPort(t)
	mdnsHolder(t, port)
	echo, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		buf := make([]byte, 64)
		for {
			n, from, err := echo.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = echo.WriteTo(append([]byte("re:"), buf[:n]...), from)
		}
	}()

	f, err := newForwarder("127.0.0.1", port, echo.LocalAddr().String(), true, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("forwarder beside mDNS: %v", err)
	}
	defer f.close()
	if want := "127.0.0.1:" + port; f.addr != want {
		t.Fatalf("forwarder on %s, want %s", f.addr, want)
	}
	c, err := net.Dial("udp4", f.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write([]byte("q")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	if n, err := c.Read(buf); err != nil || string(buf[:n]) != "re:q" {
		t.Fatalf("relayed answer %q %v", buf[:n], err)
	}
}

func isAny(err error, targets []error) bool {
	for _, t := range targets {
		if errors.Is(err, t) {
			return true
		}
	}
	return false
}
