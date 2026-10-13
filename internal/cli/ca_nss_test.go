package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/linuxuser586/gcpemu/internal/ca"
)

// nssHome makes a home with a native, a snap and a flatpak Firefox
// profile (each with a cert9.db) and one profile Firefox never started.
func nssHome(t *testing.T, create func(dir string)) string {
	t.Helper()
	home := t.TempDir()
	for _, p := range []string{".mozilla/firefox/a.default", "snap/firefox/common/.mozilla/firefox/b.default", ".var/app/org.mozilla.firefox/.mozilla/firefox/c.default"} {
		dir := filepath.Join(home, p)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		create(dir)
	}
	if err := os.MkdirAll(filepath.Join(home, ".mozilla/firefox/unused"), 0o700); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestNSSPlan(t *testing.T) {
	c, err := ca.Load(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	home := nssHome(t, func(dir string) { _ = os.WriteFile(filepath.Join(dir, "cert9.db"), nil, 0o600) })
	present := map[string]bool{filepath.Join(home, ".mozilla/firefox/a.default"): true}
	h := trustHost{goos: "linux", home: home, lookPath: func(string) bool { return true }, fileExists: fileExists,
		nssHas: func(db, nick string) bool { return nick == "gcpemu-x" && present[db] }}
	strs := func(plan []trustStep) string {
		var s []string
		for _, p := range plan {
			s = append(s, filepath.ToSlash(strings.ReplaceAll(p.String(), home, "~")))
			if !p.soft {
				t.Errorf("%s is not soft", p)
			}
		}
		return strings.Join(s, "\n")
	}

	plan, note := nssPlan(h, "x", c, nssInstall)
	want := `mkdir -p ~/.pki/nssdb
certutil -N -d sql:~/.pki/nssdb --empty-password
certutil -A -d sql:~/.pki/nssdb -n gcpemu-x -t C,, -a
certutil -D -d sql:~/.mozilla/firefox/a.default -n gcpemu-x
certutil -A -d sql:~/.mozilla/firefox/a.default -n gcpemu-x -t C,, -a
certutil -A -d sql:~/snap/firefox/common/.mozilla/firefox/b.default -n gcpemu-x -t C,, -a
certutil -A -d sql:~/.var/app/org.mozilla.firefox/.mozilla/firefox/c.default -n gcpemu-x -t C,, -a`
	if got := strs(plan); got != want || note != "" {
		t.Fatalf("install = %q (note %q), want\n%s", got, note, want)
	}
	if !bytes.Equal(plan[2].stdin, c.PEM()) {
		t.Error("add step does not read the CA from stdin")
	}
	plan, _ = nssPlan(h, "x", c, nssUninstall)
	if got := strs(plan); got != "certutil -D -d sql:~/.mozilla/firefox/a.default -n gcpemu-x" {
		t.Errorf("uninstall = %q", got)
	}
	plan, _ = nssPlan(h, "x", c, nssReplace)
	if got := strs(plan); got != "certutil -D -d sql:~/.mozilla/firefox/a.default -n gcpemu-x\ncertutil -A -d sql:~/.mozilla/firefox/a.default -n gcpemu-x -t C,, -a" {
		t.Errorf("replace = %q", got)
	}

	sudo := h
	sudo.nssUser = "alice"
	plan, _ = nssPlan(sudo, "x", c, nssUninstall)
	if got := strs(plan); got != "sudo -u alice certutil -D -d sql:~/.mozilla/firefox/a.default -n gcpemu-x" {
		t.Errorf("as sudo user = %q", got)
	}

	missing := h
	missing.lookPath = func(n string) bool { return n != "certutil" }
	plan, note = nssPlan(missing, "x", c, nssInstall)
	if plan != nil || !strings.Contains(note, "libnss3-tools") {
		t.Errorf("without certutil: %v, note %q", plan, note)
	}
	if _, note = nssPlan(missing, "x", c, nssUninstall); note != "" {
		t.Errorf("uninstall without certutil notes %q", note)
	}
	if plan, _ = nssPlan(trustHost{goos: "darwin", home: home}, "x", c, nssInstall); plan != nil {
		t.Errorf("darwin plan = %v", plan)
	}
}

// TestNSSTrust runs the plans with the real certutil: install creates
// Chrome's database and trusts the CA everywhere, rotate replaces it and
// uninstall removes it.
func TestNSSTrust(t *testing.T) {
	// The certutil on Windows is Microsoft's, not NSS's; gcpemu uses NSS
	// only on Linux.
	if runtime.GOOS != "linux" {
		t.Skip("NSS trust is Linux-only")
	}
	if !certutilPresent() {
		t.Skip("no certutil")
	}
	home := nssHome(t, func(dir string) {
		if out, err := exec.Command("certutil", "-N", "-d", "sql:"+dir, "--empty-password").CombinedOutput(); err != nil {
			t.Fatalf("certutil -N: %v\n%s", err, out)
		}
	})
	c, err := ca.Load(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	h := trustHost{goos: "linux", home: home, lookPath: func(n string) bool { _, err := exec.LookPath(n); return err == nil },
		nssHas: nssHas, fileExists: fileExists}
	run := func(mode nssMode) {
		t.Helper()
		plan, _ := nssPlan(h, "test", c, mode)
		var out, errb bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&out)
		cmd.SetErr(&errb)
		if err := runPlan(cmd, plan, true, false); err != nil || errb.Len() > 0 {
			t.Fatalf("run: %v\n%s", err, errb.String())
		}
	}
	dbs := nssDBs(home)
	if len(dbs) != 4 {
		t.Fatalf("dbs = %v", dbs)
	}
	// trusted returns the DER of the CA trusted in db with "C,,".
	trusted := func(db nssDB) []byte {
		out, err := exec.Command("certutil", "-L", "-d", db.arg()).Output()
		if err != nil || !strings.Contains(string(out), "gcpemu-test") {
			return nil
		}
		for _, l := range strings.Split(string(out), "\n") {
			if f := strings.Fields(l); len(f) == 2 && f[0] == "gcpemu-test" && f[1] != "C,," {
				t.Errorf("%s: trust %q", db.what, f[1])
			}
		}
		der, _ := exec.Command("certutil", "-L", "-d", db.arg(), "-n", "gcpemu-test", "-r").Output()
		return der
	}

	run(nssInstall)
	run(nssInstall) // idempotent
	for _, d := range dbs {
		if !bytes.Equal(trusted(d), c.Certificate().Raw) {
			t.Errorf("%s does not trust the CA after install", d.what)
		}
	}

	if err := c.Rotate("test"); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("certutil", "-D", "-d", dbs[1].arg(), "-n", "gcpemu-test").CombinedOutput(); err != nil {
		t.Fatalf("certutil -D: %v\n%s", err, out)
	}
	run(nssReplace)
	if trusted(dbs[1]) != nil {
		t.Errorf("rotate added the CA to %s, which did not trust the old one", dbs[1].what)
	}
	for _, d := range append(dbs[:1:1], dbs[2:]...) {
		if !bytes.Equal(trusted(d), c.Certificate().Raw) {
			t.Errorf("%s does not trust the rotated CA", d.what)
		}
	}

	run(nssUninstall)
	for _, d := range dbs {
		if trusted(d) != nil {
			t.Errorf("%s still trusts the CA after uninstall", d.what)
		}
	}
}
