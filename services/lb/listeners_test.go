package lb_test

import (
	"io"
	"net"
	"net/http"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/services/lb"
)

// TestLoopbackInUse: when another process (a second Instance, whose rules
// hash alike) holds a forwarding rule's loopback address on its port, the
// rule moves to another loopback address on its own port rather than a
// high port that DNS cannot express (#193).
func TestLoopbackInUse(t *testing.T) {
	e := start(t)
	if svc, _ := e.inst.Env.Lookup("lb"); lb.SingleLoopback(svc) {
		t.Skip("127.0.0.1 is the only loopback address here (single-loopback mode)")
	}
	origin := newBackend(t)
	e.lanLB(origin)
	fr := func() {
		e.do(e.c.GlobalForwardingRules.Insert(proj, &computev1.ForwardingRule{Name: "a", LoadBalancingScheme: "EXTERNAL_MANAGED",
			IPAddress: "34.120.0.77", PortRange: "8080", Target: "global/targetHttpProxies/p"}).Do())
	}
	fr()
	first := e.inst.Endpoint("lb:a")
	if h, p, _ := net.SplitHostPort(first); p != "8080" || !net.ParseIP(h).IsLoopback() {
		t.Fatalf("listener %q", first)
	}
	e.do(e.c.GlobalForwardingRules.Delete(proj, "a").Do())
	var held net.Listener
	for deadline := time.Now().Add(5 * time.Second); held == nil; {
		var err error
		if held, err = net.Listen("tcp", first); err != nil {
			if time.Now().After(deadline) {
				t.Fatalf("holding %s: %v", first, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	defer held.Close()

	fr()
	moved := e.inst.Endpoint("lb:a")
	h, p, _ := net.SplitHostPort(moved)
	if moved == first || p != "8080" || !net.ParseIP(h).IsLoopback() {
		t.Fatalf("with %s taken the rule listens on %q, want another loopback address on 8080", first, moved)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + moved + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET via %s: %d %v", moved, resp.StatusCode, err)
	}
}

// twoRules creates forwarding rules a and b on port, one after the other,
// and checks that a has 127.0.0.1:port, b another address, that emulated
// DNS maps each rule's IP to its listener IP, and that both serve.
func (e *env) twoRules(port string) {
	e.t.Helper()
	svc, _ := e.inst.Env.Lookup("lb")
	for _, name := range []string{"a", "b"} {
		e.do(e.c.GlobalForwardingRules.Insert(proj, &computev1.ForwardingRule{Name: name, LoadBalancingScheme: "EXTERNAL_MANAGED",
			PortRange: port, Target: "global/targetHttpProxies/p"}).Do())
	}
	a, b := e.inst.Endpoint("lb:a"), e.inst.Endpoint("lb:b")
	if a != "127.0.0.1:"+port {
		e.t.Fatalf("rule a listens on %q, want 127.0.0.1:%s", a, port)
	}
	if h, _, _ := net.SplitHostPort(b); b == a || h != "127.0.0.1" {
		e.t.Fatalf("rule b listens on %q, want another 127.0.0.1 port", b)
	}
	cl := &http.Client{Timeout: 10 * time.Second}
	for _, name := range []string{"a", "b"} {
		fr, err := e.c.GlobalForwardingRules.Get(proj, name).Do()
		if err != nil {
			e.t.Fatal(err)
		}
		if got := lb.MappedIP(svc, fr.IPAddress); got != "127.0.0.1" {
			e.t.Errorf("DNS maps rule %s's %s to %q, want 127.0.0.1", name, fr.IPAddress, got)
		}
		addr := e.inst.Endpoint("lb:" + name)
		resp, body := get(e.t, cl, "http://"+addr+"/"+name)
		if resp.StatusCode != http.StatusOK || !strings.HasSuffix(body, "/"+name) {
			e.t.Errorf("GET via %s: %d %q", addr, resp.StatusCode, body)
		}
	}
}

// TestSingleLoopback: without loopback aliases (macOS, #172) every
// forwarding rule listens on 127.0.0.1; the first on a port gets it, a
// second on that port a fallback port.
func TestSingleLoopback(t *testing.T) {
	if c, err := net.DialTimeout("tcp", "127.0.0.1:8080", time.Second); err == nil {
		c.Close()
		t.Skip("127.0.0.1:8080 is in use")
	}
	e := start(t)
	svc, _ := e.inst.Env.Lookup("lb")
	lb.SetSingleLoopback(svc, true)
	e.lanLB(newBackend(t))
	e.twoRules("8080")
}

// TestPrivilegedWildcard: macOS lets an unprivileged process bind port
// 443 only on the wildcard address, so the first forwarding rule on it
// listens there, reported as 127.0.0.1:443, and serves the --lb-bind
// addresses on the port too; a second rule gets a fallback port.
func TestPrivilegedWildcard(t *testing.T) {
	if goruntime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	if c, err := net.DialTimeout("tcp", "127.0.0.1:443", time.Second); err == nil {
		c.Close()
		t.Skip("127.0.0.1:443 is in use")
	}
	lan := hostIP(t)
	e := startLBBind(t, lan)
	e.lanLB(newBackend(t))
	e.twoRules("443")
	want := net.JoinHostPort(lan, "443")
	if got := e.inst.Endpoint("lb-lan:a"); got != want {
		t.Fatalf("rule a on --lb-bind: %q, want %s", got, want)
	}
	if got := e.inst.Endpoint("lb-lan:b"); got != "" {
		t.Fatalf("rule b on --lb-bind: %q, want none", got)
	}
	resp, body := get(t, &http.Client{Timeout: 10 * time.Second}, "http://"+want+"/lan")
	if resp.StatusCode != http.StatusOK || !strings.HasSuffix(body, "/lan") {
		t.Fatalf("GET via %s: %d %q", want, resp.StatusCode, body)
	}
}

// hostIP returns a non-loopback IPv4 address of this host.
func hostIP(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && n.IP.IsGlobalUnicast() {
			return n.IP.String()
		}
	}
	t.Skip("no non-loopback IPv4 address")
	return ""
}
