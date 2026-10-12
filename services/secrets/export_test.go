package secrets

import "github.com/linuxuser586/gcpemu/internal/emu"

// HoldSweep blocks sweeps until release is called, as a sweep in flight
// does (exported to the external tests).
func HoldSweep(svc emu.Service) (release func()) {
	s := svc.(*Service)
	s.sweepMu.Lock()
	return s.sweepMu.Unlock
}
