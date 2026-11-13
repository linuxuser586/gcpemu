package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/filelock"
	"github.com/linuxuser586/gcpemu/internal/instance"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// mainEnv makes the test binary run main instead of the tests, so a test
// can run gcpemu-testdir as its own process.
const mainEnv = "GCPEMU_TESTDIR_MAIN"

// TestMain runs main instead of the tests when mainEnv is set.
func TestMain(m *testing.M) {
	if os.Getenv(mainEnv) != "" {
		main()
		return
	}
	os.Exit(m.Run())
}

// testdir runs gcpemu-testdir with args. It is killed when t ends.
func testdir(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), mainEnv+"=1")
	t.Cleanup(func() {
		if cmd.Process != nil && cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	return cmd
}

// startHold runs `gcpemu-testdir hold dir` and returns once it reports
// that it holds the lock, with the pipe to its stdin.
func startHold(t *testing.T, dir string) (*exec.Cmd, *os.File) {
	t.Helper()
	cmd := testdir(t, "hold", dir)
	stdin, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdin = stdin
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stdin.Close()
	t.Cleanup(func() { w.Close() })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != heldLine {
		t.Fatalf("hold printed %q, %v; want held", line, err)
	}
	return cmd, w
}

// lockFree reports whether dir's owner lock can be taken: its test has
// ended.
func lockFree(t *testing.T, dir string) bool {
	t.Helper()
	release, err := filelock.TryLock(filepath.Join(dir, instance.OwnerLockFile), 0)
	if errors.Is(err, filelock.ErrHeld) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	release()
	return true
}

// waitExit waits for cmd to exit, failing t after a few seconds.
func waitExit(t *testing.T, cmd *exec.Cmd) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("gcpemu-testdir did not exit")
		return nil
	}
}

// TestHoldUntilStdinCloses is #1035: hold makes dir a Test data dir whose
// test runs until the harness closes the helper's stdin, as it does when
// it dies however it dies, and then a reclaim can take it.
func TestHoldUntilStdinCloses(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	cmd, stdin := startHold(t, dir)
	if lockFree(t, dir) {
		t.Fatal("the owner lock is free while hold runs")
	}
	stdin.Close()
	if err := waitExit(t, cmd); err != nil {
		t.Fatalf("hold after its stdin closed: %v", err)
	}
	if !lockFree(t, dir) {
		t.Error("the owner lock is still held after hold exited")
	}
}

// TestHoldKilled is #1035: on Windows, Playwright ends the harness's
// process tree with taskkill /T /F, which kills hold too. The OS then
// frees the lock, so the dir counts as a Test data dir whose test ended.
func TestHoldKilled(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	cmd, _ := startHold(t, dir)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = waitExit(t, cmd)
	if !lockFree(t, dir) {
		t.Error("the owner lock is still held after hold was killed")
	}
}

// fakeDocker is a Docker Engine API serving the containers, networks and
// volumes in objs, by kind and name, and recording what was removed.
type fakeDocker struct {
	mu      sync.Mutex
	objs    map[string]map[string]map[string]string // kind, name: labels
	removed []string
}

// tag serves a running container, a network and a volume of the Instance
// id whose dir is dir.
func (d *fakeDocker) tag(id, dir string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	l := map[string]string{runtime.LabelInstance: id, runtime.LabelDir: dir}
	for kind := range d.objs {
		d.objs[kind][kind[:1]+"-"+id] = l
	}
}

// removedOf returns what was removed of the Instance id, in order.
func (d *fakeDocker) removedOf(id string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var got []string
	for _, r := range d.removed {
		if strings.HasSuffix(r, "-"+id) {
			got = append(got, r)
		}
	}
	return got
}

// ServeHTTP serves the parts of the Engine API that detection, FindTagged
// and Prune use.
func (d *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/v1.41")
	switch p {
	case "/version", "/_ping":
		_ = json.NewEncoder(w).Encode(map[string]any{})
		return
	case "/info":
		_ = json.NewEncoder(w).Encode(map[string]any{"OSType": "linux"})
		return
	}
	kind, name, _ := strings.Cut(strings.TrimPrefix(p, "/"), "/")
	objs := d.objs[kind]
	if r.Method == http.MethodDelete {
		if _, ok := objs[name]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		delete(objs, name)
		d.removed = append(d.removed, kind+":"+name)
		return
	}
	name = strings.TrimSuffix(name, "/json")
	if name == "" || name == "json" {
		list := []map[string]any{}
		for n, l := range objs {
			list = append(list, map[string]any{"Id": n, "Name": n, "Labels": l})
		}
		if kind == "volumes" {
			_ = json.NewEncoder(w).Encode(map[string]any{"Volumes": list})
			return
		}
		_ = json.NewEncoder(w).Encode(list)
		return
	}
	l, ok := objs[name]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if kind == "containers" {
		_ = json.NewEncoder(w).Encode(map[string]any{"Id": name, "Name": "/" + name,
			"Config": map[string]any{"Labels": l}, "State": map[string]any{"Running": true}})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"Id": name, "Name": name, "Labels": l})
}

// useFakeDocker serves d to the gcpemu-testdir processes t starts.
func useFakeDocker(t *testing.T) *fakeDocker {
	t.Helper()
	d := &fakeDocker{objs: map[string]map[string]map[string]string{"containers": {}, "networks": {}, "volumes": {}}}
	srv := httptest.NewServer(d)
	t.Cleanup(srv.Close)
	t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(srv.URL, "http://"))
	return d
}

// endedDir makes a Test data dir under root that holds the Instance id and
// whose test has ended: its owner lock file exists and is free.
func endedDir(t *testing.T, root, id string) string {
	t.Helper()
	dir := filepath.Join(root, id)
	release, err := instance.HoldOwnerLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if err := os.WriteFile(filepath.Join(dir, instance.IDFile), []byte(id), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestReclaim is #1035: reclaim removes the Test data dirs under root
// whose test has ended, with their Instance's containers, networks and
// volumes, including a dir whose Instance made none, and leaves a dir
// whose test still runs alone.
func TestReclaim(t *testing.T) {
	d := useFakeDocker(t)
	root := t.TempDir()
	ended := endedDir(t, root, "ended")
	bare := endedDir(t, root, "bare")
	owned := filepath.Join(root, "owned")
	startHold(t, owned)
	if err := os.WriteFile(filepath.Join(owned, instance.IDFile), []byte("owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	d.tag("ended", ended)
	d.tag("owned", owned)

	out, err := testdir(t, "reclaim", root).CombinedOutput()
	if err != nil {
		t.Fatalf("reclaim: %v\n%s", err, out)
	}
	want := []string{"containers:c-ended", "networks:n-ended", "volumes:v-ended"}
	if got := d.removedOf("ended"); !slices.Equal(got, want) {
		t.Errorf("removed %v of ended; want %v", got, want)
	}
	for _, dir := range []string{ended, bare} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s after the reclaim: %v, want it removed", dir, err)
		}
	}
	if got := d.removedOf("owned"); len(got) > 0 {
		t.Errorf("the reclaim removed %v of owned, whose test still runs", got)
	}
	if _, err := os.Stat(owned); err != nil {
		t.Errorf("owned after the reclaim: %v", err)
	}
}

// TestReclaimNoRuntime is #1035: with no container runtime, reclaim keeps
// a dir whose test has ended, because its objects may be on the runtime
// and only the dir records whose they are, and fails so that the harness
// can warn.
func TestReclaimNoRuntime(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	root := t.TempDir()
	bare := endedDir(t, root, "bare")
	out, err := testdir(t, "reclaim", root).CombinedOutput()
	if err == nil {
		t.Fatalf("reclaim with no runtime succeeded:\n%s", out)
	}
	if !strings.Contains(string(out), "no container runtime") {
		t.Errorf("reclaim output %q does not say there is no container runtime", out)
	}
	if _, err := os.Stat(bare); err != nil {
		t.Errorf("dir after a reclaim with no runtime: %v, want it kept", err)
	}
}
