//go:build prerelease

package prerelease

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/instance"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// The reclaim of the checks' CLI-started Instances (#921) runs on any
// host: a fake gcpemu stands in for the Instance and a fake Docker API
// for the runtime.

// fakeGcpemuEnv makes the test binary act as gcpemu: TestMain hands its
// arguments to fakeGcpemu.
const fakeGcpemuEnv = "GCPEMU_PRERELEASE_FAKE_GCPEMU"

// fakeGcpemu acts as gcpemu with args, for the data dir --data-dir names,
// and returns its exit code. start --detach leaves a detached child,
// which writes its pid and sleeps, as an Instance serves; the ID is
// "fake-" and the dir's name. stop, like every other command, fails.
func fakeGcpemu(args []string) int {
	var dir string
	for i, a := range args {
		if a == "--data-dir" && i+1 < len(args) {
			dir = args[i+1]
		}
	}
	switch args[0] {
	case "start":
		if err := os.WriteFile(filepath.Join(dir, instance.IDFile), []byte(fakeIDOf(dir)), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := exec.Command(os.Args[0], "serve", "--data-dir", dir).Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		for range 200 {
			if _, err := os.Stat(filepath.Join(dir, instance.PIDFile)); err == nil {
				return 0
			}
			time.Sleep(50 * time.Millisecond)
		}
		fmt.Fprintln(os.Stderr, "fake gcpemu: the Instance wrote no pid")
		return 1
	case "serve":
		if err := os.WriteFile(filepath.Join(dir, instance.PIDFile), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			return 1
		}
		time.Sleep(10 * time.Minute)
		return 0
	}
	fmt.Fprintf(os.Stderr, "fake gcpemu: %s fails\n", args[0])
	return 1
}

// useFakeGcpemu runs a copy of the test binary named exe as gcpemuBin,
// with the Test data dirs under a root of t's own, for the rest of t.
func useFakeGcpemu(t *testing.T, exe string) {
	t.Helper()
	b, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), exe+exeExt())
	if err := os.WriteFile(bin, b, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeGcpemuEnv, "1")
	oldBin, oldRoot := gcpemuBin, testDirRoot
	gcpemuBin, testDirRoot = bin, t.TempDir()
	t.Cleanup(func() { gcpemuBin, testDirRoot = oldBin, oldRoot })
}

// startFake starts a fake Instance in a Test data dir whose owner lock
// it holds, and returns it with its process's pid, which is killed when
// t ends.
func startFake(t *testing.T) (*hostInstance, int) {
	t.Helper()
	h, err := newHostInstance("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.release)
	if out, err := h.gcpemu(context.Background(), "start", "--detach"); err != nil {
		t.Fatalf("fake gcpemu start: %v\n%s", err, out)
	}
	b, err := os.ReadFile(filepath.Join(h.dir, instance.PIDFile))
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(string(b))
	t.Cleanup(func() {
		if p, err := os.FindProcess(pid); err == nil && instance.ProcessAlive(pid) {
			_ = p.Kill()
		}
	})
	return h, pid
}

// fakeIDOf is the ID the fake Instance in dir holds.
func fakeIDOf(dir string) string { return "fake-" + filepath.Base(dir) }

// fakeID is the ID the fake Instance in h's dir holds.
func fakeID(h *hostInstance) string { return fakeIDOf(h.dir) }

// fakeDocker serves the Docker API calls of the reclaim: a container, a
// network and a volume for each Instance it was given, and records what
// was removed, in order.
type fakeDocker struct {
	mu      sync.Mutex
	objs    map[string]map[string]map[string]string // kind, name: labels
	removed []string
}

// useFakeDocker serves a running container, a network and a volume
// tagged with the ID of each of hs, and makes detectRuntime return a
// client for it, for the rest of t.
func useFakeDocker(t *testing.T, hs ...*hostInstance) (*fakeDocker, *runtime.Client) {
	t.Helper()
	d := &fakeDocker{objs: map[string]map[string]map[string]string{"containers": {}, "networks": {}, "volumes": {}}}
	for _, h := range hs {
		l := map[string]string{runtime.LabelInstance: fakeID(h), runtime.LabelDir: h.dir}
		for kind := range d.objs {
			d.objs[kind][kind[:1]+"-"+fakeID(h)] = l
		}
	}
	srv := httptest.NewServer(d)
	t.Cleanup(srv.Close)
	cl, err := runtime.New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	old := detectRuntime
	detectRuntime = func() (*runtime.Client, error) { return cl, nil }
	t.Cleanup(func() { detectRuntime = old })
	return d, cl
}

func (d *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/v1.41")
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

// assertReclaimed fails t unless the objects of each of hs were removed,
// containers first, and their dirs are gone.
func (d *fakeDocker) assertReclaimed(t *testing.T, hs ...*hostInstance) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, h := range hs {
		want := []string{"containers:c-" + fakeID(h), "networks:n-" + fakeID(h), "volumes:v-" + fakeID(h)}
		var got []string
		for _, r := range d.removed {
			if strings.HasSuffix(r, fakeID(h)) {
				got = append(got, r)
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("removed %v of %s; want %v", got, fakeID(h), want)
		}
		if _, err := os.Stat(h.dir); !os.IsNotExist(err) {
			t.Errorf("Test data dir %s after the reclaim: %v, want it removed", h.dir, err)
		}
	}
}

// gone reports whether the process pid exits within a few seconds.
func gone(pid int) bool {
	for range 50 {
		if !instance.ProcessAlive(pid) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// TestHostEndReclaimsWhenStopFails is #921: when a check's gcpemu stop
// fails, its cleanup still kills the Instance's process, named gcpemu,
// and removes its containers, networks and volumes and its dir, and the
// check still reports the failed stop.
func TestHostEndReclaimsWhenStopFails(t *testing.T) {
	useFakeGcpemu(t, "gcpemu")
	h, pid := startFake(t)
	d, _ := useFakeDocker(t, h)
	err := h.end(context.Background())
	if err == nil || !strings.Contains(err.Error(), "gcpemu stop") {
		t.Errorf("end = %v; want the failed gcpemu stop", err)
	}
	if !gone(pid) {
		t.Error("the Instance process still runs after the check's cleanup")
	}
	d.assertReclaimed(t, h)
}

// TestHostChildDoesNotHoldOwnerLock is #921: the processes gcpemu start
// leaves running do not inherit the check's owner lock, so the lock can
// be taken as soon as the check lets go of it (or its process dies).
func TestHostChildDoesNotHoldOwnerLock(t *testing.T) {
	useFakeGcpemu(t, "gcpemu")
	h, pid := startFake(t)
	h.release()
	if !instance.ProcessAlive(pid) {
		t.Fatal("the Instance process exited")
	}
	release, err := instance.HoldOwnerLock(h.dir)
	if err != nil {
		t.Fatalf("taking the owner lock while the Instance runs: %v", err)
	}
	release()
}

// TestRunStartReclaim is #921: before the first check, a run reclaims the
// Test data dirs whose test has ended: ended, whose objects record it,
// and bare, whose Instance made none. Their processes are killed only if
// named gcpemu, and their objects and dirs are removed either way. owned,
// whose test still runs, is left alone.
func TestRunStartReclaim(t *testing.T) {
	for _, tc := range []struct {
		exe    string
		killed bool
	}{
		{exe: "gcpemu", killed: true},
		{exe: "GCPEMU", killed: true},
		{exe: "not-gcpemu"},
	} {
		t.Run(tc.exe, func(t *testing.T) {
			useFakeGcpemu(t, tc.exe)
			ended, endedPid := startFake(t)
			bare, barePid := startFake(t)
			owned, ownedPid := startFake(t)
			ended.release()
			bare.release()
			d, cl := useFakeDocker(t, ended, owned)

			if err := instance.ReclaimEndedUnder(context.Background(), testDirRoot, []*runtime.Client{cl}); err != nil {
				t.Fatalf("ReclaimEndedUnder: %v", err)
			}
			for name, pid := range map[string]int{"ended": endedPid, "bare": barePid} {
				if gone(pid) != tc.killed {
					t.Errorf("process of %s, named %s, gone: %v; want %v", name, tc.exe, !tc.killed, tc.killed)
				}
			}
			d.assertReclaimed(t, ended)
			if _, err := os.Stat(bare.dir); !os.IsNotExist(err) {
				t.Errorf("Test data dir of bare after the reclaim: %v, want it removed", err)
			}
			if !instance.ProcessAlive(ownedPid) {
				t.Error("the reclaim stopped owned's process")
			}
			if _, err := os.Stat(owned.dir); err != nil {
				t.Errorf("owned's dir after the reclaim: %v", err)
			}
			d.mu.Lock()
			defer d.mu.Unlock()
			if slices.ContainsFunc(d.removed, func(r string) bool { return strings.HasSuffix(r, fakeID(owned)) }) {
				t.Errorf("the reclaim removed owned's objects: %v", d.removed)
			}
		})
	}
}

// TestRunStartReclaimKeepsUnrecorded is #921: when a runtime did not
// answer, the run-start reclaim leaves a dir that no object records, whose
// objects may be on that runtime, for a later run.
func TestRunStartReclaimKeepsUnrecorded(t *testing.T) {
	useFakeGcpemu(t, "gcpemu")
	bare, barePid := startFake(t)
	bare.release()
	_, cl := useFakeDocker(t)
	if err := instance.ReclaimEndedUnder(context.Background(), testDirRoot, []*runtime.Client{cl, nil}); err != nil {
		t.Fatalf("ReclaimEndedUnder: %v", err)
	}
	if !instance.ProcessAlive(barePid) {
		t.Error("the reclaim stopped the process of a dir no object records")
	}
	if _, err := os.Stat(bare.dir); err != nil {
		t.Errorf("dir no object records after the reclaim: %v", err)
	}
}
