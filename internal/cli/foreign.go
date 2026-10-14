package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/instance"
)

// Foreign credentials (see CONTEXT.md): credentials no Instance issued,
// whose token requests go to real Google (#247).

const adcEnv = "GOOGLE_APPLICATION_CREDENTIALS"

// googleUniverse is what clients assume when a credentials file names no
// universe.
const googleUniverse = "googleapis.com"

// foreignCredsFile reports why the credentials file at path holds Foreign
// credentials, or "" when it is in the Invalid universe.
func foreignCredsFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var f struct {
		Type     string `json:"type"`
		Universe string `json:"universe_domain"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return "", fmt.Errorf("%s is not a credentials file: %w", path, err)
	}
	if f.Universe == emu.InvalidUniverse {
		return "", nil
	}
	u := f.Universe
	if u == "" {
		u = googleUniverse
	}
	return fmt.Sprintf("%s holds %s credentials in the %s universe, not issued by gcpemu", path, f.Type, u), nil
}

// wellKnownADCPaths are where ADC looks for gcloud's `auth
// application-default login` credentials, which it uses before the
// metadata server: Python honours CLOUDSDK_CONFIG, Go only the home
// directory.
func wellKnownADCPaths(goos string, getenv func(string) string) []string {
	const name = "application_default_credentials.json"
	var out []string
	if d := getenv("CLOUDSDK_CONFIG"); d != "" {
		out = append(out, filepath.Join(d, name))
	}
	if goos == "windows" {
		if d := getenv("APPDATA"); d != "" {
			out = append(out, filepath.Join(d, "gcloud", name))
		}
	} else if d := getenv("HOME"); d != "" {
		out = append(out, filepath.Join(d, ".config", "gcloud", name))
	}
	return out
}

// wellKnownADC returns the first of wellKnownADCPaths that exists.
func wellKnownADC() string {
	for _, p := range wellKnownADCPaths(runtime.GOOS, os.Getenv) {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// guardADC adds GOOGLE_APPLICATION_CREDENTIALS to vars when gcloud's
// application-default credentials would otherwise win over the metadata
// server (ADR 0010), and warns on w about a user-set one holding Foreign
// credentials. It never overrides one the user set.
func guardADC(c *adminClient, vars map[string]string, w io.Writer) {
	// A key from an earlier `gcpemu env` is renewed: Reset removes its key,
	// and an Ephemeral Instance's data directory goes when it exits.
	if p := os.Getenv(adcEnv); p != "" && filepath.Base(p) != instance.ADCKeyFile {
		if why, err := foreignCredsFile(p); err == nil && why != "" {
			fmt.Fprintf(w, "# gcpemu: %s=%s\n#   %s.\n#   Its token requests go to real Google; unset it, or use a key from `gcloud iam service-accounts keys create` against gcpemu.\n", adcEnv, p, why)
		}
		return
	}
	wk := wellKnownADC()
	if wk == "" {
		return
	}
	code, b, err := c.do(http.MethodPost, "/_emu/v1/adc-key")
	var res struct {
		Path  string `json:"path"`
		Error string `json:"error"`
	}
	if err == nil {
		_ = json.Unmarshal(b, &res)
	}
	if err != nil || code != http.StatusOK || res.Path == "" {
		why := res.Error
		if err != nil {
			why = err.Error()
		}
		fmt.Fprintf(w, "# gcpemu: ADC would use gcloud's application-default credentials at %s, which go to real Google, and no emulator key could be written: %s\n", wk, why)
		return
	}
	vars[adcEnv] = res.Path
	fmt.Fprintf(w, "# gcpemu: %s points at an emulator key, so ADC does not use gcloud's application-default credentials at %s, which go to real Google.\n", adcEnv, wk)
}
