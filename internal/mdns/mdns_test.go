package mdns_test

import (
	"context"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/hosttest"
	"github.com/linuxuser586/gcpemu/internal/mdns"
	"github.com/linuxuser586/gcpemu/internal/netplane"
)

// daemonStandIn holds 0.0.0.0:port with SO_REUSEADDR (and SO_REUSEPORT
// where there is one), as avahi-daemon, mDNSResponder and Windows' DNS
// Client hold 5353.
func daemonStandIn(t *testing.T, port int) {
	t.Helper()
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		_ = c.Control(func(fd uintptr) { serr = mdns.SetReuse(fd) })
		return serr
	}}
	pc, err := lc.ListenPacket(context.Background(), "udp4", "0.0.0.0:"+strconv.Itoa(port))
	if err != nil {
		t.Skipf("port %d taken: %v", port, err)
	}
	t.Cleanup(func() { pc.Close() })
}

// advertise advertises svc on port until the test ends.
func advertise(t *testing.T, svc mdns.Service, port int) *mdns.Responder {
	t.Helper()
	r, err := mdns.Advertise(svc, mdns.Options{Port: port, Log: testLog(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// browse browses for the tests' service type on port.
func browse(t *testing.T, port int) []mdns.Entry {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := mdns.Browse(ctx, "_gcpemu-test._tcp", mdns.Options{Port: port, Wait: time.Second, Log: testLog(t)})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// An advertised service is browsed on the same LAN, with its port,
// addresses and TXT records, beside an mDNS daemon on the wildcard address
// and the DNS service on loopback, as Host mode's DNS forwarding runs on
// 5353; the DNS service still gets its unicast queries.
func TestAdvertiseAndBrowseBesideDNS(t *testing.T) {
	ip := net.ParseIP(hosttest.LANIP(t)).To4()
	port := hosttest.FreeUDPPort(t)
	daemonStandIn(t, port)
	dnsSock, err := netplane.ListenUDP(context.Background(), "127.0.0.1", strconv.Itoa(port))
	if err != nil {
		t.Fatalf("DNS service beside the daemon: %v", err)
	}
	defer dnsSock.Close()

	advertise(t, mdns.Service{
		Instance: "office@desk", Type: "_gcpemu-test._tcp", Host: "gcpemu-1234",
		Port: 4511, Addrs: []net.IP{ip}, TXT: []string{"name=office", "id=1234"},
	}, port)

	var e *mdns.Entry
	for _, g := range browse(t, port) {
		if g.Instance == "office@desk" {
			e = &g
		}
	}
	if e == nil {
		t.Fatal("advertised service not browsed")
	}
	if e.Port != 4511 || !slices.ContainsFunc(e.Addrs, ip.Equal) {
		t.Fatalf("entry %+v, want port 4511 at %s", e, ip)
	}
	if e.TXT["name"] != "office" || e.TXT["id"] != "1234" {
		t.Fatalf("TXT %v", e.TXT)
	}

	// Unicast to the DNS service's address still reaches it.
	c, err := net.Dial("udp4", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	_ = dnsSock.SetReadDeadline(time.Now().Add(2 * time.Second))
	b := make([]byte, 16)
	n, _, err := dnsSock.ReadFrom(b)
	if err != nil || string(b[:n]) != "ping" {
		t.Fatalf("DNS service read %q, %v", b[:n], err)
	}
}

// A closed Responder is no longer browsed, and an unadvertised one never
// is.
func TestClosedIsNotBrowsed(t *testing.T) {
	ip := net.ParseIP(hosttest.LANIP(t)).To4()
	port := hosttest.FreeUDPPort(t)
	r := advertise(t, mdns.Service{Instance: "gone@desk", Type: "_gcpemu-test._tcp", Host: "gcpemu-gone", Port: 1, Addrs: []net.IP{ip}}, port)
	r.Close()
	for _, g := range browse(t, port) {
		if g.Instance == "gone@desk" {
			t.Fatalf("closed Responder browsed: %+v", g)
		}
	}
}

// testLog logs at Debug to t until the test ends, so that a failure shows
// what the responder and the browser saw.
func testLog(t *testing.T) *slog.Logger {
	t.Helper()
	w := &testWriter{t: t}
	t.Cleanup(func() {
		w.mu.Lock()
		w.done = true
		w.mu.Unlock()
	})
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// testWriter writes to t.Log until done.
type testWriter struct {
	mu   sync.Mutex
	t    *testing.T
	done bool
}

func (w *testWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.done {
		w.t.Log(strings.TrimSpace(string(p)))
	}
	return len(p), nil
}
