package lb_test

import (
	"net"
	"net/http"
	goruntime "runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/services/lb"
)

// startPublished starts an Instance whose data plane serves forwarding
// rules on Published ports as a Containerized Instance does (ADR 0012,
// #289), with ports the container's published ports (nil: no runtime
// socket), and returns the address the test dials for a container port.
// On Linux the published side listens on the wildcard address, as in a
// container; elsewhere on 127.0.0.1, or on ::1 in single-loopback mode,
// where the rules' loopback listeners hold 127.0.0.1.
func startPublished(t *testing.T, portRange string, ports map[int]string) (*env, func(port int) string) {
	t.Helper()
	if c, err := net.DialTimeout("tcp", "127.0.0.1:8080", time.Second); err == nil {
		c.Close()
		t.Skip("127.0.0.1:8080 is in use")
	}
	e := start(t)
	e.inst.Env.Config.PortRange = portRange
	svc, _ := e.inst.Env.Lookup("lb")
	host, bind := "127.0.0.1", "127.0.0.1"
	if goruntime.GOOS == "linux" {
		bind = ""
	}
	if lb.SingleLoopback(svc) {
		host, bind = "::1", "::1"
		if l, err := net.Listen("tcp", "[::1]:0"); err != nil {
			t.Skip("no ::1 to serve published ports on beside 127.0.0.1")
		} else {
			l.Close()
		}
	}
	lb.SetPublished(svc, ports, bind)
	e.lanLB(newBackend(t))
	return e, func(port int) string { return net.JoinHostPort(host, strconv.Itoa(port)) }
}

// waitEndpoint waits for endpoint name to become want.
func (e *env) waitEndpoint(name, want string) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for e.inst.Endpoint(name) != want {
		if time.Now().After(deadline) {
			e.t.Fatalf("endpoint %s = %q, want %q", name, e.inst.Endpoint(name), want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// serves fails the test unless addr answers HTTP through the rule.
func (e *env) serves(addr string) {
	e.t.Helper()
	resp, body := get(e.t, &http.Client{Timeout: 10 * time.Second}, "http://"+addr+"/pub")
	if resp.StatusCode != http.StatusOK || !strings.HasSuffix(body, "/pub") {
		e.t.Fatalf("GET via %s: %d %q", addr, resp.StatusCode, body)
	}
}

// unpublished returns the publish hint the admin API reports for rule name
// ("" when it has a published port).
func (e *env) unpublished(name string) string {
	for _, l := range e.rest(http.MethodGet, "/_emu/v1/lb/listeners?project="+proj, nil)["listeners"].([]any) {
		m := l.(map[string]any)
		if strings.HasSuffix(m["forwardingRule"].(string), "/"+name) {
			h, _ := m["unpublished"].(string)
			return h
		}
	}
	e.t.Fatalf("no listener for %s", name)
	return ""
}

// In a Containerized Instance each forwarding rule is also served on a
// port its container publishes, and GCPEMU_LB_<NAME> is the host side:
// the rule's own port first (here published at another host port), then
// a free port of --port-range. A rule with none free has no endpoint and
// a publish hint, and is served once a port frees, whether another rule
// or anything else (a GKE LoadBalancer Service) held it. The loopback
// listener keeps serving.
func TestPublishedPorts(t *testing.T) {
	rng := freePort(t)
	e, dial := startPublished(t, strconv.Itoa(rng)+"-"+strconv.Itoa(rng), map[int]string{
		8080: "127.0.0.1:28080", rng: "127.0.0.1:40001",
	})
	svc, _ := e.inst.Env.Lookup("lb")

	// Something else holds the range port at first.
	other, err := net.Listen("tcp", dial(rng))
	if err != nil {
		t.Fatal(err)
	}
	e.forwardingRule("a", 8080)
	e.waitEndpoint("lb:a", "127.0.0.1:28080")
	e.serves(dial(8080))
	e.serves(net.JoinHostPort(lb.ListenerAddr(svc, "a"), "8080"))
	if h := e.unpublished("a"); h != "" {
		t.Errorf("rule a: hint %q, want none", h)
	}

	e.forwardingRule("b", 8080)
	if got := e.inst.Endpoint("lb:b"); got != "" {
		t.Fatalf("rule b, no port free: endpoint %q, want none", got)
	}
	if h := e.unpublished("b"); !strings.Contains(h, "--port-range "+strconv.Itoa(rng)) {
		t.Errorf("rule b: hint %q, want one naming the range", h)
	}
	other.Close()
	e.waitEndpoint("lb:b", "127.0.0.1:40001")
	e.serves(dial(rng))
	if l, err := net.Listen("tcp", dial(rng)); err == nil {
		l.Close()
		t.Error("a plain listener took the range port rule b serves on")
	}

	e.forwardingRule("c", 8080)
	if got := e.inst.Endpoint("lb:c"); got != "" {
		t.Fatalf("rule c, no port free: endpoint %q, want none", got)
	}
	e.do(e.c.GlobalForwardingRules.Delete(proj, "a").Do())
	e.waitEndpoint("lb:a", "")
	e.waitEndpoint("lb:c", "127.0.0.1:28080")
	e.serves(dial(8080))
}

// Without the runtime socket the Instance cannot read its published
// ports: a rule listens on its own port and reports 127.0.0.1 there, and
// of two rules on one port the first is served until it goes.
func TestPublishedNoSocket(t *testing.T) {
	e, dial := startPublished(t, "", nil)
	e.forwardingRule("a", 8080)
	e.waitEndpoint("lb:a", "127.0.0.1:8080")
	e.serves(dial(8080))
	e.forwardingRule("b", 8080)
	if got := e.inst.Endpoint("lb:b"); got != "" {
		t.Fatalf("rule b: endpoint %q, want none", got)
	}
	if h := e.unpublished("b"); !strings.Contains(h, "-p 127.0.0.1:8080:8080") {
		t.Errorf("rule b: hint %q", h)
	}
	e.do(e.c.GlobalForwardingRules.Delete(proj, "a").Do())
	e.waitEndpoint("lb:b", "127.0.0.1:8080")
	e.serves(dial(8080))
}

// freePort returns a TCP port free on 127.0.0.1.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
