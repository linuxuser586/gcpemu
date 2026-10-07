package dns

import (
	"context"
	"errors"
	"net"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// mdnsHolder binds 0.0.0.0:port with SO_REUSEADDR, as avahi-daemon,
// systemd-resolved and browsers do for mDNS on 5353.
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
	return pc
}

func freeUDPPort(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	return strconv.Itoa(pc.LocalAddr().(*net.UDPAddr).Port)
}

// TestListenUDPBesideMDNS: with a wildcard mDNS socket on the port, a
// plain bind of a specific address fails, listenUDP succeeds, and unicast
// queries to that address reach the emulator rather than the mDNS daemon.
// A wildcard bind does not use SO_REUSEADDR, so it can't take over the
// daemon's traffic.
func TestListenUDPBesideMDNS(t *testing.T) {
	port := freeUDPPort(t)
	holder := mdnsHolder(t, port)
	ctx := context.Background()

	if pc, err := net.ListenPacket("udp4", "127.0.0.1:"+port); err == nil {
		pc.Close()
		t.Fatal("a plain bind beside the mDNS socket succeeded; the scenario is not reproduced")
	} else if !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("plain bind: %v", err)
	}
	ours, err := listenUDP(ctx, "127.0.0.1", port)
	if err != nil {
		t.Fatalf("listenUDP beside mDNS: %v", err)
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

	for _, host := range []string{"0.0.0.0", ""} {
		if pc, err := listenUDP(ctx, host, port); err == nil {
			pc.Close()
			t.Errorf("wildcard bind %q shared the mDNS port", host)
		}
	}
}
