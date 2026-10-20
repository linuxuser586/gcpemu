package build_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/linuxuser586/gcpemu/services/build"
)

// stopAt runs stop, once, when a Build reaches the checkpoint at, and
// holds the Build there until stop has stopped it.
func (e *env) stopAt(at string, stop func()) {
	e.t.Helper()
	svc, ok := e.inst.Internal().Env.Lookup("build")
	if !ok {
		e.t.Fatal("no build Service")
	}
	var fired atomic.Bool
	build.SetCheckpoint(svc, func(ctx context.Context, _, point string) {
		if point != at || !fired.CompareAndSwap(false, true) {
			return
		}
		go stop()
		select {
		case <-ctx.Done():
		case <-time.After(time.Minute):
			e.t.Errorf("the Build was not stopped at %q", at)
		}
	})
}

// A cancel arriving once every step has ended, while the Build's
// containers are still being removed, leaves a SUCCESS Build SUCCESS and
// its Operation without error (#455): by CancelBuild, which answers the
// Build unchanged, and by cancelling the Build's Operation.
func TestLateCancelKeepsSuccess(t *testing.T) {
	for _, by := range []string{"CancelBuild", "CancelOperation"} {
		t.Run(by, func(t *testing.T) {
			e := start(t)
			answered := make(chan *cloudbuildpb.Build, 1)
			target := make(chan string, 1)
			e.stopAt("ended", func() {
				name := <-target
				if by == "CancelBuild" {
					answered <- e.cancel(name)
					return
				}
				if err := e.cb.LROClient.CancelOperation(e.ctx, &longrunningpb.CancelOperationRequest{Name: name}); err != nil {
					t.Errorf("CancelOperation: %v", err)
				}
				answered <- nil
			})
			op, created := e.create(&cloudbuildpb.Build{Steps: []*cloudbuildpb.BuildStep{step("", "true")}})
			if by == "CancelBuild" {
				target <- created.GetId()
			} else {
				target <- op.Name()
			}
			b, err := op.Wait(e.ctx)
			if err != nil || b.GetStatus() != cloudbuildpb.Build_SUCCESS {
				t.Fatalf("Wait = %v, %v; want SUCCESS", b.GetStatus(), err)
			}
			if a := <-answered; by == "CancelBuild" && a.GetStatus() != cloudbuildpb.Build_SUCCESS {
				t.Errorf("CancelBuild answered %v, want SUCCESS", a.GetStatus())
			}
			if got := e.get(created.GetId()).GetSteps()[0].GetStatus(); got != cloudbuildpb.Build_SUCCESS {
				t.Errorf("step = %v, want SUCCESS", got)
			}
		})
	}
}

// A Build timeout firing once every step has ended keeps the steps'
// outcome (#455).
func TestLateTimeoutKeepsOutcome(t *testing.T) {
	for _, tc := range []struct {
		script string
		want   cloudbuildpb.Build_Status
	}{
		{"true", cloudbuildpb.Build_SUCCESS},
		{"exit 3", cloudbuildpb.Build_FAILURE},
	} {
		t.Run(tc.want.String(), func(t *testing.T) {
			e := start(t)
			e.stopAt("ended", func() {
				if err := e.inst.Internal().AdvanceClock(e.ctx, 61*time.Second); err != nil {
					t.Error(err)
				}
			})
			op, created := e.create(&cloudbuildpb.Build{Timeout: durationpb.New(time.Minute), Steps: []*cloudbuildpb.BuildStep{step("", tc.script)}})
			_, _ = op.Wait(e.ctx)
			if b := e.get(created.GetId()); b.GetStatus() != tc.want {
				t.Fatalf("Build = %v (%s), want %v", b.GetStatus(), b.GetStatusDetail(), tc.want)
			}
		})
	}
}

// A cancel arriving once a step's container exited, before its status is
// recorded, leaves the step's own outcome: a failing step ends the Build
// FAILURE, not CANCELLED (#455).
func TestCancelAfterStepExits(t *testing.T) {
	for _, tc := range []struct {
		script string
		want   cloudbuildpb.Build_Status
	}{
		{"true", cloudbuildpb.Build_SUCCESS},
		{"exit 3", cloudbuildpb.Build_FAILURE},
	} {
		t.Run(tc.want.String(), func(t *testing.T) {
			e := start(t)
			ids := make(chan string, 1)
			e.stopAt("step 0 exited", func() { e.cancel(<-ids) })
			op, created := e.create(&cloudbuildpb.Build{Steps: []*cloudbuildpb.BuildStep{step("", tc.script)}})
			ids <- created.GetId()
			_, _ = op.Wait(e.ctx)
			b := e.get(created.GetId())
			if b.GetStatus() != tc.want || b.GetSteps()[0].GetStatus() != tc.want {
				t.Fatalf("Build = %v (%s), step %v; want %v", b.GetStatus(), b.GetStatusDetail(), b.GetSteps()[0].GetStatus(), tc.want)
			}
		})
	}
}

// A cancel arriving between two sequential steps ends the Build CANCELLED,
// the second step never running.
func TestCancelBetweenSteps(t *testing.T) {
	e := start(t)
	ids := make(chan string, 1)
	e.stopAt("step 1", func() { e.cancel(<-ids) })
	op, created := e.create(&cloudbuildpb.Build{Steps: []*cloudbuildpb.BuildStep{step("", "true"), step("", "true")}})
	ids <- created.GetId()
	if _, err := op.Wait(e.ctx); err == nil {
		t.Error("the Operation of a cancelled Build succeeded")
	}
	b := e.get(created.GetId())
	if b.GetStatus() != cloudbuildpb.Build_CANCELLED {
		t.Fatalf("Build = %v (%s), want CANCELLED", b.GetStatus(), b.GetStatusDetail())
	}
	want := []cloudbuildpb.Build_Status{cloudbuildpb.Build_SUCCESS, cloudbuildpb.Build_QUEUED}
	for i, s := range b.GetSteps() {
		if s.GetStatus() != want[i] {
			t.Errorf("step %d = %v, want %v", i, s.GetStatus(), want[i])
		}
	}
	e.noBuildObjects()
}

// A cancel arriving before the Build's volumes and network are made ends
// it CANCELLED, not INTERNAL_ERROR.
func TestCancelDuringSetup(t *testing.T) {
	e := start(t)
	ids := make(chan string, 1)
	e.stopAt("setup", func() { e.cancel(<-ids) })
	op, created := e.create(&cloudbuildpb.Build{Steps: []*cloudbuildpb.BuildStep{step("", "true")}})
	ids <- created.GetId()
	_, _ = op.Wait(e.ctx)
	if b := e.get(created.GetId()); b.GetStatus() != cloudbuildpb.Build_CANCELLED {
		t.Fatalf("Build = %v (%s), want CANCELLED", b.GetStatus(), b.GetStatusDetail())
	}
	e.noBuildObjects()
}
