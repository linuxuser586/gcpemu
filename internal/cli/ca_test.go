package cli

import (
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
