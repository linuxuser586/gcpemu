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
)

// TestContainerizedForwardingRules (#289): in a Containerized Instance a
// forwarding rule's loopback listener is the container's, which `-p`
// never reaches, so each rule is also served on a port the container
// publishes (ADR 0012), and GCPEMU_LB_<NAME> is the host side of it.
//
// With the runtime socket a rule takes its own port, published here at
// another host port, then a free port of --port-range; a rule with none
// free has no GCPEMU_LB_<NAME>, `gcpemu doctor` lists it, and it takes a
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
		gid := socketGroup(t, "/var/run/docker.sock")
		gw := runEmulator(t, name+"-socket", image,
			[]string{"-v", "/var/run/docker.sock:/var/run/docker.sock", "--group-add", gid,
				"-p", "127.0.0.1:" + own + ":8080", "-p", "127.0.0.1:" + rng + ":" + rng},
			"--port-range", rng+"-"+rng)
		gcpemuEnv := func() string { return docker(t, "exec", name+"-socket", "/gcpemu", "env") }
		lb := newLBClient(t, gw)
		lb.rule("a")
		if got := envAddr(t, gcpemuEnv, "GCPEMU_LB_A"); got != "127.0.0.1:"+own {
			t.Fatalf("GCPEMU_LB_A=%s, want 127.0.0.1:%s", got, own)
		}
		answers(t, "127.0.0.1:"+own)
		lb.rule("b")
		if got := envAddr(t, gcpemuEnv, "GCPEMU_LB_B"); got != "127.0.0.1:"+rng {
			t.Fatalf("GCPEMU_LB_B=%s, want 127.0.0.1:%s of --port-range", got, rng)
		}
		answers(t, "127.0.0.1:"+rng)

		// The loopback listener still answers inside the container. The
		// admin API needs the token beyond loopback.
		req, _ := http.NewRequest(http.MethodGet, "http://"+gw+"/_emu/v1/lb/listeners", nil)
		req.Header.Set(gateway.AdminTokenHeader, docker(t, "exec", name+"-socket", "/gcpemu", "admin", "token"))
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
		docker(t, "run", "--rm", "--network", "container:"+name+"-socket", "busybox:1.37",
			"sh", "-c", "wget -S -O /dev/null http://"+ls.Listeners[0].Listener+"/ 2>&1 | grep -q 'HTTP/'")

		lb.rule("c")
		time.Sleep(2 * time.Second)
		if out := gcpemuEnv(); strings.Contains(out, "GCPEMU_LB_C=") {
			t.Fatalf("rule c has no free published port, but gcpemu env prints it:\n%s", out)
		}
		if out := docker(t, "exec", name+"-socket", "/gcpemu", "doctor"); !strings.Contains(out, "forwardingRules/c (port 8080) has no published port") {
			t.Errorf("gcpemu doctor does not list rule c:\n%s", out)
		}
		lb.delete("a")
		eventually(t, 30*time.Second, "rule c takes port "+own, func() bool {
			return strings.Contains(gcpemuEnv(), "GCPEMU_LB_C='127.0.0.1:"+own+"'")
		}, gcpemuEnv)
		answers(t, "127.0.0.1:"+own)
	})

	t.Run("no socket", func(t *testing.T) {
		if c, err := net.DialTimeout("tcp", "127.0.0.1:8080", time.Second); err == nil {
			c.Close()
			t.Skip("127.0.0.1:8080 is in use on the host")
		}
		gw := runEmulator(t, name+"-nosocket", image, []string{"-p", "127.0.0.1:8080:8080"})
		gcpemuEnv := func() string { return docker(t, "exec", name+"-nosocket", "/gcpemu", "env") }
		newLBClient(t, gw).rule("a")
		if got := envAddr(t, gcpemuEnv, "GCPEMU_LB_A"); got != "127.0.0.1:8080" {
			t.Fatalf("GCPEMU_LB_A=%s, want 127.0.0.1:8080", got)
		}
		answers(t, "127.0.0.1:8080")
	})
}

// runEmulator runs the image as an Ephemeral Instance in container name,
// with extra docker run args and start args, waits until it is healthy,
// and returns the host address of its API gateway.
func runEmulator(t *testing.T, name, image string, args []string, start ...string) string {
	t.Helper()
	run := append([]string{"run", "-d", "--name", name, "-p", "127.0.0.1::4510"}, args...)
	docker(t, append(append(run, image, "start", "--ephemeral"), start...)...)
	t.Cleanup(func() {
		if t.Failed() {
			out, _ := exec.Command("docker", "logs", "--tail", "80", name).CombinedOutput()
			t.Logf("emulator logs:\n%s", out)
		}
		_, _ = exec.Command("docker", "stop", "-t", "60", name).CombinedOutput()
		_, _ = exec.Command("docker", "rm", "-f", name).CombinedOutput()
	})
	eventually(t, 3*time.Minute, "emulator healthy", func() bool {
		return docker(t, "inspect", "-f", "{{.State.Health.Status}}", name) == "healthy"
	}, nil)
	return strings.TrimSpace(strings.Split(docker(t, "port", name, "4510/tcp"), "\n")[0])
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
	eventually(t, 30*time.Second, "an HTTP response from "+addr, func() bool {
		resp, err := (&http.Client{Timeout: 3 * time.Second}).Get("http://" + addr + "/")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return true
	}, nil)
}
