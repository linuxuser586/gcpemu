package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestStartRefusesUnusableGKEMaxNodes is #1068: a Node limit that is not
// a number, or below 1, stops `gcpemu start` with an error naming where it
// came from, before any Service starts, rather than becoming the default.
func TestStartRefusesUnusableGKEMaxNodes(t *testing.T) {
	for _, tc := range []struct {
		name, env string
		args      []string
		want      []string
	}{
		{name: "env not a number", env: "abc", want: []string{"GCPEMU_GKE_MAX_NODES"}},
		{name: "env trailing space", env: "8 ", want: []string{"GCPEMU_GKE_MAX_NODES"}},
		{name: "env zero", env: "0", want: []string{"--gke-max-nodes", "GCPEMU_GKE_MAX_NODES", "gkeMaxNodes", " 0 "}},
		{name: "flag negative", args: []string{"--gke-max-nodes", "-3"}, want: []string{"--gke-max-nodes", "GCPEMU_GKE_MAX_NODES", "gkeMaxNodes", "-3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GCPEMU_GKE_MAX_NODES", tc.env)
			dir := t.TempDir()
			root := New(nil)
			root.SetArgs(append([]string{"start", "--config", filepath.Join(dir, "none.yaml"), "--data-dir", dir, "--services", "gke"}, tc.args...))
			root.SilenceUsage = true
			err := root.Execute()
			if err == nil {
				t.Fatal("start succeeded")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %q", err, w)
				}
			}
		})
	}
}

// TestGKEMaxNodesFlag: --gke-max-nodes sets the Node limit over the
// environment, which sets it over the config file (FR-CORE-010, #1068).
func TestGKEMaxNodesFlag(t *testing.T) {
	t.Setenv("GCPEMU_GKE_MAX_NODES", "")
	if got := loadUnder(t, "", nil, nil, "").GKEMaxNodes; got != 5 {
		t.Errorf("unset: %d, want 5", got)
	}
	if got := loadUnder(t, "", nil, nil, "gkeMaxNodes: 2\n").GKEMaxNodes; got != 2 {
		t.Errorf("file: %d, want 2", got)
	}
	t.Setenv("GCPEMU_GKE_MAX_NODES", "4")
	if got := loadUnder(t, "", nil, nil, "gkeMaxNodes: 2\n").GKEMaxNodes; got != 4 {
		t.Errorf("env over file: %d, want 4", got)
	}
	if got := loadUnder(t, "", []string{"--gke-max-nodes", "7"}, nil, "gkeMaxNodes: 2\n").GKEMaxNodes; got != 7 {
		t.Errorf("flag over env: %d, want 7", got)
	}
}
