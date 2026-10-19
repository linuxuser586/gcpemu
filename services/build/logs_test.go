package build_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"

	"github.com/linuxuser586/gcpemu/internal/project"
)

// A Build without logsBucket reports the default one, which exists, and
// its log object there holds every step's prefixed output.
func TestLogsGoToTheDefaultBucket(t *testing.T) {
	e := start(t)
	op, created := e.create(&cloudbuildpb.Build{Steps: []*cloudbuildpb.BuildStep{
		step("hi", "echo hello"),
		step("", "echo again"),
	}})
	want := "gs://" + project.NumberString(testProject) + ".cloudbuild-logs.googleusercontent.com"
	if got := created.GetLogsBucket(); got != want {
		t.Fatalf("logsBucket = %q, want %q", got, want)
	}
	b, err := op.Wait(e.ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	log := e.storedLog(b)
	for _, line := range []string{`Step #0 - "hi": hello`, "Step #1: again", "DONE"} {
		if !strings.Contains(log, line+"\n") {
			t.Errorf("log lacks %q:\n%s", line, log)
		}
	}
}

// A user logsBucket with a path prefix receives PREFIX/log-ID.txt, which
// grows while the Build runs: a ranged read from its current end answers
// 416, and one from an earlier offset 206 with the new bytes.
func TestLogGrowsInAPrefixedBucket(t *testing.T) {
	e := start(t)
	if code := e.rest(http.MethodPost, "/storage/v1/b?project="+testProject, map[string]string{"name": "my-logs"}, nil); code != http.StatusOK {
		t.Fatalf("create bucket = %d", code)
	}
	op, b := e.create(&cloudbuildpb.Build{
		LogsBucket: "gs://my-logs/some/dir",
		Steps:      []*cloudbuildpb.BuildStep{step("slow", "echo one; sleep 5; echo two")},
	})
	if b.GetLogsBucket() != "gs://my-logs/some/dir" {
		t.Fatalf("logsBucket = %q", b.GetLogsBucket())
	}
	name := "some/dir/log-" + b.GetId() + ".txt"
	var end int
	poll(t, func() bool {
		code, body := e.object("my-logs", name, 0)
		end = len(body)
		return code == http.StatusOK && strings.Contains(body, `Step #0 - "slow": one`)
	})
	if code, body := e.object("my-logs", name, end); code != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("read from the end = %d %q, want 416", code, body)
	}
	poll(t, func() bool {
		code, body := e.object("my-logs", name, end)
		return code == http.StatusPartialContent && strings.Contains(body, `Step #0 - "slow": two`)
	})
	if _, err := op.Wait(e.ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

// poll calls cond until it holds, failing after a minute.
func poll(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never held")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
