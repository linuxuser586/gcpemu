package instance

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/gateway"
)

// sleeperEnv makes TestReclaimSleeper sleep, as a stand-in for the
// process of a CLI-started Instance.
const sleeperEnv = "GCPEMU_TEST_RECLAIM_SLEEPER"

func TestReclaimSleeper(t *testing.T) {
	if os.Getenv(sleeperEnv) == "" {
		t.Skip("run by the reclaim tests")
	}
	time.Sleep(10 * time.Minute)
}

// sleeper is a running TestReclaimSleeper whose executable is exe.
type sleeper struct {
	cmd    *exec.Cmd
	exited chan struct{}
}

// startSleeper runs TestReclaimSleeper from a copy of the test binary
// named exe in its own dir ("" runs the test binary itself), and records
// its pid in dir's gcpemu.pid. It is killed when t ends.
func startSleeper(t *testing.T, dir, exe string) *sleeper {
	t.Helper()
	bin := os.Args[0]
	if exe != "" {
		b, err := os.ReadFile(bin)
		if err != nil {
			t.Fatal(err)
		}
		if goruntime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(exe), ".exe") {
			exe += ".exe" // Windows runs only .exe files
		}
		bin = filepath.Join(t.TempDir(), exe)
		if err := os.WriteFile(bin, b, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(bin, "-test.run=^TestReclaimSleeper$")
	cmd.Env = append(os.Environ(), sleeperEnv+"=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s := &sleeper{cmd: cmd, exited: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(s.exited) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-s.exited })
	if err := os.WriteFile(filepath.Join(dir, PIDFile), []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	return s
}

// alive reports whether the sleeper still runs.
func (s *sleeper) alive() bool {
	select {
	case <-s.exited:
		return false
	case <-time.After(100 * time.Millisecond):
		return true
	}
}

// endedTest is a Test data dir whose test has ended, holding ID "ended",
// with a running container, a network and a volume.
func endedTest(t *testing.T) (*fakeRuntime, Abandoned) {
	t.Helper()
	dir := testDataDir(t, filepath.Join(t.TempDir(), "ended"), "ended")
	f := &fakeRuntime{
		containers: map[string]fakeObj{"c-ended": {labels: labelled("ended", dir), running: true}},
		networks:   map[string]fakeObj{"n-ended": {labels: labelled("ended", dir)}},
		volumes:    map[string]fakeObj{"v-ended": {labels: labelled("ended", dir)}},
	}
	abandoned, err := FindAbandoned(context.Background(), fakeClient(t, f), filepath.Join(t.TempDir(), "state"))
	if err != nil || len(abandoned) != 1 {
		t.Fatalf("FindAbandoned = %+v, %v; want the ended test", abandoned, err)
	}
	return f, abandoned[0]
}

// assertReclaimed fails t unless a's objects were removed, containers
// first, and its Test data dir is gone.
func assertReclaimed(t *testing.T, f *fakeRuntime, a Abandoned) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if want := []string{"containers:c-ended", "networks:n-ended", "volumes:v-ended"}; !slices.Equal(f.removed, want) {
		t.Errorf("removed %v; want %v", f.removed, want)
	}
	for _, d := range a.EndedTestDirs {
		if _, err := os.Stat(d); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Test data dir %s after the reclaim: %v, want it removed", d, err)
		}
	}
}

// TestReclaimEndedTest is #914: reclaiming an ended test removes its
// objects, running container included, and its dir, after which it is no
// longer Abandoned; two reclaims at once both succeed.
func TestReclaimEndedTest(t *testing.T) {
	f, a := endedTest(t)
	cl := fakeClient(t, f)
	ctx := context.Background()
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			if err := a.Reclaim(ctx, cl); err != nil {
				t.Errorf("Reclaim: %v", err)
			}
		})
	}
	wg.Wait()
	assertReclaimed(t, f, a)
	if again, err := FindAbandoned(ctx, cl, filepath.Join(t.TempDir(), "state")); err != nil || len(again) != 0 {
		t.Errorf("after the reclaim: %+v, %v", again, err)
	}
}

// TestReclaimStopsInstanceProcess: a process still serving the ended
// test's Instance is asked to stop through the dir's endpoints, with its
// Admin token, once they serve that Instance, before anything is removed.
func TestReclaimStopsInstanceProcess(t *testing.T) {
	f, a := endedTest(t)
	dir := a.EndedTestDirs[0]
	s := startSleeper(t, dir, "")
	var asked []string
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/_emu/v1/info" {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": a.ID, "pid": s.cmd.Process.Pid})
			return
		}
		f.mu.Lock()
		removed := len(f.removed)
		f.mu.Unlock()
		asked = append(asked, r.Method+" "+r.URL.Path+" "+r.Header.Get(gateway.AdminTokenHeader)+" removed:"+strconv.Itoa(removed))
		_, _ = io.WriteString(w, "{}")
		_ = s.cmd.Process.Kill()
	}))
	defer gw.Close()
	eps, _ := json.Marshal(map[string]string{"gateway": strings.TrimPrefix(gw.URL, "http://")})
	for name, b := range map[string][]byte{EndpointsFile: eps, AdminTokenFile: []byte("tok\n")} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Reclaim(context.Background(), fakeClient(t, f)); err != nil {
		t.Fatal(err)
	}
	if want := []string{"POST /_emu/v1/shutdown tok removed:0"}; !slices.Equal(asked, want) {
		t.Errorf("asked %q; want %q", asked, want)
	}
	if s.alive() {
		t.Error("Instance process still runs after the reclaim")
	}
	assertReclaimed(t, f, a)
}

// TestReclaimKillsOnlyGcpemu: a process that the dir's endpoints cannot
// stop is killed only if its executable is named gcpemu, in any case,
// with or without .exe; the objects and dir are removed either way.
func TestReclaimKillsOnlyGcpemu(t *testing.T) {
	for _, tc := range []struct {
		exe    string
		killed bool
	}{
		{exe: "gcpemu", killed: true},
		{exe: "GcpEmu.exe", killed: true},
		{exe: "gcpemu-other"},
		{exe: ""}, // the test binary
	} {
		t.Run(tc.exe, func(t *testing.T) {
			f, a := endedTest(t)
			s := startSleeper(t, a.EndedTestDirs[0], tc.exe)
			if err := a.Reclaim(context.Background(), fakeClient(t, f)); err != nil {
				t.Fatal(err)
			}
			if s.alive() == tc.killed {
				t.Errorf("process of %q alive after the reclaim: %v, want %v", tc.exe, !tc.killed, !tc.killed)
			}
			assertReclaimed(t, f, a)
		})
	}
}
