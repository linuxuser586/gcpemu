package build_test

import (
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/protobuf/types/known/durationpb"
)

// A step that waitFors another reads what it wrote to /workspace, and the
// Build's Operation completes with the SUCCESS Build.
func TestStepsShareTheWorkspace(t *testing.T) {
	e := start(t)
	op, _ := e.create(&cloudbuildpb.Build{Steps: []*cloudbuildpb.BuildStep{
		step("write", "echo hello > /workspace/greeting"),
		step("read", "cat greeting", "write"),
	}})
	b, err := op.Wait(e.ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if b.GetStatus() != cloudbuildpb.Build_SUCCESS {
		t.Fatalf("status = %v (%s)", b.GetStatus(), b.GetStatusDetail())
	}
	for i, s := range b.GetSteps() {
		if s.GetStatus() != cloudbuildpb.Build_SUCCESS || s.GetTiming().GetStartTime() == nil || s.GetTiming().GetEndTime() == nil {
			t.Errorf("step %d = %v, timing %v", i, s.GetStatus(), s.GetTiming())
		}
	}
	if log := e.log(b); !strings.Contains(log, `Step #1 - "read": hello`) {
		t.Errorf("log = %q", log)
	}
	e.noBuildObjects()
}

// A step exiting non-zero fails the Build, and stops the step running
// beside it, unless allowFailure or allowExitCodes lets it fail.
func TestFailingStep(t *testing.T) {
	e := start(t)
	op, created := e.create(&cloudbuildpb.Build{Steps: []*cloudbuildpb.BuildStep{
		step("sleeper", "sleep 600", "-"),
		step("fails", "exit 3", "-"),
		step("never", "true"),
	}})
	if _, err := op.Wait(e.ctx); err == nil {
		t.Fatal("Wait succeeded for a failed Build")
	}
	b := e.get(created.GetId())
	if b.GetStatus() != cloudbuildpb.Build_FAILURE || b.GetFailureInfo().GetType() != cloudbuildpb.Build_FailureInfo_USER_BUILD_STEP {
		t.Fatalf("Build = %v, failure %v", b.GetStatus(), b.GetFailureInfo())
	}
	want := []cloudbuildpb.Build_Status{cloudbuildpb.Build_CANCELLED, cloudbuildpb.Build_FAILURE, cloudbuildpb.Build_QUEUED}
	for i, s := range b.GetSteps() {
		if s.GetStatus() != want[i] {
			t.Errorf("step %d = %v, want %v", i, s.GetStatus(), want[i])
		}
	}
	if code := b.GetSteps()[1].GetExitCode(); code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
	e.noBuildObjects()

	allowed := []*cloudbuildpb.BuildStep{step("", "exit 3"), step("", "exit 4"), step("", "true")}
	allowed[0].AllowFailure = true
	allowed[1].AllowExitCodes = []int32{1, 4}
	op, _ = e.create(&cloudbuildpb.Build{Steps: allowed})
	b, err := op.Wait(e.ctx)
	if err != nil || b.GetStatus() != cloudbuildpb.Build_SUCCESS {
		t.Fatalf("allowed failures: %v, %v", b.GetStatus(), err)
	}
	for i, s := range b.GetSteps()[:2] {
		if s.GetStatus() != cloudbuildpb.Build_FAILURE || s.GetExitCode() != int32(3+i) {
			t.Errorf("allowed step %d = %v exit %d", i, s.GetStatus(), s.GetExitCode())
		}
	}
}

// Steps honour script, env (after the Build's options.env), dir and
// volumes, which steps naming the same volume share.
func TestStepFields(t *testing.T) {
	e := start(t)
	vol := []*cloudbuildpb.Volume{{Name: "cache", Path: "/cache"}}
	op, _ := e.create(&cloudbuildpb.Build{
		Options: &cloudbuildpb.BuildOptions{Env: []string{"WHO=options", "KEEP=kept"}},
		Steps: []*cloudbuildpb.BuildStep{
			{Name: busybox, Id: "script", Env: []string{"WHO=step"}, Dir: "sub", Volumes: vol,
				Script: "echo \"$WHO $KEEP $(pwd) $HOME\"\necho cached > /cache/v\n"},
			{Name: busybox, Id: "shebang", Script: "#!/bin/sh -e\ncat /cache/v\n", Volumes: vol},
		},
	})
	b, err := op.Wait(e.ctx)
	if err != nil {
		t.Fatalf("Wait: %v (%s)", err, b.GetStatusDetail())
	}
	log := e.log(b)
	for _, want := range []string{`Step #0 - "script": step kept /workspace/sub /builder/home`, `Step #1 - "shebang": cached`} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
}

// Advancing the Emulator clock past a step's own timeout ends the step and
// its Build TIMEOUT.
func TestStepTimeout(t *testing.T) {
	e := start(t)
	s := step("", "sleep 600")
	s.Timeout = durationpb.New(time.Minute)
	_, b := e.create(&cloudbuildpb.Build{Steps: []*cloudbuildpb.BuildStep{s}})
	e.awaitStep(b.GetId(), 0)
	if err := e.inst.Internal().AdvanceClock(e.ctx, 61*time.Second); err != nil {
		t.Fatal(err)
	}
	b = e.await(b.GetId(), cloudbuildpb.Build_TIMEOUT, cloudbuildpb.Build_FAILURE, cloudbuildpb.Build_SUCCESS)
	if b.GetStatus() != cloudbuildpb.Build_TIMEOUT || b.GetSteps()[0].GetStatus() != cloudbuildpb.Build_TIMEOUT {
		t.Fatalf("Build %v, step %v", b.GetStatus(), b.GetSteps()[0].GetStatus())
	}
	e.noBuildObjects()
}
