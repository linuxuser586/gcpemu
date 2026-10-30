package emutest

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/instance"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// The child test process of the reclaim tests: reclaimChildEnv names the
// kind of Instance it starts, reclaimChildDirEnv its data dir.
const (
	reclaimChildEnv    = "GCPEMU_TEST_RECLAIM_CHILD"
	reclaimChildDirEnv = "GCPEMU_TEST_RECLAIM_DIR"
	reclaimImage       = "busybox:1.36.1"
)

// The kinds of Instance a child starts.
const (
	childTestDataDir = "test-data-dir" // WithTestDataDir
	childDataDir     = "data-dir"      // WithDataDir
	childEphemeral   = "ephemeral"
)

// TestReclaimChild is the child process of the reclaim tests: it starts an
// Instance with a running container, a network and a volume, prints its
// ID and data dir, and waits to be killed.
func TestReclaimChild(t *testing.T) {
	kind := os.Getenv(reclaimChildEnv)
	if kind == "" {
		t.Skip("run by the reclaim tests")
	}
	var opts []Option
	switch dir := os.Getenv(reclaimChildDirEnv); kind {
	case childTestDataDir:
		opts = append(opts, WithTestDataDir(t, dir))
	case childDataDir:
		opts = append(opts, WithDataDir(dir))
	}
	inst := Start(t, []string{"gcs"}, opts...)
	labelledObjects(t, inst)
	fmt.Printf("READY %s %s\n", inst.in.ID, inst.in.Dir)
	time.Sleep(10 * time.Minute)
}

// labelledObjects gives inst a running container, a network and a volume
// carrying its labels.
func labelledObjects(t *testing.T, inst *Instance) {
	t.Helper()
	ctx := context.Background()
	plane, err := inst.in.Env.Containers.Netplane(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plane.Services(ctx); err != nil {
		t.Fatal(err)
	}
	rt := plane.Runtime()
	if err := rt.CreateVolume(ctx, rt.Name("reclaim"), rt.Labels("", "", "")); err != nil {
		t.Fatal(err)
	}
	id, err := rt.CreateContainer(ctx, runtime.ContainerSpec{Name: rt.Name("reclaim"), Image: reclaimImage,
		Cmd: []string{"sleep", "600"}, Labels: rt.Labels("", "", "")})
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
}

// reclaimRuntime skips t without a runtime and returns a client for it.
func reclaimRuntime(t *testing.T) *runtime.Client {
	t.Helper()
	RequireRuntime(t)
	cl, err := runtime.Detect()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := cl.EnsureImage(ctx, reclaimImage, false); err != nil {
		t.Skipf("image %s unavailable: %v", reclaimImage, err)
	}
	return cl
}

// child is a running TestReclaimChild.
type child struct {
	cmd     *exec.Cmd
	id, dir string
}

// startChild runs TestReclaimChild with an Instance of kind in dir, and
// waits until it is ready. Its objects are removed when t ends.
func startChild(t *testing.T, cl *runtime.Client, kind, dir string) *child {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestReclaimChild$")
	cmd.Env = append(os.Environ(), reclaimChildEnv+"="+kind, reclaimChildDirEnv+"="+dir)
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &child{cmd: cmd}
	t.Cleanup(func() {
		c.kill()
		if c.id != "" {
			m := &runtime.Manager{Client: cl, InstanceID: c.id}
			_ = m.Cleanup(context.Background(), true)
		}
	})
	ready := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if rest, ok := strings.CutPrefix(sc.Text(), "READY "); ok {
				c.id, c.dir, _ = strings.Cut(rest, " ")
				ready <- nil
				return
			}
		}
		ready <- errors.New("child exited before it was ready")
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Minute):
		t.Fatal("child not ready after 2m")
	}
	return c
}

// kill kills the child without letting it clean up, as `go test -timeout`
// or ^C in a CI job does.
func (c *child) kill() {
	if c.cmd.ProcessState == nil {
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
	}
}

// objects returns the names of the containers, networks and volumes that
// id tags.
func objects(t *testing.T, cl *runtime.Client, id string) []string {
	t.Helper()
	ctx := context.Background()
	sel := map[string]string{runtime.LabelInstance: id}
	var out []string
	cs, err := cl.ListContainers(ctx, sel)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		out = append(out, "container "+c.Name)
	}
	nets, err := cl.ListNetworks(ctx, sel)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nets {
		out = append(out, "network "+n.Name)
	}
	vols, err := cl.ListVolumes(ctx, sel)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vols {
		out = append(out, "volume "+v)
	}
	return out
}

// mustNotLog is a sweep's logf that fails t.
func mustNotLog(t *testing.T) logFunc {
	return func(format string, args ...any) { t.Errorf("sweep logged: "+format, args...) }
}

// assertKept fails t unless id still tags its container, network and
// volume and dir still exists.
func assertKept(t *testing.T, cl *runtime.Client, id, dir string) {
	t.Helper()
	if got := objects(t, cl, id); len(got) < 3 {
		t.Errorf("Instance %s has %q after the sweep, want its container, networks and volume", id, got)
	}
	if _, err := os.Stat(filepath.Join(dir, instance.IDFile)); err != nil {
		t.Errorf("data dir after the sweep: %v", err)
	}
}

// assertReclaimed fails t if id tags anything or dir still exists.
func assertReclaimed(t *testing.T, cl *runtime.Client, id, dir string) {
	t.Helper()
	for _, o := range objects(t, cl, id) {
		t.Errorf("%s of Instance %s left after the sweep", o, id)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("data dir %s after the sweep: %v, want it removed", dir, err)
	}
}

// testReclaimKilled: the Instance of a live child is never swept; once
// the child is killed, two sweeps at once reclaim it without an error.
func testReclaimKilled(t *testing.T, kind, dir string) {
	cl := reclaimRuntime(t)
	c := startChild(t, cl, kind, dir)
	ctx := context.Background()
	sweep(ctx, cl, mustNotLog(t))
	assertKept(t, cl, c.id, c.dir)

	c.kill()
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sweep(ctx, cl, mustNotLog(t))
		}()
	}
	wg.Wait()
	assertReclaimed(t, cl, c.id, c.dir)
}

// TestSweepReclaimsKilledTestDataDir is #846 for WithTestDataDir.
func TestSweepReclaimsKilledTestDataDir(t *testing.T) {
	testReclaimKilled(t, childTestDataDir, filepath.Join(t.TempDir(), "data"))
}

// TestSweepReclaimsKilledEphemeral is #846 for an Ephemeral Instance.
func TestSweepReclaimsKilledEphemeral(t *testing.T) {
	testReclaimKilled(t, childEphemeral, "")
}

// TestSweepSparesDataDir: the Instance of a killed test that used
// WithDataDir, whose dir has no owner lock file, is never swept, even in
// the OS temp dir.
func TestSweepSparesDataDir(t *testing.T) {
	cl := reclaimRuntime(t)
	c := startChild(t, cl, childDataDir, t.TempDir())
	c.kill()
	sweep(context.Background(), cl, mustNotLog(t))
	assertKept(t, cl, c.id, c.dir)
}

// TestSweepSparesOwnInstance: the sweep never touches a Test data dir
// that this process owns.
func TestSweepSparesOwnInstance(t *testing.T) {
	cl := reclaimRuntime(t)
	inst := Start(t, []string{"gcs"}, WithTestDataDir(t, t.TempDir()))
	labelledObjects(t, inst)
	sweep(context.Background(), cl, mustNotLog(t))
	assertKept(t, cl, inst.in.ID, inst.in.Dir)
}

// TestSweepReclaimsDeletedTempDir: an Abandoned Instance ID whose dir in
// the OS temp dir is gone is reclaimed; an Unverified one, and one whose
// dir was outside the OS temp dir, are not.
func TestSweepReclaimsDeletedTempDir(t *testing.T) {
	cl := reclaimRuntime(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%x", time.Now().UnixNano())
	vols := map[string]map[string]string{
		"gone" + suffix:       {runtime.LabelInstance: "gone" + suffix, runtime.LabelDir: filepath.Join(t.TempDir(), "gone")},
		"unverified" + suffix: {runtime.LabelInstance: "unverified" + suffix},
		"outside" + suffix:    {runtime.LabelInstance: "outside" + suffix, runtime.LabelDir: "/gcpemu-no-such-dir/" + suffix},
	}
	for id, labels := range vols {
		if err := cl.CreateVolume(ctx, "gcpemu-"+id, labels); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cl.RemoveVolume(context.Background(), "gcpemu-"+id) })
	}
	sweep(ctx, cl, mustNotLog(t))
	if got := objects(t, cl, "gone"+suffix); len(got) != 0 {
		t.Errorf("Abandoned ID in the temp dir: %q left", got)
	}
	for _, id := range []string{"unverified" + suffix, "outside" + suffix} {
		if got := objects(t, cl, id); len(got) != 1 {
			t.Errorf("%s: %q after the sweep, want its volume kept", id, got)
		}
	}
}

// TestSweepOncePerProcess: the sweep looks for a runtime once, does
// nothing without one, and logs a failure instead of failing.
func TestSweepOncePerProcess(t *testing.T) {
	detects := 0
	s := &sweeper{detect: func() (*runtime.Client, error) {
		detects++
		return nil, errors.New("no runtime")
	}}
	s.run(mustNotLog(t))
	s.run(mustNotLog(t))
	if detects != 1 {
		t.Errorf("runtime detected %d times, want once", detects)
	}

	broken, err := runtime.New("tcp://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	var logged []string
	s = &sweeper{detect: func() (*runtime.Client, error) { return broken, nil }}
	s.run(func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) })
	if len(logged) != 1 || !strings.HasPrefix(logged[0], "emutest: ") {
		t.Errorf("a failed sweep logged %q, want one emutest error", logged)
	}
}

// TestStartSweepsFirst: starting an Instance runs the process's sweep
// first, even when the start fails.
func TestStartSweepsFirst(t *testing.T) {
	detects := 0
	saved := processSweep
	processSweep = &sweeper{detect: func() (*runtime.Client, error) {
		detects++
		return nil, errors.New("no runtime")
	}}
	t.Cleanup(func() { processSweep = saved })
	for range 2 {
		if i, err := New(context.Background(), []string{"no-such-service"}); err == nil {
			i.Close(context.Background())
		}
	}
	if detects != 1 {
		t.Errorf("the sweep looked for a runtime %d times, want once", detects)
	}
}
