package gcs_test

import (
	"testing"

	"github.com/linuxuser586/gcpemu/internal/emutestx"
)

// TestStopEndedCtxRequestInProgress: with Teardown's ctx already ended, a
// request still in progress on Cloud Storage's Service endpoint is closed
// by force and Stop reports success (#1117).
func TestStopEndedCtxRequestInProgress(t *testing.T) {
	inst := start(t)
	c := emutestx.HeldRequest(t, inst.Internal(), "gcs", "gcs", "/storage/v1/b/no-such-bucket")
	if err := gcsService(t, inst).Stop(emutestx.Ended()); err != nil {
		t.Fatalf("Stop = %v, want nil", err)
	}
	emutestx.AssertClosed(t, c)
}
