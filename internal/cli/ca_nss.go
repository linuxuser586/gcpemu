package cli

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"

	"github.com/linuxuser586/gcpemu/internal/ca"
)

// Browsers on Linux ignore the OS trust store: Chrome reads the user's
// shared NSS database (~/.pki/nssdb) and Firefox each profile's own
// cert9.db. `gcpemu ca install` adds the CA to them with certutil, as the
// user who owns them (Section 7.4).

// nssProvider is the package that ships certutil on Debian and Ubuntu.
const nssProvider = "libnss3-tools"

// firefoxProfileRoots are where native, snap and flatpak Firefox keep
// their profiles, relative to the home directory.
var firefoxProfileRoots = []string{
	".mozilla/firefox",
	"snap/firefox/common/.mozilla/firefox",
	".var/app/org.mozilla.firefox/.mozilla/firefox",
}

// nssUser returns the user whose NSS databases gcpemu manages: the
// invoking user, or the one behind sudo when run as root. name is set
// only in the sudo case, where steps must run as that user.
func nssUser(root bool) (home, name string) {
	if su := os.Getenv("SUDO_USER"); root && su != "" && su != "root" {
		if u, err := user.Lookup(su); err == nil {
			return u.HomeDir, su
		}
	}
	home, _ = os.UserHomeDir()
	return home, ""
}

// nssDB is one NSS database the CA is trusted in.
type nssDB struct {
	dir  string
	what string // "Chrome" or "Firefox profile NAME"
}

func (d nssDB) arg() string { return "sql:" + d.dir }

// nssDBs lists the user's NSS databases: Chrome's (even if it does not
// exist yet) and every Firefox profile that has a cert9.db.
func nssDBs(home string) []nssDB {
	if home == "" {
		return nil
	}
	dbs := []nssDB{{dir: filepath.Join(home, ".pki/nssdb"), what: "Chrome"}}
	for _, r := range firefoxProfileRoots {
		m, _ := filepath.Glob(filepath.Join(home, r, "*", "cert9.db"))
		for _, f := range m {
			dir := filepath.Dir(f)
			dbs = append(dbs, nssDB{dir: dir, what: "Firefox profile " + filepath.Base(dir)})
		}
	}
	return dbs
}

func nssNickname(instance string) string { return "gcpemu-" + instance }

func certutilPresent() bool { _, err := exec.LookPath("certutil"); return err == nil }

// nssHas reports whether db holds a certificate named nick.
func nssHas(db, nick string) bool {
	return exec.Command("certutil", "-L", "-d", "sql:"+db, "-n", nick).Run() == nil
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// nssMode selects what nssPlan does.
type nssMode int

const (
	nssInstall   nssMode = iota // add to every database, creating Chrome's
	nssUninstall                // remove where present
	nssReplace                  // re-add where present (`ca rotate`)
)

// nssPlan returns the certutil steps for mode, run as the NSS user. The
// CA is added as a trusted CA for TLS servers ("C,,") under the nickname
// gcpemu-INSTANCE; an existing entry is deleted first so a rotated CA
// replaces it. Without certutil the plan is empty and note says what to
// install. The steps are soft: a failing browser database never fails the
// rest of the install.
func nssPlan(h trustHost, instance string, c *ca.CA, mode nssMode) (plan []trustStep, note string) {
	if h.goos != "linux" {
		return nil, ""
	}
	dbs := nssDBs(h.home)
	if len(dbs) == 0 {
		return nil, ""
	}
	if !h.lookPath("certutil") {
		if mode == nssInstall {
			note = "certutil not found: install " + nssProvider + " and re-run `gcpemu ca install` to trust the CA in Chrome and Firefox"
		}
		return nil, note
	}
	as := func(args ...string) []string {
		if h.nssUser != "" {
			return append([]string{"sudo", "-u", h.nssUser}, args...)
		}
		return args
	}
	nick := nssNickname(instance)
	for _, d := range dbs {
		has := h.fileExists(filepath.Join(d.dir, "cert9.db")) && h.nssHas(d.dir, nick)
		if has {
			plan = append(plan, trustStep{args: as("certutil", "-D", "-d", d.arg(), "-n", nick), soft: true})
		}
		if mode == nssUninstall || (mode == nssReplace && !has) {
			continue
		}
		if !h.fileExists(filepath.Join(d.dir, "cert9.db")) {
			plan = append(plan,
				trustStep{args: as("mkdir", "-p", d.dir), soft: true},
				trustStep{args: as("certutil", "-N", "-d", d.arg(), "--empty-password"), soft: true})
		}
		plan = append(plan, trustStep{args: as("certutil", "-A", "-d", d.arg(), "-n", nick, "-t", "C,,", "-a"), stdin: c.PEM(), soft: true})
	}
	return plan, note
}
