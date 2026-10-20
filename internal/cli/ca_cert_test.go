package cli

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/ca"
)

// testCA returns an Instance CA and its ca.pem read back as a --cert file.
func testCA(t *testing.T) (*ca.CA, *certFile) {
	t.Helper()
	c, err := ca.Load(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	f, err := readCertFile(c.Path(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return c, f
}

// A --cert certificate yields exactly the steps the instance's own CA
// does, on every trust host, for install and uninstall (#496).
func TestCertFilePlansMatchCA(t *testing.T) {
	c, f := testCA(t)
	if f.Path() != c.Path() || !bytes.Equal(f.PEM(), c.PEM()) || !f.Certificate().Equal(c.Certificate()) {
		t.Fatalf("certFile = %q %q, want %q", f.Path(), f.PEM(), c.Path())
	}
	if thumbprint(f) != thumbprint(c) {
		t.Fatalf("thumbprint %s, want %s", thumbprint(f), thumbprint(c))
	}
	home := t.TempDir()
	hosts := map[string]trustHost{
		"debian": {goos: "linux", lookPath: func(string) bool { return true }, home: home,
			isDir: func(p string) bool {
				return p == "/usr/local/share/ca-certificates" || p == "/etc/docker" || p == "/etc/containerd"
			},
			nssHas: func(string, string) bool { return true }, fileExists: func(string) bool { return true }},
		"rhel": {goos: "linux", root: true, lookPath: func(n string) bool { return n != "update-ca-certificates" }, home: home,
			isDir:  func(p string) bool { return p == "/etc/pki/ca-trust/source/anchors" || p == "/etc/docker" },
			nssHas: func(string, string) bool { return false }, fileExists: func(string) bool { return false }},
		"darwin":  {goos: "darwin"},
		"windows": {goos: "windows"},
	}
	for name, h := range hosts {
		for _, install := range []bool{true, false} {
			want, werr := trustPlan(h, "container", c, install)
			got, gerr := trustPlan(h, "container", f, install)
			if werr != nil || gerr != nil || !reflect.DeepEqual(got, want) {
				t.Errorf("%s install=%v: trustPlan %v %v, want %v", name, install, got, gerr, want)
			}
			if got, want := containerTrustPlan(h, "container", f, install), containerTrustPlan(h, "container", c, install); !reflect.DeepEqual(got, want) {
				t.Errorf("%s install=%v: containerTrustPlan %v, want %v", name, install, got, want)
			}
			for _, mode := range []nssMode{nssInstall, nssUninstall} {
				got, gn := nssPlan(h, "container", f, mode)
				want, wn := nssPlan(h, "container", c, mode)
				if !reflect.DeepEqual(got, want) || gn != wn {
					t.Errorf("%s mode=%v: nssPlan %v, want %v", name, mode, got, want)
				}
			}
		}
	}
	for _, ep := range []string{"/Users/u/.colima/default/docker.sock", "npipe:////./pipe/podman-machine-default", "/Users/u/.docker/run/docker.sock"} {
		for _, install := range []bool{true, false} {
			if got, want := vmTrustCommand(ep, "container", f, install), vmTrustCommand(ep, "container", c, install); got != want {
				t.Errorf("%s: vmTrustCommand %+v, want %+v", ep, got, want)
			}
		}
	}
	// Uninstall removes by the SHA-1 thumbprint on macOS and Windows.
	sum := strings.ToUpper(thumbprint(c))
	if p, _ := trustPlan(hosts["darwin"], "container", f, false); !strings.Contains(p[0].String(), "-Z "+sum) {
		t.Errorf("darwin uninstall: %v", p)
	}
	if p, _ := trustPlan(hosts["windows"], "container", f, false); !strings.HasSuffix(p[0].String(), " "+thumbprint(c)) {
		t.Errorf("windows uninstall: %v", p)
	}
}

func pemOf(typ string, b []byte) []byte { return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: b}) }

func leafDER(t *testing.T) []byte {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "leaf"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// --cert takes exactly one PEM CERTIFICATE block holding a CA certificate;
// anything else fails with an error naming the problem.
func TestReadCertFileErrors(t *testing.T) {
	c, _ := testCA(t)
	dir := t.TempDir()
	key, _ := os.ReadFile(filepath.Join(filepath.Dir(c.Path()), "ca-key.pem"))
	for _, tc := range []struct {
		name, content, want string
	}{
		{"missing", "", ""},
		{"text", "hello\n", "no PEM block"},
		{"key", string(key), "PRIVATE KEY"},
		{"two", string(c.PEM()) + string(c.PEM()), "more than one PEM block"},
		{"garbage", string(pemOf("CERTIFICATE", []byte("der"))), "not an X.509 certificate"},
		{"leaf", string(pemOf("CERTIFICATE", leafDER(t))), "not a CA certificate"},
	} {
		p := filepath.Join(dir, tc.name+".pem")
		if tc.name != "missing" {
			if err := os.WriteFile(p, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		_, err := readCertFile(p, nil)
		if tc.name == "missing" {
			if !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "--cert") {
				t.Errorf("%s: err %v, want a --cert not-exist error", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "--cert") {
			t.Errorf("%s: err %v, want it to contain %q", tc.name, err, tc.want)
		}
	}
	if _, err := readCertFile("-", strings.NewReader("")); err == nil || !strings.Contains(err.Error(), "--cert -: no PEM block") {
		t.Errorf("empty stdin: %v", err)
	}
}

// --cert - reads stdin; a step that needs a path gets a temporary file,
// which cleanup removes, or on a dry run a placeholder.
func TestReadCertFileStdin(t *testing.T) {
	c, _ := testCA(t)
	f, err := readCertFile("-", bytes.NewReader(c.PEM()))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(f.PEM(), c.PEM()) || f.Path() != "" {
		t.Fatalf("stdin cert: %q %q", f.Path(), f.PEM())
	}
	cleanup, err := f.ensurePath(false, false)
	if err != nil || f.Path() != "" {
		t.Fatalf("ensurePath(false): %q %v", f.Path(), err)
	}
	cleanup()
	// A dry run names a placeholder and creates nothing.
	d, _ := readCertFile("-", bytes.NewReader(c.PEM()))
	cleanup, err = d.ensurePath(true, true)
	if err != nil || d.Path() != stdinCertPath {
		t.Fatalf("dry run: %q %v", d.Path(), err)
	}
	cleanup()
	cleanup, err = f.ensurePath(true, false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(f.Path())
	if err != nil || !bytes.Equal(b, c.PEM()) {
		t.Fatalf("temp file %s: %q %v", f.Path(), b, err)
	}
	cleanup()
	if _, err := os.Stat(f.Path()); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind: %v", err)
	}
	// A file given by path is used as it is and never removed.
	_, g := testCA(t)
	cleanup, err = g.ensurePath(true, false)
	cleanup()
	if _, serr := os.Stat(g.Path()); err != nil || serr != nil {
		t.Fatalf("file cert: %v %v", err, serr)
	}
}

// With --cert, `ca install` and `ca uninstall` need an explicit --instance
// and never touch the instance data directory.
func TestCACertCommand(t *testing.T) {
	_, f := testCA(t)
	run := func(args ...string) (string, error) {
		root := New(nil)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(args)
		err := root.Execute()
		return out.String(), err
	}
	dataDir := filepath.Join(t.TempDir(), "data")
	for _, verb := range []string{"install", "uninstall"} {
		_, err := run("--data-dir", dataDir, "ca", verb, "--cert", f.Path(), "--dry-run")
		if err == nil || !strings.Contains(err.Error(), "--cert needs --instance <name>") {
			t.Errorf("%s without --instance: %v", verb, err)
		}
		_, err = run("--data-dir", dataDir, "--instance", "container", "ca", verb, "--cert", filepath.Join(dataDir, "missing.pem"), "--dry-run")
		if !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "--cert") {
			t.Errorf("%s with a missing cert: %v", verb, err)
		}
		out, err := run("--data-dir", dataDir, "--instance", "container", "ca", verb, "--cert", f.Path(), "--dry-run")
		// Hosts without a supported trust store fail in trustPlan, after
		// reading the certificate; either way nothing is created.
		if err != nil && !strings.Contains(err.Error(), "no supported trust store") && !strings.Contains(err.Error(), "not supported on") {
			t.Errorf("%s dry run: %v\n%s", verb, err, out)
		}
		if err == nil && !strings.Contains(out, "The following commands will be run:") {
			t.Errorf("%s dry run output:\n%s", verb, out)
		}
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("data dir created: %v", err)
	}
}
