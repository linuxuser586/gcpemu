package main_test

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Performance targets of SRS 8.1 that need the real binary. They are
// asserted with GCPEMU_PERF_TESTS=1 on a quiet host (the CI perf job);
// otherwise the measurements are only logged.

func perfGate(t *testing.T, ok bool, format string, args ...any) {
	t.Helper()
	t.Logf(format, args...)
	if !ok && os.Getenv("GCPEMU_PERF_TESTS") == "1" {
		t.Errorf("target missed: "+format, args...)
	}
}

// nonContainerServices are the services NFR-PERF-001/004 cover (all
// except GKE and Cloud SQL).
const nonContainerServices = "iam,compute,dns,certs,ar,pubsub,secrets,gcs,lb,cdn,nat"

// TestColdStartAndIdleRSS is NFR-PERF-001 (cold start to ready <= 2 s)
// and NFR-PERF-004 (idle RSS <= 150 MB) for every non-container service.
func TestColdStartAndIdleRSS(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	dir := t.TempDir()
	start := time.Now()
	out := must(t, nil, "start", "--detach", "--data-dir", dir, "--services", nonContainerServices, "--port", "0", "--wait-timeout", "30s")
	took := time.Since(start)
	t.Cleanup(func() { _, _ = gcpemu(t, nil, "stop", "--data-dir", dir) })
	if !strings.Contains(out, "ready") {
		t.Fatal(out)
	}
	perfGate(t, took <= 2*time.Second, "NFR-PERF-001: cold start to ready took %v (target 2 s)", took)

	if goruntime.GOOS != "linux" {
		return
	}
	var sj struct{ Info struct{ Pid int } }
	if err := json.Unmarshal([]byte(must(t, nil, "status", "--json", "--data-dir", dir)), &sj); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second) // idle
	f, err := os.Open("/proc/" + strconv.Itoa(sj.Info.Pid) + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var rssKB int
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "VmRSS:"); ok {
			rssKB, _ = strconv.Atoi(strings.Fields(v)[0])
		}
	}
	if rssKB == 0 {
		t.Fatal("no VmRSS")
	}
	perfGate(t, rssKB <= 150*1024, "NFR-PERF-004: idle RSS %d MB (target 150 MB)", rssKB/1024)
}

// TestControlPlaneP99 is NFR-PERF-005: non-LRO control-plane calls have a
// p99 latency <= 20 ms (instant LRO mode).
func TestControlPlaneP99(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	dir := t.TempDir()
	startDetached(t, dir)
	eps := endpoints(t, dir)
	gw, gcs := "http://"+eps["gateway"], "http://"+eps["gcs"]
	if c := code(t, "POST", gcs+"/storage/v1/b?project=perf-proj", `{"name":"perf-bucket"}`); c != 200 {
		t.Fatalf("create bucket: %d", c)
	}
	if c := code(t, "PUT", gw+"/pubsub/v1/projects/perf-proj/topics/topic1", "{}"); c != 200 {
		t.Fatalf("create topic: %d", c)
	}
	calls := []string{
		gcs + "/storage/v1/b/perf-bucket",
		gcs + "/storage/v1/b?project=perf-proj",
		gw + "/pubsub/v1/projects/perf-proj/topics/topic1",
		gw + "/pubsub/v1/projects/perf-proj/topics",
	}
	cl := &http.Client{Timeout: 5 * time.Second}
	var lat []time.Duration
	for i := range 2000 {
		u := calls[i%len(calls)]
		s := time.Now()
		resp, err := cl.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s: %d", u, resp.StatusCode)
		}
		if i >= 200 { // after warm-up
			lat = append(lat, time.Since(s))
		}
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	p50, p99 := lat[len(lat)/2], lat[len(lat)*99/100]
	perfGate(t, p99 <= 20*time.Millisecond, "NFR-PERF-005: control-plane p50 %v, p99 %v (target p99 20 ms)", p50, p99)
}

// TestBinarySize is NFR-PERF-009: the release binary, gzip-compressed, is
// at most 120 MB.
func TestBinarySize(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	out := filepath.Join(t.TempDir(), "gcpemu")
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w", "-o", out, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, _ := f.Stat()
	var n countWriter
	zw, _ := gzip.NewWriterLevel(&n, gzip.BestCompression)
	if _, err := io.Copy(zw, f); err != nil {
		t.Fatal(err)
	}
	_ = zw.Close()
	perfGate(t, n <= 120<<20, "NFR-PERF-009: binary %d MB, %d MB gzip-compressed (target 120 MB compressed)", fi.Size()>>20, int64(n)>>20)
}

type countWriter int64

func (c *countWriter) Write(p []byte) (int, error) { *c += countWriter(len(p)); return len(p), nil }
