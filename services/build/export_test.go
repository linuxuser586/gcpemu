package build

import (
	"context"

	"github.com/linuxuser586/gcpemu/internal/emu"
)

// SetCheckpoint makes f run at points of every Build's run, with the
// Build's context: "setup" before its volumes and network are made, "step
// N" once step N's waitFor steps have ended, "step N exited" once step N's
// container exited, and "ended" once the step runner returned, before
// cleanup (exported to the external tests).
func SetCheckpoint(svc emu.Service, f func(ctx context.Context, id, at string)) {
	svc.(*Service).checkpoint = f
}
