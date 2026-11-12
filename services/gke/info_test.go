package gke

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

// TestInfoNodeLimit: the GKE Service reports the Node limit in force in
// /_emu/v1/info, from the same lookup its quota checks use (FR-GKE-003,
// #1065).
func TestInfoNodeLimit(t *testing.T) {
	for _, tc := range []struct {
		name, env string
		unset     bool
		want      int
	}{
		{name: "set", env: "8", want: 8},
		{name: "unset", unset: true, want: defaultMaxNodes},
		{name: "not a number", env: "abc", want: defaultMaxNodes},
		{name: "zero", env: "0", want: defaultMaxNodes},
		{name: "negative", env: "-3", want: defaultMaxNodes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(maxNodesEnv, tc.env) // restores the variable after the test
			if tc.unset {
				_ = os.Unsetenv(maxNodesEnv)
			}
			b, err := json.Marshal((&Service{}).Info())
			if err != nil {
				t.Fatal(err)
			}
			if want := fmt.Sprintf(`{"nodeLimit":%d}`, tc.want); string(b) != want {
				t.Errorf("Info() = %s, want %s", b, want)
			}
			if got := maxNodes(); got != tc.want {
				t.Errorf("maxNodes() = %d, want %d: the report and the quota checks disagree", got, tc.want)
			}
		})
	}
}
