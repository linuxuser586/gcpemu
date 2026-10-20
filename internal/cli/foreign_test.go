package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/oauth2/google"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/services"
)

func writeCreds(t *testing.T, path string, v map[string]string) string {
	t.Helper()
	b, _ := json.Marshal(v)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestForeignCredsFile checks the detector for a
// GOOGLE_APPLICATION_CREDENTIALS file (#247).
func TestForeignCredsFile(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		name    string
		creds   map[string]string // nil: no file
		foreign string            // in the reason; "" for none
		err     bool
	}{
		{"emulator key", map[string]string{"type": "service_account", "universe_domain": "gcpemu.invalid"}, "", false},
		{"google key", map[string]string{"type": "service_account", "universe_domain": "googleapis.com"}, "service_account credentials in the googleapis.com universe", false},
		{"no universe", map[string]string{"type": "service_account"}, "in the googleapis.com universe", false},
		{"authorized user", map[string]string{"type": "authorized_user", "refresh_token": "1//x"}, "authorized_user credentials", false},
		{"missing", nil, "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(dir, strings.ReplaceAll(c.name, " ", "-")+".json")
			if c.creds != nil {
				writeCreds(t, p, c.creds)
			}
			why, err := foreignCredsFile(p)
			if (err != nil) != c.err {
				t.Fatalf("err = %v", err)
			}
			if c.foreign == "" && why != "" || c.foreign != "" && !strings.Contains(why, c.foreign) {
				t.Errorf("why = %q, want %q", why, c.foreign)
			}
		})
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := foreignCredsFile(bad); err == nil {
		t.Error("not JSON: no error")
	}
}

func TestWellKnownADCPaths(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	for _, c := range []struct {
		name string
		goos string
		env  map[string]string
		want []string
	}{
		{"home", "linux", map[string]string{"HOME": "/h"}, []string{"/h/.config/gcloud/application_default_credentials.json"}},
		{"cloudsdk config first", "darwin", map[string]string{"HOME": "/h", "CLOUDSDK_CONFIG": "/c"},
			[]string{"/c/application_default_credentials.json", "/h/.config/gcloud/application_default_credentials.json"}},
		{"windows", "windows", map[string]string{"APPDATA": "/a", "HOME": "/h"}, []string{filepath.Join("/a", "gcloud", "application_default_credentials.json")}},
		{"nothing", "linux", nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := wellKnownADCPaths(c.goos, env(c.env))
			for i := range c.want {
				c.want[i] = filepath.FromSlash(c.want[i])
			}
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// TestEnvForeignCredentials checks that `gcpemu env` points ADC at an
// emulator key when gcloud's application-default credentials exist, keeps
// it until Reset removes its key, and never overrides a user-set
// GOOGLE_APPLICATION_CREDENTIALS (#247, ADR 0010).
func TestEnvForeignCredentials(t *testing.T) {
	inst := emutest.Start(t, []string{"iam"})
	run := func(args ...string) (string, string, error) {
		cmd := New(services.Factories())
		var out, errOut bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&errOut)
		cmd.SetArgs(append(args, "--data-dir", inst.Internal().Config.DataDir, "--config", os.DevNull))
		err := cmd.Execute()
		return out.String(), errOut.String(), err
	}
	keyFrom := func(out string) string {
		for _, l := range strings.Split(out, "\n") {
			if v, ok := strings.CutPrefix(l, "export "+adcEnv+"="); ok {
				return strings.Trim(v, "'")
			}
		}
		return ""
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLOUDSDK_CONFIG", "")
	t.Setenv("APPDATA", "")
	t.Setenv(adcEnv, "")

	out, _, err := run("env")
	if err != nil {
		t.Fatal(err)
	}
	if k := keyFrom(out); k != "" {
		t.Fatalf("without application-default credentials, env exported %s=%s", adcEnv, k)
	}

	wk := writeCreds(t, filepath.Join(home, ".config", "gcloud", "application_default_credentials.json"),
		map[string]string{"type": "authorized_user", "refresh_token": "1//fabricated"})
	if runtime.GOOS == "windows" {
		t.Setenv("CLOUDSDK_CONFIG", filepath.Dir(wk))
	}
	out, stderr, err := run("env")
	if err != nil {
		t.Fatal(err)
	}
	key := keyFrom(out)
	if key == "" || !strings.Contains(stderr, wk) {
		t.Fatalf("env did not neutralise %s:\n%s\nstderr: %s", wk, out, stderr)
	}
	b, err := os.ReadFile(key)
	if err != nil {
		t.Fatal(err)
	}
	if why, err := foreignCredsFile(key); err != nil || why != "" {
		t.Errorf("the emulator key is Foreign credentials: %q %v", why, err)
	}
	cfg, err := google.JWTConfigFromJSON(b, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.TokenSource(context.Background()).Token(); err != nil {
		t.Errorf("the emulator key gets no token: %v", err)
	}

	// The same key while it is valid, also from a shell that ran env before.
	t.Setenv(adcEnv, key)
	out, _, _ = run("env")
	if again, _ := os.ReadFile(keyFrom(out)); !bytes.Equal(again, b) {
		t.Errorf("a second env gave a different key")
	}
	// A new one once Reset removes it.
	if err := inst.Internal().Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	out, _, _ = run("env")
	if again, _ := os.ReadFile(keyFrom(out)); len(again) == 0 || bytes.Equal(again, b) {
		t.Errorf("env kept a key Reset removed")
	}

	// A user-set GOOGLE_APPLICATION_CREDENTIALS is only warned about.
	gk := writeCreds(t, filepath.Join(t.TempDir(), "google.json"), map[string]string{"type": "service_account", "universe_domain": "googleapis.com"})
	t.Setenv(adcEnv, gk)
	out, stderr, err = run("env")
	if err != nil {
		t.Fatal(err)
	}
	if k := keyFrom(out); k != "" {
		t.Errorf("env overrode a user-set %s with %s", adcEnv, k)
	}
	if !strings.Contains(stderr, gk) || !strings.Contains(stderr, "real Google") {
		t.Errorf("env did not warn about %s: %q", gk, stderr)
	}
}

// TestCheckForeignCreds checks the doctor check (#247).
func TestCheckForeignCreds(t *testing.T) {
	check := func() (string, int) {
		var out bytes.Buffer
		fails := 0
		checkForeignCreds(&out, func(ok bool, what, fix string) {
			if !ok {
				fails++
			}
			out.WriteString(what + "\n")
		})
		return out.String(), fails
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLOUDSDK_CONFIG", "")
	t.Setenv("APPDATA", "")
	t.Setenv(adcEnv, "")
	if out, fails := check(); out != "" || fails != 0 {
		t.Errorf("clean: %q, %d failures", out, fails)
	}
	t.Setenv(adcEnv, writeCreds(t, filepath.Join(home, "google.json"), map[string]string{"type": "service_account"}))
	if out, fails := check(); fails != 1 || !strings.Contains(out, "googleapis.com universe") {
		t.Errorf("google key: %q, %d failures", out, fails)
	}
	t.Setenv(adcEnv, writeCreds(t, filepath.Join(home, "emu.json"), map[string]string{"type": "service_account", "universe_domain": "gcpemu.invalid"}))
	if out, fails := check(); fails != 0 || !strings.Contains(out, "emulator key") {
		t.Errorf("emulator key: %q, %d failures", out, fails)
	}
	t.Setenv(adcEnv, "")
	t.Setenv("CLOUDSDK_CONFIG", home)
	writeCreds(t, filepath.Join(home, "application_default_credentials.json"), map[string]string{"type": "authorized_user"})
	if out, fails := check(); fails != 0 || !strings.Contains(out, "gcpemu env") {
		t.Errorf("well-known file: %q, %d failures", out, fails)
	}
}
