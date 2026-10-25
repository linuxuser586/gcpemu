package main_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/hosttest"
)

// startLinked starts a Persistent Instance named name in dir with a link
// listener on loopback, and returns its link address.
func startLinked(t *testing.T, name, dir string) string {
	t.Helper()
	return startLinkedOn(t, name, dir, "127.0.0.1", nil)
}

// startLinkedOn is startLinked with the link listener on ip, and env.
func startLinkedOn(t *testing.T, name, dir, ip string, env []string) string {
	t.Helper()
	out := must(t, env, "start", "--detach", "--instance", name, "--data-dir", dir, "--services", "gcs",
		"--port", "0", "--link-bind", ip, "--wait-timeout", "60s")
	if !strings.Contains(out, "ready") {
		t.Fatalf("start --detach: %s", out)
	}
	t.Cleanup(func() { _, _ = gcpemu(t, nil, "stop", "--data-dir", dir) })
	b, err := os.ReadFile(filepath.Join(dir, "endpoints.json"))
	if err != nil {
		t.Fatal(err)
	}
	var eps map[string]string
	_ = json.Unmarshal(b, &eps)
	if eps["link"] == "" {
		t.Fatalf("no link endpoint in %s", b)
	}
	return eps["link"]
}

// TestLinkPairing is #616 end to end: two Instances pair with `gcpemu
// link pair` on one and `gcpemu link approve` on the other, each lists the
// other, doctor reports the link, and the Pairings survive Reset and a
// restart of a Persistent Instance.
func TestLinkPairing(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	laptop, office := t.TempDir(), t.TempDir()
	startLinked(t, "laptop", laptop)
	addr := startLinked(t, "office", office)

	var pairOut bytes.Buffer
	pair := exec.Command(bin, "link", "pair", addr, "--yes", "--data-dir", laptop)
	pair.Env = append(os.Environ(), "CI=", "GCPEMU_INSTANCE=")
	pair.Stdout, pair.Stderr = &pairOut, &pairOut
	if err := pair.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- pair.Wait() }()

	// The office sees the request, naming the laptop, before anything is paired.
	var reqs string
	for i := 0; i < 100 && !strings.Contains(reqs, "laptop"); i++ {
		time.Sleep(100 * time.Millisecond)
		reqs = must(t, nil, "link", "requests", "--data-dir", office)
	}
	if !strings.Contains(reqs, "laptop") || !strings.Contains(reqs, "sha256:") {
		t.Fatalf("link requests on the office:\n%s", reqs)
	}
	if out := must(t, nil, "link", "list", "--data-dir", office); strings.Contains(out, "laptop") {
		t.Fatalf("paired before approval:\n%s", out)
	}
	must(t, nil, "link", "approve", "--yes", "--data-dir", office)
	select {
	case err := <-waited:
		if err != nil || !strings.Contains(pairOut.String(), `link "office"`) {
			t.Fatalf("link pair: %v\n%s", err, pairOut.String())
		}
	case <-time.After(30 * time.Second):
		_ = pair.Process.Kill()
		t.Fatalf("link pair did not return after approval:\n%s", pairOut.String())
	}

	for _, c := range []struct{ dir, other string }{{laptop, "office"}, {office, "laptop"}} {
		if out := must(t, nil, "link", "list", "--data-dir", c.dir); !strings.Contains(out, c.other) || !strings.Contains(out, "paired") {
			t.Errorf("link list lacks %s:\n%s", c.other, out)
		}
	}
	doc, _ := gcpemu(t, nil, "doctor", "--data-dir", laptop)
	if !strings.Contains(doc, `link "office"`) || !strings.Contains(doc, "reachable") || !strings.Contains(doc, "protocol version 1") {
		t.Errorf("doctor does not report the link:\n%s", doc)
	}

	must(t, nil, "reset", "--data-dir", office)
	if out := must(t, nil, "link", "list", "--data-dir", office); !strings.Contains(out, "laptop") {
		t.Errorf("link list after Reset:\n%s", out)
	}
	must(t, nil, "stop", "--data-dir", laptop)
	startLinked(t, "laptop", laptop)
	if out := must(t, nil, "link", "list", "--data-dir", laptop); !strings.Contains(out, "office") {
		t.Errorf("link list after restart:\n%s", out)
	}

	must(t, nil, "link", "unpair", "office", "--data-dir", laptop)
	if out := must(t, nil, "link", "list", "--data-dir", laptop); strings.Contains(out, "office") {
		t.Errorf("link list after unpair:\n%s", out)
	}
}

// TestLinkDiscover is #621 end to end: two Instances with --link-bind on
// the LAN each list the other in `gcpemu link discover`, and one pairs
// with the other by its discovered name, which the other still approves.
// Their mDNS port is the test's own, so that they meet no other Instances.
func TestLinkDiscover(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	ip := hosttest.LANIP(t)
	env := []string{"GCPEMU_LINK_MDNS_PORT=" + strconv.Itoa(hosttest.FreeUDPPort(t))}
	laptop, office := t.TempDir(), t.TempDir()
	startLinkedOn(t, "laptop", laptop, ip, env)
	addr := startLinkedOn(t, "office", office, ip, env)

	host, _ := os.Hostname()
	host, _, _ = strings.Cut(strings.ToLower(host), ".")
	name := "office@" + host
	out := must(t, env, "link", "discover", "--data-dir", laptop)
	if !strings.Contains(out, name) || !strings.Contains(out, addr) || strings.Contains(out, "laptop@") {
		t.Fatalf("link discover on the laptop:\n%s", out)
	}
	if out := must(t, env, "link", "discover", "--data-dir", office); !strings.Contains(out, "laptop@"+host) {
		t.Fatalf("link discover on the office:\n%s", out)
	}

	var pairOut bytes.Buffer
	pair := exec.Command(bin, "link", "pair", name, "--yes", "--data-dir", laptop)
	pair.Env = append(append(os.Environ(), "CI=", "GCPEMU_INSTANCE="), env...)
	pair.Stdout, pair.Stderr = &pairOut, &pairOut
	if err := pair.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- pair.Wait() }()
	var reqs string
	for i := 0; i < 100 && !strings.Contains(reqs, "laptop"); i++ {
		time.Sleep(100 * time.Millisecond)
		reqs = must(t, env, "link", "requests", "--data-dir", office)
	}
	if !strings.Contains(reqs, "laptop") {
		t.Fatalf("pairing by discovered name made no request on the office:\n%s\n%s", reqs, pairOut.String())
	}
	must(t, env, "link", "approve", "--yes", "--data-dir", office)
	select {
	case err := <-waited:
		if err != nil || !strings.Contains(pairOut.String(), `link "office"`) {
			t.Fatalf("link pair %s: %v\n%s", name, err, pairOut.String())
		}
	case <-time.After(30 * time.Second):
		_ = pair.Process.Kill()
		t.Fatalf("link pair did not return after approval:\n%s", pairOut.String())
	}
	if out := must(t, env, "link", "discover", "--data-dir", laptop); !strings.Contains(out, "paired as office") {
		t.Fatalf("link discover after Pairing:\n%s", out)
	}
}
