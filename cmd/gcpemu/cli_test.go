package main_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// bin is the gcpemu binary built for these tests.
var bin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gcpemu-cli-test-")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(dir, "gcpemu")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		panic("go build: " + err.Error() + "\n" + string(out))
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// gcpemu runs the binary with an instance's data dir and returns its
// combined output.
func gcpemu(t *testing.T, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(append(os.Environ(), "CI=", "GCPEMU_INSTANCE="), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func must(t *testing.T, env []string, args ...string) string {
	t.Helper()
	out, err := gcpemu(t, env, args...)
	if err != nil {
		t.Fatalf("gcpemu %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func startDetached(t *testing.T, dir string) {
	t.Helper()
	out := must(t, nil, "start", "--detach", "--data-dir", dir, "--services", "gcs,pubsub", "--port", "0", "--wait-timeout", "60s")
	if !strings.Contains(out, "ready") {
		t.Fatalf("start --detach: %s", out)
	}
	t.Cleanup(func() { _, _ = gcpemu(t, nil, "stop", "--data-dir", dir) })
}

// TestCLILifecycle is FR-CORE-001/002/004 end to end: start --detach,
// status, env in every shell format, logs, reset, version and stop.
func TestCLILifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	dir := t.TempDir()
	startDetached(t, dir)

	st := must(t, nil, "status", "--data-dir", dir)
	for _, want := range []string{"services:", "gcs      ready", "pubsub   ready", "gateway"} {
		if !strings.Contains(st, want) {
			t.Errorf("status lacks %q:\n%s", want, st)
		}
	}
	var sj struct {
		Info struct {
			Pid       int
			Endpoints map[string]string
		}
		Ready struct{ Ready bool }
	}
	if err := json.Unmarshal([]byte(must(t, nil, "status", "--json", "--data-dir", dir)), &sj); err != nil || !sj.Ready.Ready || sj.Info.Pid == 0 {
		t.Fatalf("status --json = %+v, %v", sj, err)
	}
	gcs := sj.Info.Endpoints["gcs"]

	bash := must(t, nil, "env", "--data-dir", dir)
	if !strings.Contains(bash, "export STORAGE_EMULATOR_HOST='"+gcs+"'") || !strings.Contains(bash, "export PUBSUB_EMULATOR_HOST=") {
		t.Errorf("env (bash):\n%s", bash)
	}
	if fish := must(t, nil, "env", "--shell", "fish", "--data-dir", dir); !strings.Contains(fish, "set -gx STORAGE_EMULATOR_HOST '"+gcs+"';") {
		t.Errorf("env --shell fish:\n%s", fish)
	}
	ghEnv := filepath.Join(t.TempDir(), "github_env")
	_ = os.WriteFile(ghEnv, []byte("EXISTING=1\n"), 0o644)
	must(t, []string{"GITHUB_ENV=" + ghEnv}, "env", "--shell", "github", "--data-dir", dir)
	if b, _ := os.ReadFile(ghEnv); !strings.HasPrefix(string(b), "EXISTING=1\n") || !strings.Contains(string(b), "\nSTORAGE_EMULATOR_HOST="+gcs+"\n") {
		t.Errorf("$GITHUB_ENV:\n%s", b)
	}
	if _, err := gcpemu(t, nil, "env", "--shell", "tcsh", "--data-dir", dir); err == nil {
		t.Error("unknown shell accepted")
	}

	// A bucket, then reset wipes it.
	resp, err := http.Post("http://"+gcs+"/storage/v1/b?project=cli-proj", "application/json", strings.NewReader(`{"name":"cli-bucket"}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("create bucket: %v %v", resp, err)
	}
	resp.Body.Close()
	listBuckets := func() string {
		resp, err := http.Get("http://" + gcs + "/storage/v1/b?project=cli-proj")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var l struct{ Items []struct{ Name string } }
		_ = json.NewDecoder(resp.Body).Decode(&l)
		var names []string
		for _, b := range l.Items {
			names = append(names, b.Name)
		}
		return strings.Join(names, ",")
	}
	if got := listBuckets(); got != "cli-bucket" {
		t.Fatalf("buckets = %q", got)
	}
	if logs := must(t, nil, "logs", "--requests", "--data-dir", dir); !strings.Contains(logs, "/storage/v1/b") {
		t.Errorf("logs --requests:\n%s", logs)
	}
	if logs := must(t, nil, "logs", "--data-dir", dir); !strings.Contains(logs, "ready") {
		t.Errorf("logs:\n%s", logs)
	}
	must(t, nil, "reset", "--data-dir", dir)
	if got := listBuckets(); got != "" {
		t.Errorf("buckets after reset = %q", got)
	}

	if v := must(t, nil, "version"); strings.TrimSpace(v) == "" {
		t.Error("empty version")
	}

	// FR-UI-002: the console is served on the gateway, at the URL that
	// `gcpemu console` opens.
	consoleURL := strings.TrimSpace(must(t, nil, "console", "--print", "--data-dir", dir))
	if consoleURL != "http://"+sj.Info.Endpoints["gateway"]+"/console/" {
		t.Errorf("console --print = %q", consoleURL)
	}
	if resp, err := http.Get(consoleURL); err != nil || resp.StatusCode != 200 {
		t.Errorf("GET %s: %v %v", consoleURL, resp, err)
	} else {
		resp.Body.Close()
	}

	if out := must(t, nil, "stop", "--data-dir", dir); !strings.Contains(out, "stopped") {
		t.Errorf("stop: %s", out)
	}
	if err := syscall.Kill(sj.Info.Pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("pid %d still exists after stop: %v", sj.Info.Pid, err)
	}
	if _, err := gcpemu(t, nil, "status", "--data-dir", dir); err == nil {
		t.Error("status succeeded after stop")
	}
	if out, err := gcpemu(t, nil, "console", "--print", "--data-dir", dir); err == nil || !strings.Contains(out, "not running") {
		t.Errorf("console after stop: %v %s", err, out)
	}
}

// TestSIGTERMShutdown is FR-CORE-006: SIGTERM shuts a detached instance
// down gracefully within 30 s.
func TestSIGTERMShutdown(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	dir := t.TempDir()
	startDetached(t, dir)
	var sj struct{ Info struct{ Pid int } }
	if err := json.Unmarshal([]byte(must(t, nil, "status", "--json", "--data-dir", dir)), &sj); err != nil || sj.Info.Pid == 0 {
		t.Fatalf("status --json: %v", err)
	}
	start := time.Now()
	if err := syscall.Kill(sj.Info.Pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	for syscall.Kill(sj.Info.Pid, 0) == nil {
		if time.Since(start) > 30*time.Second {
			t.Fatalf("still running 30 s after SIGTERM")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if logs := must(t, nil, "logs", "--data-dir", dir); !strings.Contains(strings.ToLower(logs), "shut") {
		t.Errorf("no shutdown in the log:\n%s", logs)
	}
}

// endpoints returns a running instance's endpoints by name.
func endpoints(t *testing.T, dir string) map[string]string {
	t.Helper()
	var sj struct {
		Info struct{ Endpoints map[string]string }
	}
	if err := json.Unmarshal([]byte(must(t, nil, "status", "--json", "--data-dir", dir)), &sj); err != nil {
		t.Fatal(err)
	}
	return sj.Info.Endpoints
}

func code(t *testing.T, method, url, body string) int {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return -1
	}
	resp.Body.Close()
	return resp.StatusCode
}

// TestFaultsFromCLIAndSeed is FR-CORE-060: rules added with `gcpemu fault
// add` and declared in a seed file fire as configured.
func TestFaultsFromCLIAndSeed(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	dir := t.TempDir()
	seed := filepath.Join(t.TempDir(), "seed.yaml")
	_ = os.WriteFile(seed, []byte("faults:\n  - id: seeded\n    service: pubsub\n    method: ':publish$'\n    code: RESOURCE_EXHAUSTED\n"), 0o644)
	out := must(t, nil, "start", "--detach", "--data-dir", dir, "--services", "gcs,pubsub", "--port", "0", "--seed", seed, "--wait-timeout", "60s")
	if !strings.Contains(out, "ready") {
		t.Fatal(out)
	}
	t.Cleanup(func() { _, _ = gcpemu(t, nil, "stop", "--data-dir", dir) })
	eps := endpoints(t, dir)
	gw, gcs := "http://"+eps["gateway"], "http://"+eps["gcs"]

	if c := code(t, "PUT", gw+"/pubsub/v1/projects/fault-proj/topics/topic1", "{}"); c != 200 {
		t.Fatalf("create topic: %d", c)
	}
	if c := code(t, "POST", gw+"/pubsub/v1/projects/fault-proj/topics/topic1:publish", `{"messages":[{"data":"eA=="}]}`); c != 429 {
		t.Errorf("publish with the seeded fault: %d, want 429", c)
	}

	must(t, nil, "fault", "add", "--data-dir", dir, "--id", "once", "--service", "gcs", "--code", "UNAVAILABLE", "--count", "1")
	if l := must(t, nil, "fault", "list", "--data-dir", dir); !strings.Contains(l, `"once"`) || !strings.Contains(l, `"seeded"`) {
		t.Errorf("fault list:\n%s", l)
	}
	bucket := gcs + "/storage/v1/b?project=fault-proj"
	if c := code(t, "GET", bucket, ""); c != 503 {
		t.Errorf("first gcs call: %d, want 503", c)
	}
	if c := code(t, "GET", bucket, ""); c != 200 {
		t.Errorf("second gcs call: %d, want 200 (count 1)", c)
	}
	must(t, nil, "fault", "clear", "seeded", "--data-dir", dir)
	if c := code(t, "POST", gw+"/pubsub/v1/projects/fault-proj/topics/topic1:publish", `{"messages":[{"data":"eA=="}]}`); c != 200 {
		t.Errorf("publish after clearing: %d", c)
	}
	must(t, nil, "fault", "add", "--data-dir", dir, "--service", "gcs", "--drop")
	if c := code(t, "GET", bucket, ""); c != -1 {
		t.Errorf("dropped connection answered %d", c)
	}
	must(t, nil, "fault", "clear", "--data-dir", dir)
	if l := must(t, nil, "fault", "list", "--data-dir", dir); strings.Contains(l, `"id"`) {
		t.Errorf("rules left after clear:\n%s", l)
	}
}

// TestSIGKILLDurability is NFR-REL-001: acknowledged writes (a bucket, an
// object, a topic, a subscription and a published message) survive the
// emulator being killed with SIGKILL.
func TestSIGKILLDurability(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	dir := t.TempDir()
	startDetached(t, dir)
	eps := endpoints(t, dir)
	gw, gcs := "http://"+eps["gateway"], "http://"+eps["gcs"]
	for _, c := range []struct{ method, url, body string }{
		{"POST", gcs + "/storage/v1/b?project=kill-proj", `{"name":"kill-bucket"}`},
		{"POST", gcs + "/upload/storage/v1/b/kill-bucket/o?uploadType=media&name=obj", "durable"},
		{"PUT", gw + "/pubsub/v1/projects/kill-proj/topics/topic1", "{}"},
		{"PUT", gw + "/pubsub/v1/projects/kill-proj/subscriptions/sub1", `{"topic":"projects/kill-proj/topics/topic1"}`},
		{"POST", gw + "/pubsub/v1/projects/kill-proj/topics/topic1:publish", `{"messages":[{"data":"ZHVyYWJsZQ=="}]}`},
	} {
		if got := code(t, c.method, c.url, c.body); got != 200 {
			t.Fatalf("%s %s: %d", c.method, c.url, got)
		}
	}
	var sj struct{ Info struct{ Pid int } }
	_ = json.Unmarshal([]byte(must(t, nil, "status", "--json", "--data-dir", dir)), &sj)
	if err := syscall.Kill(sj.Info.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for syscall.Kill(sj.Info.Pid, 0) == nil {
		time.Sleep(20 * time.Millisecond)
	}

	startDetached(t, dir)
	eps = endpoints(t, dir)
	gw, gcs = "http://"+eps["gateway"], "http://"+eps["gcs"]
	resp, err := http.Get(gcs + "/storage/v1/b/kill-bucket/o/obj?alt=media")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "durable" {
		t.Errorf("object after SIGKILL: %d %q", resp.StatusCode, b)
	}
	req, _ := http.NewRequest("POST", gw+"/pubsub/v1/projects/kill-proj/subscriptions/sub1:pull", strings.NewReader(`{"maxMessages":10}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "ZHVyYWJsZQ==") {
		t.Errorf("message after SIGKILL: %d %s", resp.StatusCode, b)
	}
}
