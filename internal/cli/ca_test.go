package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/ca"
)

func TestTrustPlan(t *testing.T) {
	c, err := ca.Load(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	deb := trustHost{goos: "linux", lookPath: func(n string) bool { return n == "update-ca-certificates" },
		isDir: func(p string) bool { return p == "/usr/local/share/ca-certificates" }}
	plan, err := trustPlan(deb, "default", c, true)
	if err != nil || len(plan) != 2 || plan[0].String() != "sudo tee /usr/local/share/ca-certificates/gcpemu-default.crt" ||
		plan[1].String() != "sudo update-ca-certificates" || string(plan[0].stdin) != string(c.PEM()) {
		t.Fatalf("debian install: %v %v", plan, err)
	}
	plan, _ = trustPlan(deb, "default", c, false)
	if plan[0].String() != "sudo rm -f /usr/local/share/ca-certificates/gcpemu-default.crt" || plan[1].String() != "sudo update-ca-certificates --fresh" {
		t.Fatalf("debian uninstall: %v", plan)
	}
	rh := trustHost{goos: "linux", root: true, lookPath: func(n string) bool { return n == "update-ca-trust" },
		isDir: func(p string) bool { return p == "/etc/pki/ca-trust/source/anchors" }}
	plan, _ = trustPlan(rh, "x", c, true)
	if plan[0].String() != "tee /etc/pki/ca-trust/source/anchors/gcpemu-x.crt" || plan[1].String() != "update-ca-trust extract" {
		t.Fatalf("rhel: %v", plan)
	}
	mac := trustHost{goos: "darwin"}
	plan, _ = trustPlan(mac, "x", c, true)
	if !strings.HasPrefix(plan[0].String(), "sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain ") {
		t.Fatalf("mac: %v", plan)
	}
	if _, err := trustPlan(trustHost{goos: "linux", lookPath: func(string) bool { return false }, isDir: func(string) bool { return false }}, "x", c, true); err == nil {
		t.Fatal("expected error without tooling")
	}
}

// TestContainerTrustPlan runs the Docker/containerd trust script against
// a temporary /etc: install writes the CA for every registry host without
// touching the user's files; uninstall removes only what it wrote.
func TestContainerTrustPlan(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	c, err := ca.Load(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	etc := t.TempDir()
	for _, d := range []string{"docker/certs.d/us-docker.pkg.dev", "containerd/certs.d/europe-docker.pkg.dev"} {
		if err := os.MkdirAll(filepath.Join(etc, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	userCA := filepath.Join(etc, "docker/certs.d/us-docker.pkg.dev/ca.crt")
	userHosts := filepath.Join(etc, "containerd/certs.d/europe-docker.pkg.dev/hosts.toml")
	_ = os.WriteFile(userCA, []byte("user"), 0o644)
	_ = os.WriteFile(userHosts, []byte("server = \"https://mirror\"\n"), 0o644)

	h := trustHost{goos: "linux", root: true, etc: etc, isDir: func(p string) bool { fi, err := os.Stat(p); return err == nil && fi.IsDir() }}
	run := func(install bool) {
		t.Helper()
		plan := containerTrustPlan(h, "default", c, install)
		if len(plan) != 1 || plan[0].String() != "sh -s" || !plan[0].script {
			t.Fatalf("plan = %v", plan)
		}
		cmd := exec.Command("sh", "-s")
		cmd.Stdin = strings.NewReader(string(plan[0].stdin))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("script: %v\n%s\n%s", err, out, plan[0].stdin)
		}
	}
	read := func(p string) string { b, _ := os.ReadFile(filepath.Join(etc, p)); return string(b) }

	run(true)
	if got := read("docker/certs.d/us-central1-docker.pkg.dev/gcpemu-default.crt"); got != strings.TrimSpace(string(c.PEM()))+"\n" {
		t.Fatalf("docker CA = %q", got)
	}
	toml := read("containerd/certs.d/us-docker.pkg.dev/hosts.toml")
	want := "# written by gcpemu (default)\nserver = \"https://us-docker.pkg.dev\"\n\n[host.\"https://us-docker.pkg.dev\"]\n  ca = \"" + etc + "/gcpemu/gcpemu-default.crt\"\n"
	if toml != want {
		t.Fatalf("hosts.toml = %q, want %q", toml, want)
	}
	if read("docker/certs.d/us-docker.pkg.dev/ca.crt") != "user" || read("containerd/certs.d/europe-docker.pkg.dev/hosts.toml") != "server = \"https://mirror\"\n" {
		t.Fatal("user trust files changed")
	}

	run(false)
	for _, p := range []string{"docker/certs.d/us-central1-docker.pkg.dev", "containerd/certs.d/us-docker.pkg.dev", "gcpemu/gcpemu-default.crt"} {
		if _, err := os.Stat(filepath.Join(etc, p)); !os.IsNotExist(err) {
			t.Errorf("%s left behind: %v", p, err)
		}
	}
	if read("docker/certs.d/us-docker.pkg.dev/ca.crt") != "user" || read("containerd/certs.d/europe-docker.pkg.dev/hosts.toml") == "" {
		t.Fatal("uninstall removed user trust files")
	}

	if p := containerTrustPlan(trustHost{goos: "darwin"}, "x", c, true); p != nil {
		t.Errorf("darwin plan = %v", p)
	}
	if p := containerTrustPlan(trustHost{goos: "linux", isDir: func(string) bool { return false }}, "x", c, true); p != nil {
		t.Errorf("plan without docker or containerd = %v", p)
	}
}
