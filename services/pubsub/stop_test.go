package pubsub_test

import (
	"testing"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/emutestx"
)

// TestStopEndedCtxRequestInProgress: with Teardown's ctx already ended, a
// request still in progress on Pub/Sub's Service endpoint is closed by force and
// Stop reports success (#1117).
func TestStopEndedCtxRequestInProgress(t *testing.T) {
	inst := emutest.Start(t, []string{"pubsub"})
	svc, ok := inst.Internal().Env.Lookup("pubsub")
	if !ok {
		t.Fatal("pubsub not running")
	}
	c := emutestx.HeldRequest(t, inst.Internal(), "pubsub", "pubsub", "/v1/projects/p/nothing-here")
	if err := svc.Stop(emutestx.Ended()); err != nil {
		t.Fatalf("Stop = %v, want nil", err)
	}
	emutestx.AssertClosed(t, c)
}
