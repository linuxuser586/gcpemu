//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// tofu runs OpenTofu on a copy of the stack module.
type tofu struct {
	t    *testing.T
	bin  string
	dir  string
	env  []string
	vars map[string]string
}

// newTofu copies stack/ into a temporary working directory. Providers are
// cached across runs in the user cache directory (TF_PLUGIN_CACHE_DIR).
func newTofu(t *testing.T, bin, gateway string) *tofu {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "stack")
	err := filepath.WalkDir("stack", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel("stack", p)
		if d.IsDir() {
			if d.Name() == ".terraform" {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dir, rel), 0o755)
		}
		if strings.HasPrefix(d.Name(), "terraform.tfstate") || d.Name() == ".terraform.lock.hcl" {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "TF_IN_AUTOMATION=1", "TF_INPUT=0", "CHECKPOINT_DISABLE=1")
	if os.Getenv("TF_PLUGIN_CACHE_DIR") == "" {
		cache := filepath.Join(cacheDir(t), "tofu-plugins")
		if err := os.MkdirAll(cache, 0o755); err != nil {
			t.Fatal(err)
		}
		env = append(env, "TF_PLUGIN_CACHE_DIR="+cache)
	}
	return &tofu{t: t, bin: bin, dir: dir, env: env, vars: map[string]string{"emulator_gateway": "http://" + gateway}}
}

// cacheDir is the directory for downloads reused across runs
// (GCPEMU_E2E_CACHE, default <user cache dir>/gcpemu-e2e).
func cacheDir(t *testing.T) string {
	if d := os.Getenv("GCPEMU_E2E_CACHE"); d != "" {
		return d
	}
	d, err := os.UserCacheDir()
	if err != nil {
		d = os.TempDir()
	}
	return filepath.Join(d, "gcpemu-e2e")
}

// run executes tofu with args and returns stdout; it fails the test on
// error, showing the end of the output.
func (tf *tofu) run(timeout time.Duration, args ...string) string {
	tf.t.Helper()
	out, err := tf.try(timeout, args...)
	if err != nil {
		tf.t.Fatalf("tofu %s: %v\n%s", args[0], err, tail(out, 60))
	}
	return out
}

func (tf *tofu) try(timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	full := append([]string{"-chdir=" + tf.dir}, args...)
	if args[0] == "apply" || args[0] == "destroy" || args[0] == "plan" {
		full = append(full, "-no-color")
		for _, k := range sortedKeys(tf.vars) {
			full = append(full, "-var", k+"="+tf.vars[k])
		}
	}
	cmd := exec.CommandContext(ctx, tf.bin, full...)
	cmd.Env = tf.env
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	start := time.Now()
	err := cmd.Run()
	tf.t.Logf("tofu %s (%v)", strings.Join(args, " "), time.Since(start).Round(100*time.Millisecond))
	return buf.String(), err
}

// apply sets vars and applies.
func (tf *tofu) apply(vars map[string]string) {
	tf.t.Helper()
	for k, v := range vars {
		tf.vars[k] = v
	}
	tf.run(15*time.Minute, "apply", "-auto-approve")
}

// planClean asserts that a plan right after apply shows no changes
// (Section 11.1, OpenTofu acceptance).
func (tf *tofu) planClean() {
	tf.t.Helper()
	out, err := tf.try(5*time.Minute, "plan", "-detailed-exitcode")
	if err != nil {
		var changes []string
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "  # ") || strings.Contains(l, "forces replacement") {
				changes = append(changes, strings.TrimSpace(l))
			}
		}
		tf.t.Errorf("plan after apply is not empty: %v\n%s", err, strings.Join(changes, "\n"))
	}
}

// outputs returns the module outputs (sensitive ones included).
func (tf *tofu) outputs() map[string]string {
	tf.t.Helper()
	var raw map[string]struct {
		Value any `json:"value"`
	}
	if err := json.Unmarshal([]byte(tf.run(time.Minute, "output", "-json")), &raw); err != nil {
		tf.t.Fatal(err)
	}
	out := map[string]string{}
	for k, v := range raw {
		out[k] = fmt.Sprint(v.Value)
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
