package cli

import (
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/instance"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

var abandonedIDs = []instance.Abandoned{
	{Tagged: instance.Tagged{ID: "aaa111", Dirs: []string{"/gone/default"},
		Containers: []runtime.Container{{ID: "c1", Name: "gcpemu-aaa111-sql"}},
		Networks:   []runtime.Network{{ID: "n1", Name: "gcpemu-aaa111-vpc"}, {ID: "n2", Name: "gcpemu-aaa111-psa"}},
		Volumes:    []runtime.Volume{{Name: "gcpemu-aaa111-pgdata"}}}},
	{Tagged: instance.Tagged{ID: "bbb222", Networks: []runtime.Network{{ID: "n3", Name: "gcpemu-bbb222-vpc"}}}, Unverified: true},
}

// doctor WARNs about each Abandoned Instance ID with its counts, marks
// legacy ones unverified and points to `gcpemu prune` (#192).
func TestCheckAbandoned(t *testing.T) {
	var out strings.Builder
	checkAbandoned(&out, abandonedIDs, nil)
	s := out.String()
	for _, want := range []string{
		"[WARN] abandoned Instance ID aaa111: 1 container(s), 2 network(s), 1 volume(s)",
		"[WARN] abandoned Instance ID bbb222: 0 container(s), 1 network(s), 0 volume(s) (unverified",
		"gcpemu prune",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	out.Reset()
	checkAbandoned(&out, nil, nil)
	if !strings.HasPrefix(out.String(), "[ok  ]") {
		t.Errorf("none abandoned: %q", out.String())
	}
}

// prune lists what it removes, volumes on their own line, and removes
// nothing unless confirmed.
func TestPrune(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		yes, pruned bool
	}{
		{name: "declined", input: "n\n"},
		{name: "confirmed", input: "y\n", pruned: true},
		{name: "--yes", yes: true, pruned: true},
	} {
		var out strings.Builder
		var got []string
		err := runPrune(&out, strings.NewReader(tc.input), tc.yes, abandonedIDs, func(a instance.Abandoned) error {
			got = append(got, a.ID)
			return nil
		})
		s := out.String()
		if tc.pruned != (len(got) == 2) || tc.pruned != (err == nil) {
			t.Errorf("%s: pruned %v, err %v", tc.name, got, err)
		}
		for _, want := range []string{
			"containers: gcpemu-aaa111-sql",
			"networks:   gcpemu-aaa111-vpc, gcpemu-aaa111-psa",
			"volumes:    gcpemu-aaa111-pgdata (their data is lost)",
			"data dir:   /gone/default",
			"unverified",
		} {
			if !strings.Contains(s, want) {
				t.Errorf("%s: missing %q in:\n%s", tc.name, want, s)
			}
		}
		if strings.Contains(s, "[y/N]") == tc.yes {
			t.Errorf("%s: prompt shown=%v:\n%s", tc.name, !tc.yes, s)
		}
	}

	var out strings.Builder
	if err := runPrune(&out, strings.NewReader(""), false, nil, nil); err != nil || !strings.Contains(out.String(), "nothing to prune") {
		t.Errorf("none abandoned: %v %q", err, out.String())
	}
}
