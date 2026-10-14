//go:build unix

package dns_test

import (
	"context"
	"net"
	"strconv"
	"syscall"
	"testing"

	mdns "github.com/miekg/dns"
	dnsv1 "google.golang.org/api/dns/v1"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
)

// TestStartBesideMDNS: the DNS service starts on a port an mDNS daemon
// holds on the wildcard address (5353 on most Linux desktops, and on macOS
// where mDNSResponder holds it) and answers UDP queries on its loopback bind.
func TestStartBesideMDNS(t *testing.T) {
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
	defer holder.Close()

	inst := emutest.Start(t, []string{"dns"}, func(c *config.Config) { c.DNSNoForward = true; c.Ports["dns"] = port })
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
