package ar_test

import (
	"testing"

	"github.com/linuxuser586/gcpemu/internal/emutestx"
)

// TestStopEndedCtxRequestInProgress: with Teardown's ctx already ended, a
// request still in progress on the registry port is closed by force and
// Stop reports success (#1117).
func TestStopEndedCtxRequestInProgress(t *testing.T) {
	e := start(t)
	svc, ok := e.inst.Internal().Env.Lookup("ar")
	if !ok {
		t.Fatal("ar not running")
	}
	c := emutestx.HeldRequest(t, e.inst.Internal(), "ar", "ar", "/v2/")
	if err := svc.Stop(emutestx.Ended()); err != nil {
		t.Fatalf("Stop = %v, want nil", err)
	}
	emutestx.AssertClosed(t, c)
}
