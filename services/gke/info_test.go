package gke

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// TestInfoNodeLimit: the GKE Service reports the Node limit in force in
// /_emu/v1/info, from the Config its quota checks use (FR-GKE-003,
// #1065, #1068).
func TestInfoNodeLimit(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  int
		want int
	}{
		{name: "set", set: 8, want: 8},
		{name: "default", want: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Defaults()
			if tc.set != 0 {
				cfg.GKEMaxNodes = tc.set
			}
			s := &Service{env: &emu.Env{Config: &cfg}}
			b, err := json.Marshal(s.Info())
			if err != nil {
				t.Fatal(err)
			}
			if want := fmt.Sprintf(`{"nodeLimit":%d}`, tc.want); string(b) != want {
				t.Errorf("Info() = %s, want %s", b, want)
			}
			if got := s.maxNodes(); got != tc.want {
				t.Errorf("maxNodes() = %d, want %d: the report and the quota checks disagree", got, tc.want)
			}
		})
	}
}

// TestNodeLimitFromConfig: the quota checks enforce the Config's Node
// limit, whatever GCPEMU_GKE_MAX_NODES says in the process environment,
// and their error names the flag and the variable (#1068).
func TestNodeLimitFromConfig(t *testing.T) {
	t.Setenv("GCPEMU_GKE_MAX_NODES", "100") // ignored: only Config counts
	cfg := config.Defaults()
	cfg.GKEMaxNodes = 3
	s := &Service{env: &emu.Env{Config: &cfg}}
	if err := s.limitFor(1, 2); err != nil {
		t.Errorf("limitFor(1, 2) at limit 3 = %v, want nil", err)
	}
	err := s.limitFor(1, 3)
	if err == nil {
		t.Fatal("limitFor(1, 3) at limit 3 accepted")
	}
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Reason != "QUOTA_EXCEEDED" {
		t.Errorf("limitFor error %v, want reason QUOTA_EXCEEDED", err)
	}
	for _, want := range []string{"--gke-max-nodes", "GCPEMU_GKE_MAX_NODES", "of 3 are in use"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("limitFor error %q does not mention %s", err, want)
		}
	}
}
