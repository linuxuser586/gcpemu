package build_test

import (
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Advancing the Emulator clock past a Build's timeout ends it TIMEOUT and
// removes its containers.
func TestTimeoutOnTheEmulatorClock(t *testing.T) {
	e := start(t)
	op, b := e.create(&cloudbuildpb.Build{Steps: []*cloudbuildpb.BuildStep{step("", "sleep 600")}})
	e.awaitStep(b.GetId(), 0)
	if err := e.inst.Internal().AdvanceClock(e.ctx, 601*time.Second); err != nil {
		t.Fatal(err)
	}
	b = e.get(b.GetId())
	if b.GetStatus() != cloudbuildpb.Build_TIMEOUT || b.GetSteps()[0].GetStatus() != cloudbuildpb.Build_TIMEOUT {
		t.Fatalf("after the advance: Build %v, step %v", b.GetStatus(), b.GetSteps()[0].GetStatus())
	}
	if _, err := op.Wait(e.ctx); err == nil {
		t.Error("the Operation of a timed-out Build succeeded")
	}
	e.noBuildObjects()
}

// With build.concurrency=1 later Builds wait QUEUED: one passing its
// queueTtl ends EXPIRED, and a QUEUED or WORKING one can be cancelled,
// the WORKING one's containers going with it.
func TestQueue(t *testing.T) {
	e := start(t, withConcurrency(1))
	_, first := e.create(&cloudbuildpb.Build{Steps: []*cloudbuildpb.BuildStep{step("", "sleep 600")}})
	e.awaitStep(first.GetId(), 0)
	_, second := e.create(&cloudbuildpb.Build{Steps: []*cloudbuildpb.BuildStep{step("", "true")}})
	expiring, third := e.create(&cloudbuildpb.Build{QueueTtl: durationpb.New(time.Minute), Steps: []*cloudbuildpb.BuildStep{step("", "true")}})
	_, fourth := e.create(&cloudbuildpb.Build{Steps: []*cloudbuildpb.BuildStep{step("", "true")}})
	time.Sleep(time.Second)
	for _, b := range []*cloudbuildpb.Build{second, third, fourth} {
		if got := e.get(b.GetId()).GetStatus(); got != cloudbuildpb.Build_QUEUED {
			t.Fatalf("Build %s = %v while the first runs, want QUEUED", b.GetId(), got)
		}
	}

	if err := e.inst.Internal().AdvanceClock(e.ctx, 61*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := e.get(third.GetId()).GetStatus(); got != cloudbuildpb.Build_EXPIRED {
		t.Errorf("past its queueTtl the third Build is %v, want EXPIRED", got)
	}
	if _, err := expiring.Wait(e.ctx); err == nil {
		t.Error("the Operation of an EXPIRED Build succeeded")
	}
	if got := e.get(second.GetId()).GetStatus(); got != cloudbuildpb.Build_QUEUED {
		t.Errorf("the second Build is %v, want QUEUED", got)
	}

	cancelled, err := e.cb.CancelBuild(e.ctx, &cloudbuildpb.CancelBuildRequest{ProjectId: testProject, Id: fourth.GetId()})
	if err != nil || cancelled.GetStatus() != cloudbuildpb.Build_CANCELLED {
		t.Fatalf("cancel QUEUED: %v, %v", cancelled.GetStatus(), err)
	}
	cancelled, err = e.cb.CancelBuild(e.ctx, &cloudbuildpb.CancelBuildRequest{ProjectId: testProject, Id: first.GetId()})
	if err != nil || cancelled.GetStatus() != cloudbuildpb.Build_CANCELLED || cancelled.GetSteps()[0].GetStatus() != cloudbuildpb.Build_CANCELLED {
		t.Fatalf("cancel WORKING: %v, %v", cancelled.GetStatus(), err)
	}
	cs, vs := e.buildObjects()
	for _, n := range append(cs, vs...) {
		if strings.Contains(n, first.GetId()) {
			t.Errorf("the cancelled Build left %s", n)
		}
	}
	e.await(second.GetId(), cloudbuildpb.Build_SUCCESS)
}
