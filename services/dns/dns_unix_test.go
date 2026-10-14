//go:build unix

package dns_test

import (
	"context"
	"io"
	"net"
	"strconv"
	"syscall"
	"testing"
	"time"

	mdns "github.com/miekg/dns"
	dnsv1 "google.golang.org/api/dns/v1"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/instance"
	"github.com/linuxuser586/gcpemu/internal/loopback"
	"github.com/linuxuser586/gcpemu/services"
)

// TestStartBesideMDNS: the DNS service starts on a port an mDNS daemon
// holds on the wildcard address (5353 on most Linux desktops, and on macOS
// where mDNSResponder holds it) and answers UDP queries on its loopback bind.
func TestStartBesideMDNS(t *testing.T) {
	inst, port := startBesideMDNS(t)
	c := newClient(t, inst.GatewayURL()+"/")
	if _, err := c.ManagedZones.Create(proj, &dnsv1.ManagedZone{Name: "z", DnsName: "mdns.test.", Description: "d"}).Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ResourceRecordSets.Create(proj, "z", &dnsv1.ResourceRecordSet{Name: "a.mdns.test.", Type: "A", Ttl: 60, Rrdatas: []string{"192.0.2.9"}}).Do(); err != nil {
		t.Fatal(err)
	}
	if got := inst.Endpoint("dns"); got != "127.0.0.1:"+strconv.Itoa(port) {
		t.Fatalf("dns endpoint %s", got)
	}
	r := query(t, inst.Endpoint("dns"), "udp", "a.mdns.test.", mdns.TypeA)
	if len(r.Answer) != 1 || r.Answer[0].(*mdns.A).A.String() != "192.0.2.9" {
		t.Fatalf("UDP answer beside mDNS: %v", r)
	}
}

// startBesideMDNS starts a DNS-only Instance on a port a UDP wildcard
// socket (the mDNS stand-in, held until the test ends) holds. The TCP port
// is free only between its pick and the service's bind, so a parallel
// package can take it; that attempt fails fast and a fresh port is tried.
func startBesideMDNS(t *testing.T) (*emutest.Instance, int) {
	t.Helper()
	const attempts = 5
	var lastErr error
	for range attempts {
		port, holder := mdnsStandIn(t)
		in, err := startDNS(t, port)
		if err == nil {
			t.Cleanup(func() { holder.Close() })
			return in, port
		}
		holder.Close()
		if !loopback.AddrInUse(err) {
			t.Fatalf("start beside mDNS on port %d: %v", port, err)
		}
		lastErr = err
	}
	t.Fatalf("start beside mDNS: %d attempts lost their port: %v", attempts, lastErr)
	return nil, 0
}

// mdnsStandIn picks a free port and holds it on the UDP wildcard address
// with SO_REUSEADDR, as an mDNS daemon does.
func mdnsStandIn(t *testing.T) (int, net.PacketConn) {
	t.Helper()
	tcp, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := tcp.Addr().(*net.TCPAddr).Port
	tcp.Close()
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		_ = c.Control(func(fd uintptr) { serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1) })
		return serr
	}}
	holder, err := lc.ListenPacket(context.Background(), "udp4", "0.0.0.0:"+strconv.Itoa(port))
	if err != nil {
		t.Skipf("port %d taken: %v", port, err)
	}
	return port, holder
}

// startDNS starts a DNS-only Instance on port, configured as emutest.Start
// configures one, and returns the dns service's readiness failure within
// seconds rather than after emutest.Start's minute, shutting the Instance
// down. Failures before the services start end the test.
func startDNS(t *testing.T, port int) (*emutest.Instance, error) {
	t.Helper()
	cfg := config.Defaults()
	cfg.Ephemeral = true
	cfg.DataDir = t.TempDir()
	cfg.Services = []string{"dns"}
	cfg.LogLevel = "warn"
	cfg.Ports[config.AllPorts] = 0
	cfg.DNSNoForward = true
	cfg.Ports["dns"] = port
	in, err := instance.New(&cfg, services.Factories(), io.Discard)
	if err != nil {
		t.Fatalf("instance: %v", err)
	}
	shutdown := func() {
		sctx, scancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer scancel()
		_ = in.Shutdown(sctx)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := in.Start(ctx); err != nil {
		shutdown()
		t.Fatalf("start: %v", err)
	}
	// Start records a failed bind before it returns, so an attempt that
	// lost its port ends here; WaitReady's short timeout covers the rest.
	if err := in.Readiness()["dns"]; err != nil && loopback.AddrInUse(err) {
		shutdown()
		return nil, err
	}
	if err := in.WaitReady(ctx); err != nil {
		if rerr := in.Readiness()["dns"]; rerr != nil {
			err = rerr
		}
		shutdown()
		return nil, err
	}
	t.Cleanup(shutdown)
	return &emutest.Instance{Instance: in}, nil
}
