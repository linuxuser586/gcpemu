package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// loopbackStubs are ifconfig, launchctl and chown for the loopback
// scripts. They log every call and keep the state macOS would: an alias
// that is not there cannot be removed, and a daemon that is loaded cannot
// be bootstrapped again or one that is not booted out.
var loopbackStubs = map[string]string{
	"ifconfig": `echo "ifconfig $*" >> "$STATE/log"
case "$2" in
alias) touch "$STATE/alias-$3" ;;
-alias) [ -e "$STATE/alias-$3" ] || { echo "ifconfig: SIOCDIFADDR: Can't assign requested address" >&2; exit 1; }; rm "$STATE/alias-$3" ;;
esac`,
	"launchctl": `echo "launchctl $*" >> "$STATE/log"
case "$1" in
print) [ -e "$STATE/loaded" ] || exit 113 ;;
bootstrap) [ ! -e "$STATE/loaded" ] && [ -f "$3" ] || { echo "Bootstrap failed: 5: Input/output error" >&2; exit 5; }; touch "$STATE/loaded" ;;
bootout) [ -e "$STATE/loaded" ] || { echo "Boot-out failed: 3: No such process" >&2; exit 3; }; rm "$STATE/loaded" ;;
esac`,
	"chown": `echo "chown $*" >> "$STATE/log"`,
}

// TestLoopbackPlan runs the install and uninstall scripts twice each
// against stubbed commands and checks the exact commands, the plist and
// that a second run does no harm.
func TestLoopbackPlan(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the scripts run on macOS")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	if p := loopbackPlan(false, loopbackDaemonDir, true); len(p) != 1 || p[0].String() != "sudo sh -s" || !p[0].script {
		t.Fatalf("plan = %v", p)
	}
	if p := loopbackPlan(true, loopbackDaemonDir, false); len(p) != 1 || p[0].String() != "sh -s" {
		t.Fatalf("plan as root = %v", p)
	}

	bin, state, dir := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "LaunchDaemons")
	for name, body := range loopbackStubs {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	plist := dir + "/" + loopbackLabel + ".plist"
	run := func(install bool) string {
		t.Helper()
		_ = os.Remove(filepath.Join(state, "log"))
		plan := loopbackPlan(true, dir, install)
		cmd := exec.Command("sh", "-s")
		cmd.Stdin = bytes.NewReader(plan[0].stdin)
		cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "STATE="+state)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("install=%v: %v\n%s\n%s", install, err, out, plan[0].stdin)
		}
		log, _ := os.ReadFile(filepath.Join(state, "log"))
		return string(log)
	}
	aliases := func(cmd string) string {
		var b strings.Builder
		for i := 2; i <= 254; i++ {
			fmt.Fprintf(&b, cmd+"\n", i)
		}
		return b.String()
	}
	count := func() int { m, _ := filepath.Glob(filepath.Join(state, "alias-*")); return len(m) }
	loaded := func() bool { _, err := os.Stat(filepath.Join(state, "loaded")); return err == nil }

	install := aliases("ifconfig lo0 alias 127.0.0.%d up") + "chown root:wheel " + plist + "\nlaunchctl print system/" + loopbackLabel + "\n"
	if got, want := run(true), install+"launchctl bootstrap system "+plist+"\n"; got != want {
		t.Fatalf("install ran:\n%s\nwant:\n%s", got, want)
	}
	if got := run(true); got != install {
		t.Fatalf("second install ran:\n%s\nwant:\n%s", got, install)
	}
	if b, err := os.ReadFile(plist); err != nil || string(b) != loopbackPlist() {
		t.Fatalf("plist = %q, %v", b, err)
	}
	if fi, _ := os.Stat(plist); fi.Mode().Perm() != 0o644 {
		t.Errorf("plist mode = %v", fi.Mode())
	}
	if count() != 253 || !loaded() {
		t.Fatalf("after install: %d aliases, loaded %v", count(), loaded())
	}

	uninstall := aliases("ifconfig lo0 -alias 127.0.0.%d")
	if got, want := run(false), "launchctl bootout system/"+loopbackLabel+"\n"+uninstall; got != want {
		t.Fatalf("uninstall ran:\n%s\nwant:\n%s", got, want)
	}
	if got, want := run(false), "launchctl bootout system/"+loopbackLabel+"\n"+uninstall; got != want {
		t.Fatalf("second uninstall ran:\n%s\nwant:\n%s", got, want)
	}
	if _, err := os.Stat(plist); !os.IsNotExist(err) {
		t.Errorf("plist left behind: %v", err)
	}
	if count() != 0 || loaded() {
		t.Fatalf("after uninstall: %d aliases, loaded %v", count(), loaded())
	}
}

func TestLoopbackPlist(t *testing.T) {
	p := loopbackPlist()
	for _, want := range []string{
		"<key>Label</key>\n\t<string>io.github.linuxuser586.gcpemu.loopback</string>",
		"<string>/bin/sh</string>\n\t\t<string>-c</string>\n\t\t<string>i=2; while [ $i -le 254 ]; do /sbin/ifconfig lo0 alias 127.0.0.$i up; i=$((i+1)); done</string>",
		"<key>RunAtLoad</key>\n\t<true/>",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plist lacks %q:\n%s", want, p)
		}
	}
	if _, err := exec.LookPath("plutil"); err == nil {
		f := filepath.Join(t.TempDir(), "x.plist")
		_ = os.WriteFile(f, []byte(p), 0o644)
		if out, err := exec.Command("plutil", "-lint", f).CombinedOutput(); err != nil {
			t.Fatalf("plutil: %v\n%s", err, out)
		}
	}
}

// TestLoopbackElsewhere: off macOS both commands do nothing and succeed.
func TestLoopbackElsewhere(t *testing.T) {
	for _, sub := range []string{"install", "uninstall"} {
		cmd := loopbackCmd("linux")
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs([]string{sub})
		if err := cmd.Execute(); err != nil || out.String() != "Nothing to do: linux routes all of 127.0.0.0/8 to loopback.\n" {
			t.Fatalf("%s: %q, %v", sub, out.String(), err)
		}
	}
}

func TestCheckLoopback(t *testing.T) {
	var b bytes.Buffer
	checkLoopback(&b, true)
	if b.String() != "[ok  ] loopback aliases 127.0.0.2-127.0.0.254 present\n" {
		t.Errorf("present: %q", b.String())
	}
	b.Reset()
	checkLoopback(&b, false)
	if !strings.HasPrefix(b.String(), "[--  ] loopback aliases 127.0.0.2-127.0.0.254 missing: `sudo gcpemu loopback install` lets more than one Cloud SQL instance run") {
		t.Errorf("missing: %q", b.String())
	}
}
