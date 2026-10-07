package nat_test

import (
	"testing"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// `--services nat` pulls in compute (routers/NATs) and is ready once the
// container runtime is reachable.
func TestNATServicePullsInCompute(t *testing.T) {
	emutest.RequireRuntime(t)
	inst := emutest.Start(t, []string{"nat"})
	svc, ok := inst.Env.Lookup("compute")
	if !ok {
		t.Fatal("compute not started with nat")
	}
	if _, ok := svc.(emu.VPC); !ok {
		t.Fatal("compute does not implement emu.VPC")
	}
	n, ok := inst.Env.Lookup("nat")
	if !ok {
		t.Fatal("nat not running")
	}
	if err := n.Ready(); err != nil {
		t.Fatalf("ready: %v", err)
	}
}
