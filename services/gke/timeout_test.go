package gke_test

import (
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/services/gke"
)

// TestClusterOpTimeoutCoversBringUp keeps the tests' cluster operation
// deadline longer than the emulator's bring-up waits added up, so a slow
// bring-up fails with the emulator's error, not the test's (#396).
func TestClusterOpTimeoutCoversBringUp(t *testing.T) {
	var sum time.Duration
	for what, d := range gke.BringUpWaits {
		if d <= 0 {
			t.Errorf("%s wait = %v", what, d)
		}
		sum += d
	}
	if emutest.ClusterOpTimeout <= sum {
		t.Errorf("emutest.ClusterOpTimeout = %v, want more than the bring-up waits' %v (%v)", emutest.ClusterOpTimeout, sum, gke.BringUpWaits)
	}
}
