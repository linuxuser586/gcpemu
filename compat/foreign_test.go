//go:build compat

package compat

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"
)

// googleTokenHost is where Foreign credentials send their token requests.
const googleTokenHost = "oauth2.googleapis.com"

// writeJSONFile writes v as JSON to path, creating its directory.
func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// fabricatedGoogleKey writes a service-account key file shaped like one
// from real Google. It is not a real key: the Escape guard refuses the
// token request by host before anything is sent.
func fabricatedGoogleKey(t *testing.T) string {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	p := filepath.Join(t.TempDir(), "google-key.json")
	writeJSONFile(t, p, map[string]string{
		"type":            "service_account",
		"project_id":      project,
		"private_key_id":  "0123456789abcdef0123456789abcdef01234567",
		"private_key":     string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email":    "fabricated@" + project + ".iam.gserviceaccount.com",
		"client_id":       "100000000000000000001",
		"token_uri":       "https://" + googleTokenHost + "/token",
		"universe_domain": "googleapis.com",
	})
	return p
}

// homeEnv returns the variables that make home where gcloud keeps its
// configuration: %APPDATA%\gcloud on Windows, $HOME/.config/gcloud
// elsewhere.
func homeEnv(home string) map[string]string {
	if goruntime.GOOS == "windows" {
		return map[string]string{"APPDATA": home}
	}
	return map[string]string{"HOME": home}
}

// withHome returns set with homeEnv(home) added.
func withHome(set map[string]string, home string) map[string]string {
	out := maps.Clone(set)
	if out == nil {
		out = map[string]string{}
	}
	maps.Copy(out, homeEnv(home))
	return out
}

// fabricatedADCHome returns a home directory (see homeEnv) holding
// gcloud's application-default credentials file, fabricated as above.
func fabricatedADCHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "gcloud")
	if goruntime.GOOS == "windows" {
		dir = filepath.Join(home, "gcloud")
	}
	writeJSONFile(t, filepath.Join(dir, "application_default_credentials.json"), map[string]string{
		"type":          "authorized_user",
		"client_id":     "fabricated.apps.googleusercontent.com",
		"client_secret": "fabricated",
		"refresh_token": "1//fabricated",
	})
	return home
}

// testForeign covers #247: Foreign credentials send their token request
// to real Google before a Go HTTP client checks the universe, and `gcpemu
// env` keeps gcloud's application-default credentials from being used.
func testForeign(t *testing.T, e *emulator) {
	bin := filepath.Join(t.TempDir(), exe("foreigngo"))
	if out, err := exec.Command("go", "build", "-tags", "compat", "-o", bin, "./sdk/foreigngo").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	endpoint := e.vars["CLOUDSDK_API_ENDPOINT_OVERRIDES_IAM"]
	probe := func(t *testing.T, env []string) (string, []string) {
		t.Helper()
		out, escaped, _ := e.exec(append(env, e.guard.Env()...), bin, project, endpoint)
		return out, escaped
	}
	// gcpemuEnv runs `gcpemu env` in a shell whose home is home (see
	// homeEnv).
	gcpemuEnv := func(t *testing.T, home string) (map[string]string, string) {
		t.Helper()
		cmd := exec.Command(e.bin, "env", "--data-dir", e.dir)
		cmd.Env = withEnv(os.Environ(), homeEnv(home), "CLOUDSDK_CONFIG", adcEnvVar)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("gcpemu env: %v\n%s", err, stderr.String())
		}
		vars := map[string]string{}
		for _, l := range strings.Split(string(out), "\n") {
			if k, v, ok := strings.Cut(strings.TrimPrefix(l, "export "), "="); ok {
				vars[k] = strings.Trim(v, "'")
			}
		}
		return vars, stderr.String()
	}

	// The canary: a key from real Google escapes on its token request.
	t.Run("google-key", func(t *testing.T) {
		env := withEnv(e.env, withHome(map[string]string{adcEnvVar: fabricatedGoogleKey(t)}, t.TempDir()))
		if out, escaped := probe(t, env); !slices.Equal(escaped, []string{googleTokenHost}) {
			t.Errorf("the Escape guard refused %v, want [%s]; the probe said: %s", escaped, googleTokenHost, out)
		}
	})

	// The canary: gcloud's application-default credentials escape when
	// nothing overrides them.
	t.Run("well-known-file", func(t *testing.T) {
		env := withEnv(e.env, homeEnv(fabricatedADCHome(t)), adcEnvVar, "CLOUDSDK_CONFIG")
		if out, escaped := probe(t, env); !slices.Equal(escaped, []string{googleTokenHost}) {
			t.Errorf("the Escape guard refused %v, want [%s]; the probe said: %s", escaped, googleTokenHost, out)
		}
	})

	// `gcpemu env` points ADC at an emulator key instead, and reuses it.
	t.Run("well-known-file-gcpemu-env", func(t *testing.T) {
		home := fabricatedADCHome(t)
		vars, stderr := gcpemuEnv(t, home)
		key := vars[adcEnvVar]
		if key == "" {
			t.Fatalf("gcpemu env exported no %s; stderr: %s", adcEnvVar, stderr)
		}
		if !strings.Contains(stderr, "application_default_credentials.json") {
			t.Errorf("gcpemu env said nothing about the well-known file: %q", stderr)
		}
		env := withEnv(e.env, withHome(map[string]string{adcEnvVar: key}, home), "CLOUDSDK_CONFIG")
		out, escaped := probe(t, env)
		if len(escaped) > 0 {
			t.Errorf("the Escape guard refused %v; the probe said: %s", escaped, out)
		}
		if !strings.HasPrefix(out, "iam-adc OK") {
			t.Errorf("the probe said: %s", out)
		}
		if again, _ := gcpemuEnv(t, home); again[adcEnvVar] != key {
			t.Errorf("a second gcpemu env gave %q, want %q", again[adcEnvVar], key)
		}
	})

	// A GOOGLE_APPLICATION_CREDENTIALS the user set is never overridden,
	// only warned about.
	t.Run("user-set-gcpemu-env", func(t *testing.T) {
		key := fabricatedGoogleKey(t)
		cmd := exec.Command(e.bin, "env", "--data-dir", e.dir)
		cmd.Env = withEnv(os.Environ(), withHome(map[string]string{adcEnvVar: key}, fabricatedADCHome(t)), "CLOUDSDK_CONFIG")
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("gcpemu env: %v\n%s", err, stderr.String())
		}
		if strings.Contains(string(out), adcEnvVar) {
			t.Errorf("gcpemu env overrode a user-set %s:\n%s", adcEnvVar, out)
		}
		if !strings.Contains(stderr.String(), key) || !strings.Contains(stderr.String(), "googleapis.com universe") {
			t.Errorf("gcpemu env did not warn about %s: %q", key, stderr.String())
		}
	})
}

const adcEnvVar = "GOOGLE_APPLICATION_CREDENTIALS"
