//go:build compat

package compat

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/escapeguard"
)

// SDK library versions the universe probes install into the compat cache.
var (
	pythonPins = []string{"google-cloud-storage==3.17.0", "google-cloud-pubsub==2.42.0"}
	nodePins   = []string{"@google-cloud/pubsub@6.3.0"}
)

// pinDir is the cache directory for a set of pinned packages.
func pinDir(t *testing.T, kind string, pins []string) string {
	sum := sha256.Sum256([]byte(strings.Join(pins, "\n")))
	return filepath.Join(cacheDir(t), kind+"-"+hex.EncodeToString(sum[:])[:12])
}

// pythonLibs returns a Python interpreter with pythonPins installed,
// creating the virtual environment once, or skips the test.
func pythonLibs(t *testing.T) string {
	t.Helper()
	py := onPath(t, "python3")
	dir := pinDir(t, "python", pythonPins)
	bin := filepath.Join(dir, "bin", "python")
	if _, err := os.Stat(filepath.Join(dir, ".done")); err == nil {
		return bin
	}
	_ = os.RemoveAll(dir)
	if out, err := exec.Command(py, "-m", "venv", dir).CombinedOutput(); err != nil {
		t.Skipf("python3 -m venv: %v\n%s", err, out)
	}
	if out, err := exec.Command(bin, append([]string{"-m", "pip", "install", "-q"}, pythonPins...)...).CombinedOutput(); err != nil {
		t.Skipf("pip install: %v\n%s", err, out)
	}
	_ = os.WriteFile(filepath.Join(dir, ".done"), nil, 0o644)
	return bin
}

// nodeLibs returns node and a node_modules directory with nodePins
// installed, installing them once, or skips the test.
func nodeLibs(t *testing.T) (node, modules string) {
	t.Helper()
	node, npm := onPath(t, "node"), onPath(t, "npm")
	dir := pinDir(t, "node", nodePins)
	modules = filepath.Join(dir, "node_modules")
	if _, err := os.Stat(filepath.Join(dir, ".done")); err == nil {
		return node, modules
	}
	_ = os.RemoveAll(dir)
	if out, err := exec.Command(npm, append([]string{"install", "--silent", "--prefix", dir}, nodePins...)...).CombinedOutput(); err != nil {
		t.Skipf("npm install: %v\n%s", err, out)
	}
	_ = os.WriteFile(filepath.Join(dir, ".done"), nil, 0o644)
	return node, modules
}

// withEnv returns env without the unset variables and with set applied.
func withEnv(env []string, set map[string]string, unset ...string) []string {
	out := slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		_, replaced := set[k]
		return replaced || slices.Contains(unset, k)
	})
	for k, v := range set {
		out = append(out, k+"="+v)
	}
	return out
}

// emulatorKey creates a service account and returns the path of a JSON key
// the emulator issued for it.
func emulatorKey(t *testing.T, e *emulator, accountID string) string {
	t.Helper()
	call := func(path string, body any) []byte {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", "http://"+e.vars["GCPEMU_GATEWAY"]+"/iam/v1/projects/"+project+path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer owner")
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		if resp.StatusCode/100 != 2 {
			t.Fatalf("POST %s: %d %s", path, resp.StatusCode, out)
		}
		return out
	}
	sa := accountID + "@" + project + ".iam.gserviceaccount.com"
	call("/serviceAccounts", map[string]string{"accountId": accountID})
	var key struct{ PrivateKeyData string }
	if err := json.Unmarshal(call("/serviceAccounts/"+sa+"/keys", map[string]any{}), &key); err != nil {
		t.Fatal(err)
	}
	kf, err := base64.StdEncoding.DecodeString(key.PrivateKeyData)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), accountID+".json")
	if err := os.WriteFile(p, kf, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// probeResults runs a probe script from sdk/ and returns each case's
// output line ("OK ..." or "ERR ...") by case name.
func probeResults(t *testing.T, e *emulator, env []string, bin string, args ...string) map[string]string {
	t.Helper()
	out, _, err := e.exec(env, bin, args...)
	got := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		if name, rest, ok := strings.Cut(sc.Text(), " "); ok && (strings.HasPrefix(rest, "OK") || strings.HasPrefix(rest, "ERR")) {
			got[name] = rest
		}
	}
	if err != nil && len(got) == 0 {
		t.Fatalf("%s: %v\n%s", filepath.Base(bin), err, out)
	}
	return got
}

// expectProbe checks that a case succeeded (want "OK") or failed with an
// error containing want.
func expectProbe(t *testing.T, got map[string]string, name, want string) {
	t.Helper()
	r, ok := got[name]
	switch {
	case !ok:
		t.Errorf("%s: no result", name)
	case want == "OK" && !strings.HasPrefix(r, "OK"):
		t.Errorf("%s: want success, got %s", name, r)
	case want != "OK" && (!strings.HasPrefix(r, "ERR") || !strings.Contains(r, want)):
		t.Errorf("%s: want an error containing %q, got %s", name, want, r)
	}
}

// testUniverse covers #215: emulator keys and GOOGLE_CLOUD_UNIVERSE_DOMAIN
// put gcloud and the client libraries in the Invalid universe. The probes
// pin down the per-library behaviour the README documents.
func testUniverse(t *testing.T, e *emulator) {
	const mismatch = "does not match the universe domain found in the credentials (gcpemu.invalid)"
	const noOverride = "pubsub.gcpemu.invalid"

	t.Run("gcloud-key", func(t *testing.T) {
		g := onPath(t, "gcloud")
		key := emulatorKey(t, e, "universe-gcloud")
		// A fresh configuration without the token file, so gcloud uses
		// only the key.
		env := withEnv(e.env, map[string]string{"CLOUDSDK_CONFIG": t.TempDir()}, "CLOUDSDK_AUTH_ACCESS_TOKEN_FILE")
		for _, args := range [][]string{
			{"auth", "activate-service-account", "--key-file=" + key},
			{"pubsub", "topics", "list"},
		} {
			out, escaped, err := e.exec(env, g, args...)
			for _, h := range escaped {
				t.Errorf("gcloud %s: %s", args[0], escapeguard.DescribeGcloud(h))
			}
			if err != nil {
				t.Fatalf("gcloud %s: %v\n%s", strings.Join(args, " "), err, out)
			}
		}
	})

	t.Run("python", func(t *testing.T) {
		py := pythonLibs(t)
		key := emulatorKey(t, e, "universe-python")
		env := withEnv(e.env, map[string]string{
			"GOOGLE_APPLICATION_CREDENTIALS": key,
			// The Python storage client needs a scheme here, which
			// `gcpemu env` leaves out.
			"STORAGE_EMULATOR_HOST": "http://" + e.vars["STORAGE_EMULATOR_HOST"],
		})
		got := probeResults(t, e, env, py, "-I", "sdk/universe.py", key, project, "1")
		expectProbe(t, got, "storage-adc", "OK")
		expectProbe(t, got, "storage-explicit", mismatch)
		expectProbe(t, got, "storage-explicit-universe", "OK")
		expectProbe(t, got, "pubsub-emulator", "OK")
		expectProbe(t, got, "pubsub-no-override", noOverride)
	})

	t.Run("node", func(t *testing.T) {
		node, modules := nodeLibs(t)
		key := emulatorKey(t, e, "universe-node")
		env := withEnv(e.env, map[string]string{"GOOGLE_APPLICATION_CREDENTIALS": key, "NODE_PATH": modules})
		got := probeResults(t, e, env, node, "sdk/universe.js", project, "1")
		expectProbe(t, got, "pubsub-emulator", mismatch)
		expectProbe(t, got, "pubsub-emulator-universe", "OK")
		expectProbe(t, got, "pubsub-no-override", noOverride)
	})
}
