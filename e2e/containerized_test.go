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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/hosttest"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// TestContainerizedLBIngress (#276, #280): a Containerized Instance, the
// OCI image run with the host's Docker socket (ADR 0003), gives each GKE
// LoadBalancer Service an ingress the host reaches.
//
// Off a VM runtime that is a node address on the Service port:
// servicelb's on an ordinary network, and the Ready nodes' Fallback-range
// addresses on a Translated network, which `gcpemu env` prints too. On a
// VM runtime (ADR 0012) it is a port the emulator's container publishes,
// at 127.0.0.1: the Service's own port when published at the same host
// port, otherwise one of --port-range, and `gcpemu env` prints which. A
// Service with no free published port has no ingress. The "auto" case
// runs as the runtime's info says, except where off Linux the info names
// no VM runtime (Colima, Podman machine): every runtime there is one, and
// ADR 0012 has it set GCPEMU_VM_RUNTIME=true, which "forced" does. "forced"
// runs only where "auto" is not on a VM runtime already.
//
// It builds the image from the repository's Dockerfile, whose base image
// comes from the internet, so it runs only with GCPEMU_NET_TESTS=1.
func TestContainerizedLBIngress(t *testing.T) {
	emutest.RequireRuntime(t)
	if os.Getenv("GCPEMU_NET_TESTS") != "1" {
		t.Skip("GCPEMU_NET_TESTS != 1: building the image pulls its base image")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not on PATH")
	}
	socket := hosttest.SocketArgs(t)
	// The default VPC goes on a Fallback range; plain's range stays its own.
	decoyNetwork(t, "10.128.0.0/20")

	name := fmt.Sprintf("gcpemu-e2e-containerized-%d", time.Now().UnixNano())
	image := name + ":test"
	buildEmulatorImage(t, image)
	info := strings.SplitN(hosttest.Docker(t, "info", "--format", "{{.OperatingSystem}}\n{{.KernelVersion}}")+"\n", "\n", 3)
	auto := runtime.DecideVM("auto", true, runtime.Info{OS: info[0], Kernel: info[1]})
	t.Logf("runtime %q, kernel %q: VM runtime %s", info[0], info[1], auto)

	t.Run("auto", func(t *testing.T) {
		if !auto.On && goruntime.GOOS != "linux" {
			t.Skipf("VM runtime %s, but on %s every runtime is one, whose node addresses the host can't reach: the forced case covers it", auto, goruntime.GOOS)
		}
		containerizedLBIngress(t, name+"-auto", image, socket, auto.On, nil)
	})
	t.Run("forced", func(t *testing.T) {
		if auto.On {
			t.Skip("the auto case ran on a VM runtime already")
		}
		containerizedLBIngress(t, name+"-forced", image, socket, true, []string{"-e", "GCPEMU_VM_RUNTIME=true"})
	})
}

// On a VM runtime the emulator's container publishes two ports of
// --port-range, one for web on each cluster, and port 18081, which
// Service own on plain takes; full on trans then finds none free. The
// frontend's dynamic port stays outside the range on a VM runtime.
const (
	vmRange   = "20400-20401"
	vmOwnPort = "18081"
)

// containerizedLBIngress runs the image as container name, with env, and
// checks the ingress of LoadBalancer Services on an ordinary and a
// Translated network: on published ports when vm, otherwise at node
// addresses.
func containerizedLBIngress(t *testing.T, name, image string, socket []string, vm bool, env []string) {
	const plainRange = "10.213.0.0/20"
	args := append(append([]string{}, socket...), env...)
	var start []string
	if vm {
		args = append(args, "-p", "127.0.0.1:"+vmRange+":"+vmRange, "-p", "127.0.0.1:"+vmOwnPort+":"+vmOwnPort)
		start = append(start, "--port-range", vmRange)
	}
	gateway := hosttest.RunEmulator(t, name, image, "", args, start...)
	gcpemuEnv := func() string { return hosttest.Docker(t, "exec", name, "/gcpemu", "env") }
	if vm {
		if out := hosttest.Docker(t, "exec", name, "/gcpemu", "doctor"); !strings.Contains(out, "VM runtime: yes") || strings.Contains(out, "[WARN] VM runtime") {
			t.Errorf("gcpemu doctor does not report a VM runtime with published ports:\n%s", out)
		}
	}

	const project, region = "e2e-containerized", "us-central1"
	post(t, gateway, "/compute/v1/projects/"+project+"/global/networks", map[string]any{"name": "plain", "autoCreateSubnetworks": false})
	hosttest.Eventually(t, 30*time.Second, "network plain", func() bool { return getOK(gateway, "/compute/v1/projects/"+project+"/global/networks/plain") }, nil)
	post(t, gateway, "/compute/v1/projects/"+project+"/regions/"+region+"/subnetworks", map[string]any{
		"name": "plain", "network": "projects/" + project + "/global/networks/plain", "ipCidrRange": plainRange})
	hosttest.Eventually(t, 30*time.Second, "subnetwork plain", func() bool {
		return getOK(gateway, "/compute/v1/projects/"+project+"/regions/"+region+"/subnetworks/plain")
	}, nil)
	createCluster(t, gateway, project, map[string]any{"name": "plain", "network": "plain", "subnetwork": "plain",
		"nodePools": []map[string]any{{"name": "np", "initialNodeCount": 1}}})
	createCluster(t, gateway, project, map[string]any{"name": "trans",
		"nodePools": []map[string]any{{"name": "np", "initialNodeCount": 2}}})

	for _, tc := range []struct {
		cluster    string
		translated bool
	}{{"plain", false}, {"trans", true}} {
		t.Run(tc.cluster, func(t *testing.T) {
			k := clusterKube(t, gateway, project, tc.cluster)
			lbWeb(t, k)
			envKey := "GCPEMU_GKE_" + strings.ToUpper(tc.cluster) + "_DEFAULT_WEB_8081"
			if vm {
				ingress := lbIngress(t, k, "web", 2*time.Minute, func(ing []string) bool { return len(ing) > 0 })
				if len(ingress) != 1 || ingress[0] != "127.0.0.1" {
					t.Fatalf("ingress %v, want [127.0.0.1]", ingress)
				}
				addr := hosttest.EnvValue(t, gcpemuEnv, envKey)
				if _, p, _ := net.SplitHostPort(addr); !inRange(p, vmRange) {
					t.Errorf("%s=%s, want a port of --port-range %s", envKey, addr, vmRange)
				}
				getWeb(t, addr)
				if !tc.translated {
					lbService(k, "own", vmOwnPort)
					lbIngress(t, k, "own", 2*time.Minute, func(ing []string) bool { return len(ing) > 0 })
					addr := hosttest.EnvValue(t, gcpemuEnv, "GCPEMU_GKE_PLAIN_DEFAULT_OWN_"+vmOwnPort)
					if want := "127.0.0.1:" + vmOwnPort; addr != want {
						t.Errorf("Service own is at %s, want its own published port %s", addr, want)
					}
					getWeb(t, addr)
					return
				}
				// Every published port is taken now.
				lbService(k, "full", "8082")
				hosttest.Eventually(t, 30*time.Second, "a warning about Service full", func() bool {
					logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
					return bytes.Contains(logs, []byte("no free published port")) && bytes.Contains(logs, []byte("default/full"))
				}, nil)
				if ing := lbIngress(t, k, "full", 0, nil); len(ing) != 0 {
					t.Errorf("Service full has ingress %v with no free published port, want none", ing)
				}
				return
			}
			ingress := lbIngress(t, k, "web", 2*time.Minute, func(ing []string) bool {
				// Both nodes, unless it is wrong already.
				return len(ing) > 0 && (!tc.translated || len(ing) == 2 || net.ParseIP(ing[0]).IsLoopback())
			})
			t.Logf("ingress %v", ingress)
			for _, ip := range ingress {
				if net.ParseIP(ip).IsLoopback() {
					t.Fatalf("ingress %s is the emulator container's loopback, not the host's", ip)
				}
				if _, plain, _ := net.ParseCIDR(plainRange); !tc.translated && !plain.Contains(net.ParseIP(ip)) {
					t.Errorf("ingress %s is not on %s: is the range held on the host?", ip, plainRange)
				}
				getWeb(t, net.JoinHostPort(ip, "8081"))
			}
			if tc.translated {
				want := envKey + "='" + net.JoinHostPort(ingress[0], "8081") + "'"
				hosttest.Eventually(t, 10*time.Second, "gcpemu env prints "+want, func() bool {
					return strings.Contains(gcpemuEnv(), want)
				}, gcpemuEnv)
			}
		})
	}
}

// gkeZone is the zone of the clusters the containerized tests create.
const gkeZone = "us-central1-a"

// post POSTs body as JSON to path on the API gateway, and fails unless it
// answers 200.
func post(t *testing.T, gateway, path string, body any) {
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

// createCluster starts creating GKE cluster c in project, in gkeZone.
func createCluster(t *testing.T, gateway, project string, c map[string]any) {
	t.Helper()
	post(t, gateway, "/container/v1/projects/"+project+"/locations/"+gkeZone+"/clusters", map[string]any{"cluster": c})
}

// clusterKube waits for cluster name of project to be RUNNING, and returns
// a client of its kube API.
func clusterKube(t *testing.T, gateway, project, name string) *kube {
	t.Helper()
	cluster := "projects/" + project + "/locations/" + gkeZone + "/clusters/" + name
	hosttest.Eventually(t, 5*time.Minute, "cluster "+name+" RUNNING", func() bool {
		var c struct{ Status string }
		return getJSON(gateway, "/container/v1/"+cluster, &c) && c.Status == "RUNNING"
	}, nil)
	return newKube(t, gateway, cluster)
}

// lbIngress returns the ingress IPs of LoadBalancer Service default/name
// once done accepts them, or as they are when timeout is 0.
func lbIngress(t *testing.T, k *kube, name string, timeout time.Duration, done func([]string) bool) []string {
	t.Helper()
	var ingress []string
	read := func() bool {
		var svc struct {
			Status struct {
				LoadBalancer struct{ Ingress []struct{ IP string } }
			}
		}
		ingress = ingress[:0]
		if k.get(objPath("Service", "default", name), &svc) {
			for _, i := range svc.Status.LoadBalancer.Ingress {
				ingress = append(ingress, i.IP)
			}
		}
		return done == nil || done(ingress)
	}
	if timeout == 0 {
		read()
		return ingress
	}
	hosttest.Eventually(t, timeout, "LoadBalancer ingress of "+name, read, nil)
	return ingress
}

// lbService adds LoadBalancer Service default/name on port, in front of
// lbWeb's Deployment.
func lbService(k *kube, name, port string) {
	p, _ := strconv.Atoi(port)
	k.apply(map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"name": name, "namespace": "default"},
		"spec": map[string]any{"type": "LoadBalancer", "selector": map[string]string{"app": "web"},
			"ports": []map[string]any{{"port": p, "targetPort": 8080}}},
	})
}

// getWeb waits for lbWeb's server to answer "ok" at addr from the host.
func getWeb(t *testing.T, addr string) {
	t.Helper()
	hosttest.Eventually(t, 60*time.Second, "web reachable from the host at "+addr, func() bool {
		resp, err := (&http.Client{Timeout: 3 * time.Second}).Get("http://" + addr + "/")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return strings.TrimSpace(string(b)) == "ok"
	}, nil)
}

// inRange reports whether port lies in "LO-HI".
func inRange(port, r string) bool {
	lo, hi, _ := strings.Cut(r, "-")
	p, _ := strconv.Atoi(port)
	l, _ := strconv.Atoi(lo)
	h, _ := strconv.Atoi(hi)
	return p >= l && p <= h
}

// buildEmulatorImage builds the repository's Dockerfile around a static
// gcpemu for the runtime's architecture, and removes it at cleanup. The
// image is loaded into the runtime, which Podman's docker-container builder
// does not do by default, with Docker media types, since Podman drops the
// HEALTHCHECK of an OCI image.
func buildEmulatorImage(t *testing.T, tag string) {
	t.Helper()
	arch := hosttest.Docker(t, "version", "--format", "{{.Server.Arch}}")
	dir := t.TempDir()
	cmd := exec.Command("go", "build", "-trimpath", "-o", filepath.Join(dir, "gcpemu-linux-"+arch), "github.com/linuxuser586/gcpemu/cmd/gcpemu")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build gcpemu (%s): %v\n%s", arch, err, out)
	}
	hosttest.Docker(t, "build", "-q", "--provenance=false", "--output", "type=docker,oci-mediatypes=false", "--build-arg", "TARGETARCH="+arch, "-t", tag, "-f", "../Dockerfile", dir)
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
	hosttest.Eventually(t, 3*time.Minute, "deployment web ready", func() bool { return k.deploymentReady("default", "web") }, nil)
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
