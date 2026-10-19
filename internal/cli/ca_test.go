package cli

import (
	"crypto/sha1"
	"encoding/hex"
	"os"
	"os/exec"
	"path"
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
	win := trustHost{goos: "windows"}
	plan, err = trustPlan(win, "x", c, true)
	if err != nil || len(plan) != 1 || plan[0].String() != "certutil -user -addstore Root "+c.Path() {
		t.Fatalf("windows install: %v %v", plan, err)
	}
	sum := sha1.Sum(c.Certificate().Raw)
	plan, err = trustPlan(win, "x", c, false)
	if err != nil || len(plan) != 1 || plan[0].String() != "certutil -user -delstore Root "+hex.EncodeToString(sum[:]) {
		t.Fatalf("windows uninstall: %v %v", plan, err)
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
	// Slash form: the script uses etc unquoted, and on native Windows sh
	// would eat the backslashes of a Windows path (#220).
	etc := filepath.ToSlash(t.TempDir())
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

// A VM runtime that keeps its own trust store gets the command to run in
// its VM; Docker Desktop, OrbStack and native sockets get none (#175). On
// Windows, the default Podman machine's named pipe gets the same command,
// for Git Bash, and Docker Desktop's pipes get a restart, since it rebuilds
// its trust from the Windows store `ca install` fills (#377).
func TestVMTrustCommand(t *testing.T) {
	c, err := ca.Load(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		endpoint, what, ssh, dir, shell string
		restart                         bool
	}{
		{endpoint: "/Users/u/.colima/default/docker.sock", what: "Colima (profile default)", ssh: "colima ssh -p default -- sudo sh -s <<'GCPEMU_VM'\n", dir: "/etc/docker/certs.d/$h/gcpemu-dev.crt"},
		{endpoint: "unix:///Users/u/.colima/work/docker.sock", what: "Colima (profile work)", ssh: "colima ssh -p work -- sudo sh -s", dir: "/etc/docker/certs.d/$h/gcpemu-dev.crt"},
		{endpoint: "/var/folders/xy/T/podman/podman-machine-default-api.sock", what: "Podman machine podman-machine-default", ssh: "podman machine ssh podman-machine-default sudo sh -s", dir: "/etc/containers/certs.d/$h/gcpemu-dev.crt"},
		{endpoint: "/Users/u/.local/share/containers/podman/machine/qemu/podman.sock", what: "Podman machine", ssh: "podman machine ssh sudo sh -s", dir: "/etc/containers/certs.d/$h/gcpemu-dev.crt"},
		{endpoint: "npipe:////./pipe/podman-machine-default", what: "Podman machine podman-machine-default", ssh: "podman machine ssh podman-machine-default sudo sh -s <<'GCPEMU_VM'\n", dir: "/etc/containers/certs.d/$h/gcpemu-dev.crt", shell: "Git Bash"},
		{endpoint: `npipe://\\.\pipe\podman-machine-default`, what: "Podman machine podman-machine-default", ssh: "podman machine ssh podman-machine-default sudo sh -s", dir: "/etc/containers/certs.d/$h/gcpemu-dev.crt", shell: "Git Bash"},
		{endpoint: "npipe:////./pipe/podman-work"}, // machine work or podman-work: ambiguous
		{endpoint: "npipe:////./pipe/dockerDesktopLinuxEngine", what: "Docker Desktop", restart: true},
		{endpoint: "npipe:////./pipe/docker_engine", what: "Docker Desktop", restart: true},
		{endpoint: "npipe:////./pipe/other"},
		{endpoint: "/Users/u/.docker/run/docker.sock"},
		{endpoint: "/Users/u/.orbstack/run/docker.sock"},
		{endpoint: "unix:///var/run/docker.sock"},
		{endpoint: ""},
	} {
		for _, install := range []bool{true, false} {
			v := vmTrustCommand(tc.endpoint, "dev", c, install)
			if v.what != tc.what || v.shell != tc.shell || v.restart != tc.restart {
				t.Errorf("%s install=%v: %+v, want what %q shell %q restart %v", tc.endpoint, install, v, tc.what, tc.shell, tc.restart)
				continue
			}
			if tc.ssh == "" {
				if v.command != "" {
					t.Errorf("%s: command %q, want none", tc.endpoint, v.command)
				}
				continue
			}
			script := certsDirScript("/etc", "dev", c, []string{path.Dir(path.Dir(tc.dir))}, false, install)
			if !strings.HasPrefix(v.command, tc.ssh) || !strings.Contains(v.command, tc.dir) ||
				!strings.HasSuffix(v.command, "\n"+script+"GCPEMU_VM") {
				t.Errorf("%s install=%v: %s", tc.endpoint, install, v.command)
			}
			if strings.Contains(v.command, "/etc/containerd") {
				t.Errorf("%s: writes containerd config: %s", tc.endpoint, v.command)
			}
		}
	}
}

// writeVMTrust's advice: unchanged for Unix sockets, naming the shell for
// a Podman machine on Windows and asking for a Docker Desktop restart.
func TestWriteVMTrust(t *testing.T) {
	for _, tc := range []struct {
		v       vmTrust
		install bool
		want    string
	}{
		{vmTrust{}, true, ""},
		{vmTrust{what: "Colima (profile default)", command: "CMD"}, true,
			"Colima (profile default) keeps its own container trust store inside its VM, which gcpemu does not change. To trust the CA for image pulls there, run:\n\nCMD\n\n"},
		{vmTrust{what: "Podman machine", command: "CMD"}, false,
			"Podman machine keeps its own container trust store inside its VM, which gcpemu does not change. To remove the CA from it, run:\n\nCMD\n\n"},
		{vmTrust{what: "Podman machine work", command: "CMD", shell: "Git Bash"}, true,
			"Podman machine work keeps its own container trust store inside its VM, which gcpemu does not change. To trust the CA for image pulls there, run this in Git Bash (PowerShell has no heredocs):\n\nCMD\n\n"},
		{vmTrust{what: "Docker Desktop", restart: true}, true,
			"Docker Desktop rebuilds its container trust store from the Windows certificate store when it starts. Restart Docker Desktop to trust the CA for image pulls there.\n\n"},
		{vmTrust{what: "Docker Desktop", restart: true}, false,
			"Docker Desktop rebuilds its container trust store from the Windows certificate store when it starts. Restart Docker Desktop to remove the CA from it.\n\n"},
	} {
		var b strings.Builder
		writeVMTrust(&b, tc.v, tc.install)
		if b.String() != tc.want {
			t.Errorf("%+v install=%v:\n got %q\nwant %q", tc.v, tc.install, b.String(), tc.want)
		}
	}
}
