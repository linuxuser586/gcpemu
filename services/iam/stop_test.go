package iam_test

import (
	"testing"

	"github.com/linuxuser586/gcpemu/internal/emutestx"
)

// TestStopEndedCtxRequestInProgress: with Teardown's ctx already ended, a
// request still in progress on the metadata server is closed by force and
// Stop reports success (#1117).
func TestStopEndedCtxRequestInProgress(t *testing.T) {
	inst := startIAM(t)
	svc, ok := inst.Env.Lookup("iam")
	if !ok {
		t.Fatal("iam not running")
	}
	c := emutestx.HeldRequest(t, inst.Instance, "metadata", "iam", "/computeMetadata/v1/project/project-id")
	if err := svc.Stop(emutestx.Ended()); err != nil {
		t.Fatalf("Stop = %v, want nil", err)
	}
	emutestx.AssertClosed(t, c)
}
