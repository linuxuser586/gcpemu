//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/gateway"
	"github.com/linuxuser586/gcpemu/internal/hosttest"
)

// TestContainerizedForwardingRules (#289): in a Containerized Instance a
// forwarding rule's loopback listener is the container's, which `-p`
// never reaches, so each rule is also served on a port the container
// publishes (ADR 0012), and its GCPEMU_LB_* variables (ADR 0013) are the
// host side of it.
//
// With the runtime socket a rule takes its own port, published here at
// another host port, then a free port of --port-range; a rule with none
// free has no GCPEMU_LB_* variable, `gcpemu doctor` lists it, and it takes a
// port once one frees. The loopback listener inside the container keeps
// answering. Without the socket a rule listens on its own port, which is
// published at the same host port.
//
// It builds the image from the repository's Dockerfile, whose base image
// comes from the internet, so it runs only with GCPEMU_NET_TESTS=1.
func TestContainerizedForwardingRules(t *testing.T) {
	emutest.RequireRuntime(t)
	if os.Getenv("GCPEMU_NET_TESTS") != "1" {
		t.Skip("GCPEMU_NET_TESTS != 1: building the image pulls its base image")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not on PATH")
	}
	name := fmt.Sprintf("gcpemu-e2e-frules-%d", time.Now().UnixNano())
	image := name + ":test"
	buildEmulatorImage(t, image)

	t.Run("socket", func(t *testing.T) {
		const own, rng = "28080", "20410"
		gw := hosttest.RunEmulator(t, name+"-socket", image, "",
			append(hosttest.SocketArgs(t), "-p", "127.0.0.1:"+own+":8080", "-p", "127.0.0.1:"+rng+":"+rng),
			"--port-range", rng+"-"+rng)
		gcpemuEnv := func() string { return hosttest.Docker(t, "exec", name+"-socket", "/gcpemu", "env") }
		lb := newLBClient(t, gw)
		lb.rule("a")
		for _, v := range []string{"GCPEMU_LB_E2E_FRULES_GLOBAL_A", "GCPEMU_LB_A"} {
			if got := hosttest.EnvValue(t, gcpemuEnv, v); got != "127.0.0.1:"+own {
				t.Fatalf("%s=%s, want 127.0.0.1:%s", v, got, own)
			}
		}
		answers(t, "127.0.0.1:"+own)
		lb.rule("b")
		if got := hosttest.EnvValue(t, gcpemuEnv, "GCPEMU_LB_E2E_FRULES_GLOBAL_B"); got != "127.0.0.1:"+rng {
			t.Fatalf("GCPEMU_LB_E2E_FRULES_GLOBAL_B=%s, want 127.0.0.1:%s of --port-range", got, rng)
		}
		answers(t, "127.0.0.1:"+rng)

		// The loopback listener still answers inside the container. The
		// admin API needs the token beyond loopback.
		req, _ := http.NewRequest(http.MethodGet, "http://"+gw+"/_emu/v1/lb/listeners", nil)
		req.Header.Set(gateway.AdminTokenHeader, hosttest.Docker(t, "exec", name+"-socket", "/gcpemu", "admin", "token"))
		var ls struct {
			Listeners []struct {
				ForwardingRule, Listener string
			}
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		err = json.NewDecoder(resp.Body).Decode(&ls)
		resp.Body.Close()
		if err != nil || len(ls.Listeners) == 0 {
			t.Fatalf("listeners: %d, %v", resp.StatusCode, err)
		}
		hosttest.Docker(t, "run", "--rm", "--network", "container:"+name+"-socket", "busybox:1.38",
			"sh", "-c", "wget -S -O /dev/null http://"+ls.Listeners[0].Listener+"/ 2>&1 | grep -q 'HTTP/'")

		lb.rule("c")
		time.Sleep(2 * time.Second)
		if out := gcpemuEnv(); strings.Contains(out, "GCPEMU_LB_C=") || strings.Contains(out, "GCPEMU_LB_E2E_FRULES_GLOBAL_C=") {
			t.Fatalf("rule c has no free published port, but gcpemu env prints it:\n%s", out)
		}
		if out := hosttest.Docker(t, "exec", name+"-socket", "/gcpemu", "doctor"); !strings.Contains(out, "forwardingRules/c (port 8080) has no published port") {
			t.Errorf("gcpemu doctor does not list rule c:\n%s", out)
		}
		lb.delete("a")
		hosttest.Eventually(t, 30*time.Second, "rule c takes port "+own, func() bool {
			out := gcpemuEnv()
			return strings.Contains(out, "GCPEMU_LB_E2E_FRULES_GLOBAL_C='127.0.0.1:"+own+"'") && !strings.Contains(out, "GCPEMU_LB_A=")
		}, gcpemuEnv)
		answers(t, "127.0.0.1:"+own)
	})

	t.Run("no socket", func(t *testing.T) {
		if c, err := net.DialTimeout("tcp", "127.0.0.1:8080", time.Second); err == nil {
			c.Close()
			t.Skip("127.0.0.1:8080 is in use on the host")
		}
		gw := hosttest.RunEmulator(t, name+"-nosocket", image, "", []string{"-p", "127.0.0.1:8080:8080"})
		gcpemuEnv := func() string { return hosttest.Docker(t, "exec", name+"-nosocket", "/gcpemu", "env") }
		newLBClient(t, gw).rule("a")
		if got := hosttest.EnvValue(t, gcpemuEnv, "GCPEMU_LB_E2E_FRULES_GLOBAL_A"); got != "127.0.0.1:8080" {
			t.Fatalf("GCPEMU_LB_E2E_FRULES_GLOBAL_A=%s, want 127.0.0.1:8080", got)
		}
		answers(t, "127.0.0.1:8080")
	})
}

// TestContainerizedRuleServiceContention (#315): forwarding rules and GKE
// LoadBalancer Services take Published ports (ADR 0012) from the same
// --port-range, and the first to bind a port holds it. With one port to
// compete for, exactly one of a rule and a Service holds it at a time;
// the other has no Published port, and takes it once the holder goes.
//
// Services take Published ports only on a VM runtime, which
// GCPEMU_VM_RUNTIME=true forces. Neither the rule's nor the Service's
// Own port is published, so both can only take the range's port.
//
// It builds the image from the repository's Dockerfile, whose base image
// comes from the internet, so it runs only with GCPEMU_NET_TESTS=1.
func TestContainerizedRuleServiceContention(t *testing.T) {
	emutest.RequireRuntime(t)
	if os.Getenv("GCPEMU_NET_TESTS") != "1" {
		t.Skip("GCPEMU_NET_TESTS != 1: building the image pulls its base image")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not on PATH")
	}
	const port = "20420"
	const addr = "127.0.0.1:" + port
	name := fmt.Sprintf("gcpemu-e2e-contention-%d", time.Now().UnixNano())
	image := name + ":test"
	buildEmulatorImage(t, image)
	gw := hosttest.RunEmulator(t, name, image, "",
		append(hosttest.SocketArgs(t), "-e", "GCPEMU_VM_RUNTIME=true", "-p", addr+":"+port),
		"--port-range", port+"-"+port)
	gcpemuEnv := func() string { return hosttest.Docker(t, "exec", name, "/gcpemu", "env") }
	doctor := func() string { return hosttest.Docker(t, "exec", name, "/gcpemu", "doctor") }
	const svcKey = "GCPEMU_GKE_CONTEND_DEFAULT_WEB_8081"
	// holders reports which of rule and Service web gcpemu env gives the
	// port, and fails if it gives it to both.
	holders := func(rule string) (ruleHolds, svcHolds bool) {
		t.Helper()
		out := gcpemuEnv()
		ruleHolds = strings.Contains(out, "GCPEMU_LB_"+strings.ToUpper(rule)+"='"+addr+"'")
		svcHolds = strings.Contains(out, svcKey+"='"+addr+"'")
		if ruleHolds && svcHolds {
			t.Errorf("rule %s and Service web both hold %s:\n%s", rule, addr, out)
		}
		return ruleHolds, svcHolds
	}
	svcWarnings := func() int {
		logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
		n := 0
		for _, line := range strings.Split(string(logs), "\n") {
			if strings.Contains(line, "no free published port") && strings.Contains(line, "default/web") {
				n++
			}
		}
		return n
	}

	// Start the cluster first: it takes minutes, the rule does not.
	createCluster(t, gw, lbProject, map[string]any{"name": "contend",
		"nodePools": []map[string]any{{"name": "np", "initialNodeCount": 1}}})
	lb := newLBClient(t, gw)

	// 1. A rule takes the port.
	lb.rule("first")
	if got := hosttest.EnvValue(t, gcpemuEnv, "GCPEMU_LB_FIRST"); got != addr {
		t.Fatalf("GCPEMU_LB_FIRST=%s, want %s of --port-range", got, addr)
	}
	answers(t, addr)

	// 2. A Service finds it taken: no ingress, no Service endpoint, one
	// warning.
	k := clusterKube(t, gw, lbProject, "contend")
	lbWeb(t, k)
	hosttest.Eventually(t, 30*time.Second, "a warning about Service web", func() bool { return svcWarnings() > 0 }, nil)
	time.Sleep(3 * time.Second)
	if n := svcWarnings(); n != 1 {
		t.Errorf("%d no-free-published-port warnings for Service web, want 1", n)
	}
	if ing := lbIngress(t, k, "web", 0, nil); len(ing) != 0 {
		t.Errorf("Service web has ingress %v while rule first holds %s, want none", ing, addr)
	}
	if out := gcpemuEnv(); strings.Contains(out, svcKey+"=") {
		t.Errorf("Service web has no published port, but gcpemu env prints it:\n%s", out)
	}
	if ruleHolds, _ := holders("first"); !ruleHolds {
		t.Fatalf("rule first no longer holds %s:\n%s", addr, gcpemuEnv())
	}

	// 3. The rule goes; the Service takes the port.
	lb.delete("first")
	hosttest.Eventually(t, 30*time.Second, "Service web takes "+addr, func() bool {
		_, svcHolds := holders("first")
		return svcHolds
	}, gcpemuEnv)
	if ing := lbIngress(t, k, "web", 30*time.Second, func(ing []string) bool { return len(ing) > 0 }); len(ing) != 1 || ing[0] != "127.0.0.1" {
		t.Fatalf("ingress %v, want [127.0.0.1]", ing)
	}
	getWeb(t, addr)

	// 4. A new rule finds it taken: no GCPEMU_LB_SECOND, and doctor lists it.
	lb.rule("second")
	hosttest.Eventually(t, 30*time.Second, "gcpemu doctor lists rule second", func() bool {
		return strings.Contains(doctor(), "forwardingRules/second (port 8080) has no published port")
	}, doctor)
	if out := gcpemuEnv(); strings.Contains(out, "GCPEMU_LB_SECOND=") {
		t.Fatalf("rule second has no free published port, but gcpemu env prints it:\n%s", out)
	}
	if _, svcHolds := holders("second"); !svcHolds {
		t.Fatalf("Service web no longer holds %s:\n%s", addr, gcpemuEnv())
	}

	// 5. The Service goes; the rule takes the port.
	k.delete("Service", "default", "web")
	hosttest.Eventually(t, 30*time.Second, "rule second takes "+addr, func() bool {
		ruleHolds, _ := holders("second")
		return ruleHolds
	}, gcpemuEnv)
	// The rule's backend service has no backends, so it answers 503, where
	// Service web answered 200.
	hosttest.Eventually(t, 30*time.Second, "a 503 from rule second at "+addr, func() bool {
		resp, err := (&http.Client{Timeout: 3 * time.Second}).Get("http://" + addr + "/")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusServiceUnavailable
	}, nil)
}

// lbClient creates external Application Load Balancers through the
// emulator's compute API: a backend service with no backends, which
// answers 503, behind URL map m and target proxy p.
type lbClient struct {
	t  *testing.T
	gw string
}

const lbProject = "e2e-frules"

func newLBClient(t *testing.T, gw string) *lbClient {
	c := &lbClient{t: t, gw: gw}
	c.call(http.MethodPost, "backendServices", map[string]any{"name": "be", "loadBalancingScheme": "EXTERNAL_MANAGED", "protocol": "HTTP"})
	c.call(http.MethodPost, "urlMaps", map[string]any{"name": "m", "defaultService": "global/backendServices/be"})
	c.call(http.MethodPost, "targetHttpProxies", map[string]any{"name": "p", "urlMap": "global/urlMaps/m"})
	return c
}

// rule creates forwarding rule name on port 8080.
func (c *lbClient) rule(name string) {
	c.call(http.MethodPost, "forwardingRules", map[string]any{"name": name, "loadBalancingScheme": "EXTERNAL_MANAGED",
		"portRange": "8080", "target": "global/targetHttpProxies/p"})
}

func (c *lbClient) delete(name string) { c.call(http.MethodDelete, "forwardingRules/"+name, nil) }

func (c *lbClient) call(method, coll string, body any) {
	c.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://"+c.gw+"/compute/v1/projects/"+lbProject+"/global/"+coll, r)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out, _ := io.ReadAll(resp.Body); resp.StatusCode != http.StatusOK {
		c.t.Fatalf("%s %s: %d %s", method, coll, resp.StatusCode, out)
	}
}

// answers waits for an HTTP response from addr on the host: any status,
// rather than the empty reply of a port nothing listens on behind -p.
func answers(t *testing.T, addr string) {
	t.Helper()
	hosttest.Eventually(t, 30*time.Second, "an HTTP response from "+addr, func() bool {
		resp, err := (&http.Client{Timeout: 3 * time.Second}).Get("http://" + addr + "/")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return true
	}, nil)
}
