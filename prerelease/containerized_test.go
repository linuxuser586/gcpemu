//go:build prerelease

package prerelease

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/hosttest"
	"github.com/linuxuser586/gcpemu/internal/porttest"
)

// The Containerized Instance's container, the ports it publishes and the
// image built from the commit under test.
const (
	container = "gcpemu-prerelease"
	portRange = "20000-20019"
	rcImage   = "gcpemu:rc"
)

// release builds the release binaries once for every runtime: the image
// each runtime builds from them lives in that runtime's own store.
var release = sync.OnceValue(func() error {
	cmd := exec.Command("task", "release")
	cmd.Dir = repo
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
})

// containerizedChecks runs the Windows checks on runtime rt: the release's
// image as a Containerized Instance, with the runtime's socket and a port
// range and the registry's port, reached by the host's clients through
// its published ports.
// Then the Resolver routes check runs an Instance on the host, gcpemu.exe.
func containerizedChecks(t *testing.T, rt string) {
	done := setup(t, rt)
	// task and go build gcpemu.exe for the Resolver routes check.
	requireTools(t, "docker", "task", "go", "gcloud", "kubectl", "cloud-sql-proxy")
	requireBin(t)
	useRuntime(t, rt)
	// A dev build's Sidecar agent, for this runtime's architecture: the
	// relays of checks 7 and 8 run it, and gcpemu.exe finds it in bin.
	task(t, context.Background(), "agent")
	image := releaseImage(t)
	gw := hosttest.RunEmulator(t, container, image, "4562", containerArgs(t, rt, portRange, registryPort), "--port-range", portRange)
	t.Logf("API gateway at %s", gw)
	done()

	if !step(t, rt, "1-start", 5*time.Minute, startChecks) {
		t.FailNow() // the steps below need the addresses it checks
	}
	step(t, rt, "2-sql", 30*time.Minute, containerizedSQL)
	step(t, rt, "3-gke-loadbalancer", 30*time.Minute, containerizedLoadBalancer)
	step(t, rt, "4-forwarding-rule", 15*time.Minute, forwardingRule)
	// Not in the container: the Host attachment links the host's own
	// loopback with the VPC, so it needs gcpemu running on the host.
	step(t, rt, "5-host-attachment", 30*time.Minute, hostAttachment)
	// An Instance on the host, gcpemu.exe, with dns on port 53,
	// since NRPT can't name a port (ADR 0015, ADR 0026).
	step(t, rt, "6-resolver-routes", 30*time.Minute, resolverRoutesStep)
	step(t, rt, "7-vm-relay", 15*time.Minute, vmRelay)
	// Like checks 5 to 7, on the host.
	step(t, rt, "8-registry-relay", 15*time.Minute, func(t *testing.T, ctx context.Context) { registryRelay(t, ctx, rt) })
	step(t, rt, "9-containerized-registry", 15*time.Minute, func(t *testing.T, ctx context.Context) { containerizedRegistry(t, ctx, rt) })
}

// containerArgs removes a leftover container and returns the docker run
// args of the Containerized Instance on runtime rt: the runtime's socket,
// and ports (a port or a range) each published at the same host port.
func containerArgs(t *testing.T, rt string, ports ...string) []string {
	t.Helper()
	_, _ = exec.Command("docker", "rm", "-f", container).CombinedOutput()
	args := hosttest.SocketArgs(t)
	for _, p := range ports {
		args = append(args, "-p", "127.0.0.1:"+p+":"+p)
	}
	if strings.Contains(rt, "podman") {
		args = append(args, "-e", "GCPEMU_VM_RUNTIME=true")
	}
	return args
}

// releaseImage returns IMAGE, or the image this runtime builds from the
// release binaries.
func releaseImage(t *testing.T) string {
	t.Helper()
	if image := os.Getenv(imageEnv); image != "" {
		return image
	}
	requireTools(t, "task", "pnpm")
	if err := release(); err != nil {
		t.Fatalf("task release: %v", err)
	}
	arch := hosttest.Docker(t, "version", "--format", "{{.Server.Arch}}")
	hosttest.Docker(t, "build", "-q", "--provenance=false", "--output", "type=docker,oci-mediatypes=false", "--build-arg", "TARGETARCH="+arch, "-t", rcImage, "-f", filepath.Join(repo, "Dockerfile"), filepath.Join(repo, "dist"))
	return rcImage
}

// containerEnv returns what `gcpemu env --shell powershell` prints in the
// container now.
func containerEnv(t *testing.T) string {
	t.Helper()
	return hosttest.Docker(t, "exec", container, "/gcpemu", "env", "--shell", "powershell")
}

// containerClients points the host's clients at the container, as
// Invoke-Expression on `gcpemu env` would. KUBECONFIG stays the clients'
// own: env's is a path inside the container.
func containerClients(t *testing.T) *clients {
	t.Helper()
	c := newClients(t)
	c.load(hosttest.ParseEnv(containerEnv(t)))
	return c
}

// startChecks is Windows step 1: doctor sees a VM runtime and uses the
// loopback aliases, and the API gateway env prints is one the host
// reaches.
func startChecks(t *testing.T, ctx context.Context) {
	out, _ := exec.CommandContext(ctx, "docker", "exec", container, "/gcpemu", "doctor").CombinedOutput()
	requireDoctor(t, string(out), "[ok  ] VM runtime: yes (", "[ok  ] loopback aliases for Cloud SQL: yes (")
	c := containerClients(t)
	gw := c.vars["GCPEMU_GATEWAY"]
	if gw == "" {
		t.Fatalf("gcpemu env prints no GCPEMU_GATEWAY:\n%s", containerEnv(t))
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get("http://" + gw + "/")
	if err != nil {
		t.Fatalf("the API gateway at %s, from gcpemu env, does not answer the host: %v", gw, err)
	}
	resp.Body.Close()
}

// containerizedSQL is Windows step 2: a Cloud SQL instance reached from
// the host through the Cloud SQL Auth Proxy, and straight at its own
// loopback alias on 5432 while 5432 is free on the host.
func containerizedSQL(t *testing.T, ctx context.Context) {
	l, err := net.Listen("tcp", "127.0.0.1:5432")
	free := err == nil
	if free {
		l.Close()
	}
	c := containerClients(t)
	c.run(t, ctx, "gcloud", append([]string{"sql", "instances", "create", "one"}, sqlFlags...)...)
	c.run(t, ctx, "gcloud", "sql", "users", "set-password", "postgres", "--instance=one", "--password=pw")
	var in struct {
		ConnectionName string
		IpAddresses    []struct{ Type, IpAddress string }
	}
	if err := json.Unmarshal([]byte(c.run(t, ctx, "gcloud", "sql", "instances", "describe", "one", "--format=json")), &in); err != nil {
		t.Fatal(err)
	}

	t.Run("auth-proxy", func(t *testing.T) {
		// The proxy is another process and binds --port itself, so the
		// test cannot hold the port for it: another process's explicit
		// bind can still take it first (#743).
		port := porttest.Free(t)
		// The proxy's static --token is in Google's universe.
		cmd := exec.CommandContext(ctx, "cloud-sql-proxy", "--sqladmin-api-endpoint", "http://"+c.vars["GCPEMU_GATEWAY"]+"/",
			"--token", "owner", "--port", strconv.Itoa(port), in.ConnectionName)
		cmd.Env = c.environ("GOOGLE_CLOUD_UNIVERSE_DOMAIN")
		var log syncBuffer
		cmd.Stdout, cmd.Stderr = &log, &log
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			if t.Failed() {
				t.Logf("cloud-sql-proxy log:\n%s", log.String())
			}
		}()
		hosttest.Eventually(t, 30*time.Second, "cloud-sql-proxy to start", func() bool {
			return strings.Contains(log.String(), "ready for new connections")
		}, log.String)
		selectOne(t, ctx, pgConnect(t, ctx, fmt.Sprintf("127.0.0.1:%d", port)), "the Auth Proxy")
	})

	var primary string
	for _, a := range in.IpAddresses {
		if a.Type == "PRIMARY" {
			primary = a.IpAddress
		}
	}
	if !loopbackAlias(primary) {
		t.Fatalf("instance one's PRIMARY address is %q, want a 127.0.0.x of its own", primary)
	}
	t.Run("direct-5432", func(t *testing.T) {
		if !free {
			t.Skip("5432 is taken on the host")
		}
		selectOne(t, ctx, pgConnect(t, ctx, net.JoinHostPort(primary, "5432")), "PRIMARY "+primary)
	})
}

// containerizedLoadBalancer is Windows step 3: a GKE LoadBalancer Service
// reached from the host on a port of the published range.
func containerizedLoadBalancer(t *testing.T, ctx context.Context) {
	c := containerClients(t)
	c.run(t, ctx, "gcloud", "container", "clusters", "create", "lb", "--zone="+zone, "--num-nodes=1")
	c.kubeconfig(t, "lb")
	if ip := c.lbService(t, ctx); ip != "127.0.0.1" {
		t.Fatalf("Service web's EXTERNAL-IP is %s, want 127.0.0.1", ip)
	}
	addr := hosttest.EnvValue(t, func() string { return containerEnv(t) }, "GCPEMU_GKE_LB_DEFAULT_WEB_80")
	if !inRange(addr, portRange) {
		t.Fatalf("GCPEMU_GKE_LB_DEFAULT_WEB_80=%s, want a port of the published range %s", addr, portRange)
	}
	getBody(t, "http://"+addr+"/", "Welcome to nginx")
}

// forwardingRule is Windows step 4: a forwarding rule in front of a
// backend bucket, reached from the host on a port of the published range.
func forwardingRule(t *testing.T, ctx context.Context) {
	c := containerClients(t)
	if err := os.WriteFile(filepath.Join(c.dir, "index.html"), []byte("lb ok\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"storage", "buckets", "create", "gs://lb-site", "--location=us-central1"},
		{"storage", "cp", "index.html", "gs://lb-site/index.html"},
		{"compute", "backend-buckets", "create", "site", "--gcs-bucket-name=lb-site"},
		{"compute", "url-maps", "create", "site", "--default-backend-bucket=site"},
		{"compute", "target-http-proxies", "create", "site", "--url-map=site"},
		{"compute", "forwarding-rules", "create", "site", "--global", "--target-http-proxy=site", "--ports=8080"},
	} {
		c.run(t, ctx, "gcloud", args...)
	}
	addr := hosttest.EnvValue(t, func() string { return containerEnv(t) }, "GCPEMU_LB_SITE")
	if host, _, _ := net.SplitHostPort(addr); host != "127.0.0.1" || !inRange(addr, portRange) {
		t.Fatalf("GCPEMU_LB_SITE=%s, want 127.0.0.1 and a port of the published range %s", addr, portRange)
	}
	if body := getBody(t, "http://"+addr+"/index.html", "lb ok"); strings.TrimSpace(body) != "lb ok" {
		t.Fatalf("index.html is %q, want lb ok", body)
	}
}

// syncBuffer is a strings.Builder a process writes while the test reads.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
