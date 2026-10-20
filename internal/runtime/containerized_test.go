package runtime

import "testing"

// The runtime's answer wins when there is one, conflicting with a set
// --containerized; without it a set --containerized decides, and auto
// goes by the marker files, a guess when they say yes (#502).
func TestDecideContainerized(t *testing.T) {
	for _, tc := range []struct {
		name               string
		setting            string
		markers, rt        bool
		self               string
		on, guess, clashes bool
	}{
		{"runtime: own bridge container", "auto", true, true, "abc", true, false, false},
		{"runtime: host network", "auto", true, true, "", false, false, false},
		{"runtime: on the host", "auto", false, true, "", false, false, false},
		{"runtime: bridge container, set false", "false", true, true, "abc", true, false, true},
		{"runtime: host network, set true", "true", true, true, "", false, false, true},
		{"runtime: bridge container, set true", "TRUE", true, true, "abc", true, false, false},
		{"no runtime: set false", "false", true, false, "", false, false, false},
		{"no runtime: set true", "true", false, false, "", true, false, false},
		{"no runtime: auto, markers", "auto", true, false, "", true, true, false},
		{"no runtime: default, markers", "", true, false, "", true, true, false},
		{"no runtime: auto, no markers", "auto", false, false, "", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := DecideContainerized(tc.setting, tc.markers, tc.rt, tc.self)
			if d.On != tc.on || d.Guess != tc.guess || (d.Conflict != "") != tc.clashes {
				t.Errorf("DecideContainerized(%q, %v, %v, %q) = %+v; want On=%v Guess=%v conflict=%v",
					tc.setting, tc.markers, tc.rt, tc.self, d, tc.on, tc.guess, tc.clashes)
			}
		})
	}
}

// Only a container (markers) or --containerized=true may be a
// Containerized Instance, so only then is the runtime worth asking.
func TestMayBeContainerized(t *testing.T) {
	for _, tc := range []struct {
		setting string
		markers bool
		want    bool
	}{
		{"auto", true, true},
		{"false", true, true},
		{"auto", false, false},
		{"false", false, false},
		{"true", false, true},
	} {
		if got := MayBeContainerized(tc.setting, tc.markers); got != tc.want {
			t.Errorf("MayBeContainerized(%q, %v) = %v, want %v", tc.setting, tc.markers, got, tc.want)
		}
	}
}
