//go:build prerelease

package prerelease

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/hosttest"
)

// relayImage is what the Registry relay check pushes and pulls.
const relayImage = "busybox:1.37"

// registryRelay is step 8 of both suites: the runtime's own push and pull
// of $GCPEMU_REGISTRY/... reach an Instance on the host through the
// Registry relay (ADR 0043). CI has no VM runtime, where the daemon pushes
// from inside its VM. Podman machine pushes with `podman push
// --tls-verify=false`, as Podman sends localhost registries HTTPS.
func registryRelay(t *testing.T, ctx context.Context, rt string) {
	requireTools(t, "docker", "gcloud")
	requireBin(t)
	h := startHost(t, ctx, "", "iam,ar")
	c := newClients(t)
	env := hosttest.ParseEnv(h.env(t))
	c.load(env)
	reg := env["GCPEMU_REGISTRY"]
	if reg == "" {
		t.Fatalf("gcpemu env sets no GCPEMU_REGISTRY:\n%s", h.env(t))
	}
	c.run(t, ctx, "gcloud", "artifacts", "repositories", "create", "relay", "--repository-format=docker", "--location=us-central1")
	waitRegistryRelay(t, ctx, h)

	pushPull(t, ctx, c, rt, reg+"/us-central1-docker.pkg.dev/"+project+"/relay/busybox:prerelease")
}

// pushPull pushes relayImage to ref with the runtime's own CLI, removes
// it and pulls it back.
func pushPull(t *testing.T, ctx context.Context, c *clients, rt, ref string) {
	t.Helper()
	cli, push := "docker", []string{"push", ref}
	pull := []string{"pull", ref}
	if tableRow(rt) == "Podman machine" {
		requireTools(t, "podman")
		cli, push = "podman", []string{"push", "--tls-verify=false", ref}
		pull = []string{"pull", "--tls-verify=false", ref}
	}
	c.run(t, ctx, cli, "pull", relayImage)
	c.run(t, ctx, cli, "tag", relayImage, ref)
	c.run(t, ctx, cli, push...)
	c.run(t, ctx, cli, "rmi", ref)
	t.Cleanup(func() { _ = exec.Command(cli, "rmi", ref).Run() })
	c.run(t, ctx, cli, pull...)
}

// registryPort is the registry's default port, which a Containerized
// Instance publishes at the same host port.
const registryPort = "4561"

// containerizedRegistryStep is macOS step 9: the release's image as a
// Containerized Instance with Artifact Registry, its API gateway and
// registry ports published at the same host ports, as `gcpemu env` in
// the container prints them, then containerizedRegistry.
func containerizedRegistryStep(t *testing.T, ctx context.Context, rt string) {
	requireTools(t, "docker", "gcloud")
	image := releaseImage(t)
	hosttest.RunEmulator(t, container, image, "4562", containerArgs(t, rt, registryPort), "--services", "iam,ar")
	containerizedRegistry(t, ctx, rt)
}

// containerizedRegistry is step 9 of both suites: the runtime's own push
// and pull of $GCPEMU_REGISTRY/... reach the registry of the Containerized
// Instance in container, whose Published registry port VM-side clients
// reach with no Registry relay (#835).
func containerizedRegistry(t *testing.T, ctx context.Context, rt string) {
	out, _ := exec.CommandContext(ctx, "docker", "exec", container, "/gcpemu", "doctor").CombinedOutput()
	want := []string{"[--  ] Registry relay: off (a Containerized Instance's Published registry port " + registryPort}
	if tableRow(rt) == "Podman machine" {
		want = append(want, "[WARN] Podman machine pushes to the registry over HTTPS by default")
	}
	requireDoctor(t, string(out), want...)
	c := containerClients(t)
	reg := c.vars["GCPEMU_REGISTRY"]
	if reg != "127.0.0.1:"+registryPort {
		t.Fatalf("gcpemu env sets GCPEMU_REGISTRY=%q; want 127.0.0.1:%s:\n%s", reg, registryPort, containerEnv(t))
	}
	c.run(t, ctx, "gcloud", "artifacts", "repositories", "create", "published", "--repository-format=docker", "--location=us-central1")
	pushPull(t, ctx, c, rt, reg+"/us-central1-docker.pkg.dev/"+project+"/published/busybox:prerelease")
}

// waitRegistryRelay waits until doctor reports the Instance's Registry
// relay running, which it starts in the background, and fails t when it
// reports it skipped or failed.
func waitRegistryRelay(t *testing.T, ctx context.Context, h *hostInstance) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		out, _ := h.gcpemu(ctx, "doctor")
		for line := range strings.Lines(out) {
			switch {
			case strings.HasPrefix(line, "[ok  ] Registry relay:"):
				return
			case strings.Contains(line, "Registry relay") && strings.HasPrefix(line, "[FAIL]"):
				t.Fatalf("gcpemu doctor reports the Registry relay not running:\n%s", out)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("gcpemu doctor does not report the Registry relay running after 2 minutes:\n%s", out)
		}
		time.Sleep(2 * time.Second)
	}
}
