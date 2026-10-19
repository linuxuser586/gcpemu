package build_test

import (
	"testing"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A Reset mid-Build leaves none of the Build's containers or volumes, and
// no Build.
func TestResetMidBuild(t *testing.T) {
	e := start(t, withConcurrency(1))
	_, working := e.create(&cloudbuildpb.Build{Steps: []*cloudbuildpb.BuildStep{step("", "sleep 600")}})
	e.awaitStep(working.GetId(), 0)
	_, queued := e.create(&cloudbuildpb.Build{Steps: []*cloudbuildpb.BuildStep{step("", "true")}})
	if err := e.inst.Reset(e.ctx); err != nil {
		t.Fatal(err)
	}
	e.noBuildObjects()
	for _, b := range []*cloudbuildpb.Build{working, queued} {
		_, err := e.cb.GetBuild(e.ctx, &cloudbuildpb.GetBuildRequest{ProjectId: testProject, Id: b.GetId()})
		if status.Code(err) != codes.NotFound {
			t.Errorf("after Reset, get %s: %v", b.GetId(), err)
		}
	}
	op, _ := e.create(&cloudbuildpb.Build{Steps: []*cloudbuildpb.BuildStep{step("", "true")}})
	if b, err := op.Wait(e.ctx); err != nil || b.GetStatus() != cloudbuildpb.Build_SUCCESS {
		t.Fatalf("a Build after Reset: %v, %v", b.GetStatus(), err)
	}
}

// A Build WORKING or QUEUED when a State snapshot was saved restores as
// INTERNAL_ERROR, with its Operation done, and no containers left.
func TestSnapshotMidBuild(t *testing.T) {
	e := start(t, withConcurrency(1))
	_, working := e.create(&cloudbuildpb.Build{Steps: []*cloudbuildpb.BuildStep{step("", "sleep 600")}})
	e.awaitStep(working.GetId(), 0)
	queuedOp, queued := e.create(&cloudbuildpb.Build{Steps: []*cloudbuildpb.BuildStep{step("", "true")}})
	if _, err := e.inst.SaveSnapshot(e.ctx, "mid-build"); err != nil {
		t.Fatal(err)
	}
	if err := e.inst.RestoreSnapshot(e.ctx, "mid-build"); err != nil {
		t.Fatal(err)
	}
	for _, b := range []*cloudbuildpb.Build{working, queued} {
		got := e.get(b.GetId())
		if got.GetStatus() != cloudbuildpb.Build_INTERNAL_ERROR || got.GetFinishTime() == nil {
			t.Errorf("restored Build %s = %v", b.GetId(), got.GetStatus())
		}
	}
	if _, err := queuedOp.Wait(e.ctx); status.Code(err) != codes.Internal {
		t.Errorf("the restored Build's Operation: %v", err)
	}
	e.noBuildObjects()
}
