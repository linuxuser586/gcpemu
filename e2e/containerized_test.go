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
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/emutest"
)

// TestContainerizedLBIngress (#276): a Containerized Instance, the OCI
// image run with the host's Docker socket (ADR 0003), gives each GKE
// LoadBalancer Service an ingress the host reaches on the Service port:
// servicelb's node address on an ordinary network, and the Ready nodes'
// Fallback-range addresses on a Translated network, which `gcpemu env`
// prints too. VM runtimes are #280.
//
// It builds the image from the repository's Dockerfile, whose base image
// comes from the internet, so it runs only with GCPEMU_NET_TESTS=1.
func TestContainerizedLBIngress(t *testing.T) {
	emutest.RequireRuntime(t)
	if goruntime.GOOS != "linux" {
		t.Skip("the host routes to container networks only on Linux (VM runtimes: #280)")
	}
	if os.Getenv("GCPEMU_NET_TESTS") != "1" {
		t.Skip("GCPEMU_NET_TESTS != 1: building the image pulls its base image")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not on PATH")
	}
	const sock = "/var/run/docker.sock"
	if _, err := os.Stat(sock); err != nil {
		t.Skipf("no Docker socket: %v", err)
	}
	// The default VPC goes on a Fallback range; plain's range stays its own.
	decoyNetwork(t, "10.128.0.0/20")
	const plainRange = "10.213.0.0/20"

	name := fmt.Sprintf("gcpemu-e2e-containerized-%d", time.Now().UnixNano())
	image := name + ":test"
	buildEmulatorImage(t, image)
	gid, err := exec.Command("stat", "-c", "%g", sock).Output()
	if err != nil {
		t.Fatalf("stat %s: %v", sock, err)
	}
	docker(t, "run", "-d", "--name", name, "-p", "127.0.0.1::4510", "-v", sock+":"+sock,
		"--group-add", strings.TrimSpace(string(gid)), image, "start", "--ephemeral")
	t.Cleanup(func() {
		if t.Failed() {
			out, _ := exec.Command("docker", "logs", "--tail", "60", name).CombinedOutput()
			t.Logf("emulator logs:\n%s", out)
		}
		// An Ephemeral Instance removes its containers and networks on stop.
		_, _ = exec.Command("docker", "stop", "-t", "60", name).CombinedOutput()
		_, _ = exec.Command("docker", "rm", "-f", name).CombinedOutput()
	})
	eventually(t, 3*time.Minute, "emulator healthy", func() bool {
		return docker(t, "inspect", "-f", "{{.State.Health.Status}}", name) == "healthy"
	}, nil)
	gateway := strings.TrimSpace(strings.Split(docker(t, "port", name, "4510/tcp"), "\n")[0])

	const project, region, zone = "e2e-containerized", "us-central1", "us-central1-a"
	post := func(path string, body any) {
		t.Helper()
		b, _ := json.Marshal(body)
		resp, err := http.Post("http://"+gateway+path, "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if out, _ := io.ReadAll(resp.Body); resp.StatusCode != http.StatusOK {
			t.Fatalf("POST %s: %d %s", path, resp.StatusCode, out)
		}
	}
	post("/compute/v1/projects/"+project+"/global/networks", map[string]any{"name": "plain", "autoCreateSubnetworks": false})
	eventually(t, 30*time.Second, "network plain", func() bool { return getOK(gateway, "/compute/v1/projects/"+project+"/global/networks/plain") }, nil)
	post("/compute/v1/projects/"+project+"/regions/"+region+"/subnetworks", map[string]any{
		"name": "plain", "network": "projects/" + project + "/global/networks/plain", "ipCidrRange": plainRange})
	eventually(t, 30*time.Second, "subnetwork plain", func() bool {
		return getOK(gateway, "/compute/v1/projects/"+project+"/regions/"+region+"/subnetworks/plain")
	}, nil)
	clusters := "/container/v1/projects/" + project + "/locations/" + zone + "/clusters"
	post(clusters, map[string]any{"cluster": map[string]any{"name": "plain", "network": "plain", "subnetwork": "plain",
		"nodePools": []map[string]any{{"name": "np", "initialNodeCount": 1}}}})
	post(clusters, map[string]any{"cluster": map[string]any{"name": "trans",
		"nodePools": []map[string]any{{"name": "np", "initialNodeCount": 2}}}})

	for _, tc := range []struct {
		cluster    string
		translated bool
	}{{"plain", false}, {"trans", true}} {
		t.Run(tc.cluster, func(t *testing.T) {
			eventually(t, 5*time.Minute, "cluster "+tc.cluster+" RUNNING", func() bool {
				var c struct{ Status string }
				return getJSON(gateway, clusters+"/"+tc.cluster, &c) && c.Status == "RUNNING"
			}, nil)
			k := newKube(t, gateway, "projects/"+project+"/locations/"+zone+"/clusters/"+tc.cluster)
			lbWeb(t, k)
			var ingress []string
			eventually(t, 2*time.Minute, "LoadBalancer ingress", func() bool {
				var svc struct {
					Status struct {
						LoadBalancer struct{ Ingress []struct{ IP string } }
					}
				}
				ingress = ingress[:0]
				if k.get(objPath("Service", "default", "web"), &svc) {
					for _, i := range svc.Status.LoadBalancer.Ingress {
						ingress = append(ingress, i.IP)
					}
				}
				// Both nodes, unless it is wrong already.
				return len(ingress) > 0 && (!tc.translated || len(ingress) == 2 || net.ParseIP(ingress[0]).IsLoopback())
			}, nil)
			t.Logf("ingress %v", ingress)
			for _, ip := range ingress {
				if net.ParseIP(ip).IsLoopback() {
					t.Fatalf("ingress %s is the emulator container's loopback, not the host's", ip)
				}
				if _, plain, _ := net.ParseCIDR(plainRange); !tc.translated && !plain.Contains(net.ParseIP(ip)) {
					t.Errorf("ingress %s is not on %s: is the range held on the host?", ip, plainRange)
				}
				addr := net.JoinHostPort(ip, "8081")
				eventually(t, 60*time.Second, "web reachable from the host at "+addr, func() bool {
					resp, err := (&http.Client{Timeout: 3 * time.Second}).Get("http://" + addr + "/")
					if err != nil {
						return false
					}
					defer resp.Body.Close()
					b, _ := io.ReadAll(resp.Body)
					return strings.TrimSpace(string(b)) == "ok"
				}, nil)
			}
			if tc.translated {
				want := "GCPEMU_GKE_TRANS_DEFAULT_WEB_8081='" + net.JoinHostPort(ingress[0], "8081") + "'"
				eventually(t, 10*time.Second, "gcpemu env prints "+want, func() bool {
					return strings.Contains(docker(t, "exec", name, "/gcpemu", "env"), want)
				}, func() string { return docker(t, "exec", name, "/gcpemu", "env") })
			}
		})
	}
}

// buildEmulatorImage builds the repository's Dockerfile around a static
// gcpemu for the runtime's architecture, and removes it at cleanup.
func buildEmulatorImage(t *testing.T, tag string) {
	t.Helper()
	arch := docker(t, "version", "--format", "{{.Server.Arch}}")
	dir := t.TempDir()
	cmd := exec.Command("go", "build", "-trimpath", "-o", filepath.Join(dir, "gcpemu-linux-"+arch), "github.com/linuxuser586/gcpemu/cmd/gcpemu")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build gcpemu (%s): %v\n%s", arch, err, out)
	}
	docker(t, "build", "-q", "--build-arg", "TARGETARCH="+arch, "-t", tag, "-f", "../Dockerfile", dir)
	t.Cleanup(func() { _, _ = exec.Command("docker", "rmi", "-f", tag).CombinedOutput() })
}

// lbWeb deploys an HTTP server answering "ok" behind LoadBalancer Service
// default/web on port 8081, from an image the nodes have preloaded.
func lbWeb(t *testing.T, k *kube) {
	t.Helper()
	k.apply(map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "web", "namespace": "default"},
		"spec": map[string]any{
			"replicas": 1,
			"selector": map[string]any{"matchLabels": map[string]string{"app": "web"}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]string{"app": "web"}},
				"spec": map[string]any{"containers": []map[string]any{{
					"name": "web", "image": "rancher/klipper-lb:v0.4.17", "imagePullPolicy": "IfNotPresent",
					"command": []string{"sh", "-c", "while true; do echo -e 'HTTP/1.1 200 OK\\r\\n\\r\\nok' | nc -l -p 8080; done"},
				}}},
			},
		},
	})
	k.apply(map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"name": "web", "namespace": "default"},
		"spec": map[string]any{"type": "LoadBalancer", "selector": map[string]string{"app": "web"},
			"ports": []map[string]any{{"port": 8081, "targetPort": 8080}}},
	})
	eventually(t, 3*time.Minute, "deployment web ready", func() bool { return k.deploymentReady("default", "web") }, nil)
}

// docker runs the docker CLI and returns its trimmed output.
func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func getOK(gateway, path string) bool { return getJSON(gateway, path, nil) }

func getJSON(gateway, path string, out any) bool {
	resp, err := http.Get("http://" + gateway + path)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	if out == nil {
		return true
	}
	return json.NewDecoder(resp.Body).Decode(out) == nil
}
