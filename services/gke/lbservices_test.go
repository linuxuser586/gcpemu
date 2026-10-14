package gke

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// holder is a peer Service that holds loopback addresses.
type holder struct {
	emu.Service
	addrs []string
}

func (h holder) LoopbackAddrs() []string { return h.addrs }

func newUnitService(t *testing.T) *Service {
	t.Helper()
	cfg := config.Defaults()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(&emu.Env{Config: &cfg, Store: store.NewMemory(), Clock: clock.Real{}, IDs: emu.NewIDs(true), Log: log,
		Auth: emu.NewPolicyAuthorizer(config.IAMOff, log), Endpoints: emu.NewEndpoints()}).(*Service)
}

// A LoadBalancer Service's loopback address skips those forwarding rules,
// Cloud SQL instances and other Services hold, and starts at its previous
// one.
func TestLBLoopCandidates(t *testing.T) {
	s := newUnitService(t)
	const name = "p/us-central1-a/c1/default/web"
	first := s.loopCandidates(name, "")[0]
	other := s.loopCandidates("p/us-central1-a/c1/default/api", "")[0]
	s.env.SetServices(map[string]emu.Service{
		"lb":  holder{addrs: []string{first}},
		"sql": holder{addrs: []string{"127.0.0.9"}},
	})
	s.loops["p/us-central1-a/c1/default/api"] = other
	got := s.loopCandidates(name, "")
	held := map[string]bool{first: true, "127.0.0.9": true, other: true}
	for a := range held {
		if slices.Contains(got, a) {
			t.Errorf("candidates include %s, which is held", a)
		}
	}
	if slices.Contains(got, "127.0.0.1") || len(got) != 253-len(held) {
		t.Errorf("%d candidates, want 127.0.0.2-254 less %d held", len(got), len(held))
	}
	if got := s.loopCandidates(name, "127.0.0.200"); got[0] != "127.0.0.200" || slices.Contains(got[1:], "127.0.0.200") {
		t.Errorf("with prev: candidates start %v", got[:2])
	}
	if got := s.LoopbackAddrs(); !slices.Equal(got, []string{other}) {
		t.Errorf("LoopbackAddrs = %v, want [%s]", got, other)
	}
}

func TestLBEnvVars(t *testing.T) {
	s := newUnitService(t)
	got := s.EnvVars("127.0.0.1:4000", map[string]string{
		"gke:c-1/default/web/80": "127.0.0.7:80",
		"lb:fr":                  "127.0.0.8:80",
	})
	if got["GCPEMU_GKE_C_1_DEFAULT_WEB_80"] != "127.0.0.7:80" || len(got) != 3 {
		t.Errorf("EnvVars = %v", got)
	}
}

// Without a VM relay the controller leaves LoadBalancer Services to
// servicelb.
func TestLBServicesLoopWithoutRuntime(t *testing.T) {
	s := newUnitService(t)
	s.lbServicesLoop(context.Background(), "p/l/c")
}
