package lb_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/runtime"
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
	return startContainerized(t, portRange, func(svc emu.Service, bind string) { lb.SetPublished(svc, ports, bind) })
}

// startContainerized starts an Instance as startPublished does, with set
// making its data plane a Containerized Instance's, serving published
// ports on bind.
func startContainerized(t *testing.T, portRange string, set func(svc emu.Service, bind string)) (*env, func(port int) string) {
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
	set(svc, bind)
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
// port its container publishes, and its endpoint is the host side:
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
	e.waitEndpoint(ep("a"), "127.0.0.1:28080")
	e.serves(dial(8080))
	e.serves(net.JoinHostPort(lb.ListenerAddr(svc, "a"), "8080"))
	if h := e.unpublished("a"); h != "" {
		t.Errorf("rule a: hint %q, want none", h)
	}

	e.forwardingRule("b", 8080)
	if got := e.inst.Endpoint(ep("b")); got != "" {
		t.Fatalf("rule b, no port free: endpoint %q, want none", got)
	}
	if h := e.unpublished("b"); !strings.Contains(h, "--port-range "+strconv.Itoa(rng)) {
		t.Errorf("rule b: hint %q, want one naming the range", h)
	}
	other.Close()
	e.waitEndpoint(ep("b"), "127.0.0.1:40001")
	e.serves(dial(rng))
	if l, err := net.Listen("tcp", dial(rng)); err == nil {
		l.Close()
		t.Error("a plain listener took the range port rule b serves on")
	}

	e.forwardingRule("c", 8080)
	if got := e.inst.Endpoint(ep("c")); got != "" {
		t.Fatalf("rule c, no port free: endpoint %q, want none", got)
	}
	e.do(e.c.GlobalForwardingRules.Delete(proj, "a").Do())
	e.waitEndpoint(ep("a"), "")
	e.waitEndpoint(ep("c"), "127.0.0.1:28080")
	e.serves(dial(8080))
}

// Without the runtime socket the Instance cannot read its published
// ports: a rule listens on its own port and reports 127.0.0.1 there, and
// of two rules on one port the first is served until it goes.
func TestPublishedNoSocket(t *testing.T) {
	e, dial := startPublished(t, "", nil)
	e.forwardingRule("a", 8080)
	e.waitEndpoint(ep("a"), "127.0.0.1:8080")
	e.serves(dial(8080))
	e.forwardingRule("b", 8080)
	if got := e.inst.Endpoint(ep("b")); got != "" {
		t.Fatalf("rule b: endpoint %q, want none", got)
	}
	if h := e.unpublished("b"); !strings.Contains(h, "-p 127.0.0.1:8080:8080") {
		t.Errorf("rule b: hint %q", h)
	}
	e.do(e.c.GlobalForwardingRules.Delete(proj, "a").Do())
	e.waitEndpoint(ep("b"), "127.0.0.1:8080")
	e.serves(dial(8080))
}

// fakeRuntime stands in for the Instance's container runtime: each
// Runtime call takes the next of errs (nil once they run out) and is
// counted.
type fakeRuntime struct {
	mu    sync.Mutex
	errs  []error
	calls int
}

func (f *fakeRuntime) runtime(context.Context) (*runtime.Manager, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return nil, err
	}
	return &runtime.Manager{}, nil
}

func (f *fakeRuntime) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// A transient runtime failure on the first try leaves the Published ports
// unknown rather than deciding no-socket mode: rules are not served on
// published ports and the runtime is not asked again until the backoff
// ends. The next try, which does not wait for the edge's own backoff,
// reads the ports, and the waiting rule is served on its --port-range
// port (#313).
func TestPublishedRuntimeTransient(t *testing.T) {
	rng := freePort(t)
	rt := &fakeRuntime{errs: []error{errors.New("runtime socket not ready")}}
	e, dial := startContainerized(t, strconv.Itoa(rng)+"-"+strconv.Itoa(rng), func(svc emu.Service, bind string) {
		lb.SetSelfRuntime(svc, rt.runtime, func(context.Context) (string, bool, map[int]string, error) {
			return "self", true, map[int]string{rng: "127.0.0.1:40001"}, nil
		}, bind)
	})
	svc, _ := e.inst.Env.Lookup("lb")
	e.forwardingRule("a", 8080)
	if got := e.inst.Endpoint(ep("a")); got != "" {
		t.Fatalf("runtime not yet known: endpoint %q, want none", got)
	}
	time.Sleep(1500 * time.Millisecond) // the data plane retries each second
	if lb.RuntimeAvailable(svc) {
		t.Error("runtime available while it is down")
	}
	if n := rt.count(); n != 1 {
		t.Errorf("runtime tried %d times within the backoff, want 1", n)
	}
	if got := e.inst.Endpoint(ep("a")); got != "" {
		t.Fatalf("runtime not yet known: endpoint %q, want none", got)
	}

	lb.RetryPublishedNow(svc)
	e.waitEndpoint(ep("a"), "127.0.0.1:40001")
	e.serves(dial(rng))
	if !lb.RuntimeAvailable(svc) {
		t.Error("runtime not available once it answers")
	}
}

// A permanent runtime failure (no runtime at all) decides no-socket mode
// for good, as TestPublishedNoSocket: the runtime is not asked again.
func TestPublishedRuntimeNone(t *testing.T) {
	rt := &fakeRuntime{errs: []error{emu.NoRuntime(errors.New("no runtime found"))}}
	e, dial := startContainerized(t, "", func(svc emu.Service, bind string) {
		lb.SetSelfRuntime(svc, rt.runtime, func(context.Context) (string, bool, map[int]string, error) {
			t.Error("published ports read without a runtime")
			return "", false, nil, nil
		}, bind)
	})
	svc, _ := e.inst.Env.Lookup("lb")
	e.forwardingRule("a", 8080)
	e.waitEndpoint(ep("a"), "127.0.0.1:8080")
	e.serves(dial(8080))
	lb.RetryPublishedNow(svc)
	time.Sleep(1500 * time.Millisecond)
	if lb.RuntimeAvailable(svc) {
		t.Error("runtime available without one")
	}
	if n := rt.count(); n != 1 {
		t.Errorf("runtime tried %d times, want 1", n)
	}
	if got := e.inst.Endpoint(ep("a")); got != "127.0.0.1:8080" {
		t.Errorf("endpoint %q moved from no-socket mode's", got)
	}
}

// Without a runtime, --containerized=false declares a container on the
// host's network: no rule is served on a published port, and its
// endpoint is its loopback listener, as on the host (#502).
func TestPublishedNoRuntimeNotContainerized(t *testing.T) {
	rt := &fakeRuntime{errs: []error{emu.NoRuntime(errors.New("no runtime found"))}}
	e, _ := startContainerized(t, "", func(svc emu.Service, bind string) {
		lb.SetContainerized(svc, "false")
		lb.SetSelfRuntime(svc, rt.runtime, func(context.Context) (string, bool, map[int]string, error) {
			t.Error("published ports read without a runtime")
			return "", false, nil, nil
		}, bind)
	})
	svc, _ := e.inst.Env.Lookup("lb")
	e.forwardingRule("a", 8080)
	want := net.JoinHostPort(lb.ListenerAddr(svc, "a"), "8080")
	e.waitEndpoint(ep("a"), want)
	e.serves(want)
	if lb.Publishing(svc) {
		t.Error("forwarding rules served on published ports")
	}
	if h := e.unpublished("a"); h != "" {
		t.Errorf("rule a: hint %q, want none", h)
	}
}

// The runtime's answer wins over --containerized: the emulator's own
// bridge-network container is a Containerized Instance whatever it says
// (#502).
func TestPublishedRuntimeWinsOverSetting(t *testing.T) {
	rng := freePort(t)
	rt := &fakeRuntime{}
	e, dial := startContainerized(t, strconv.Itoa(rng)+"-"+strconv.Itoa(rng), func(svc emu.Service, bind string) {
		lb.SetContainerized(svc, "false")
		lb.SetSelfRuntime(svc, rt.runtime, func(context.Context) (string, bool, map[int]string, error) {
			return "self", true, map[int]string{rng: "127.0.0.1:40001"}, nil
		}, bind)
	})
	e.forwardingRule("a", 8080)
	e.waitEndpoint(ep("a"), "127.0.0.1:40001")
	e.serves(dial(rng))
}

// A runtime that can't find the emulator's own container is no answer:
// under auto the marker files make it a Containerized Instance, whose
// published ports are unknown, so rules are served as without the runtime
// socket (#502).
func TestPublishedSelfNotFound(t *testing.T) {
	rt := &fakeRuntime{}
	e, dial := startContainerized(t, "", func(svc emu.Service, bind string) {
		lb.SetSelfRuntime(svc, rt.runtime, func(context.Context) (string, bool, map[int]string, error) {
			return "", false, nil, nil
		}, bind)
	})
	e.forwardingRule("a", 8080)
	e.waitEndpoint(ep("a"), "127.0.0.1:8080")
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
