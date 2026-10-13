package lb_test

import (
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	computev1 "google.golang.org/api/compute/v1"
)

// TestLoopbackInUse: when another process (a second Instance, whose rules
// hash alike) holds a forwarding rule's loopback address on its port, the
// rule moves to another loopback address on its own port rather than a
// high port that DNS cannot express (#193).
func TestLoopbackInUse(t *testing.T) {
	e := start(t)
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
