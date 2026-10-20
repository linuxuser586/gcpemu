package runtime

import (
	"strings"
	"testing"
)

// The runtime's answer, when it found the emulator's own container, wins,
// conflicting with a set --containerized; without it a set
// --containerized decides, and auto goes by the marker files, a guess
// when they say yes that names why the runtime can't tell (#502).
func TestDecideContainerized(t *testing.T) {
	const (
		noSocket = "no container runtime socket"
		notFound = SelfEnv
	)
	for _, tc := range []struct {
		name    string
		f       ContainerizedFacts
		on      bool
		guess   string
		clashes bool
	}{
		{"found: own bridge container", ContainerizedFacts{"auto", true, true, true, "abc"}, true, "", false},
		{"found: host network", ContainerizedFacts{"auto", true, true, true, ""}, false, "", false},
		{"found: bridge container, set false", ContainerizedFacts{"false", true, true, true, "abc"}, true, "", true},
		{"found: host network, set true", ContainerizedFacts{"true", true, true, true, ""}, false, "", true},
		{"found: bridge container, set true", ContainerizedFacts{"TRUE", true, true, true, "abc"}, true, "", false},
		{"not found: set true, no markers", ContainerizedFacts{"true", false, true, false, ""}, true, "", false},
		{"not found: set false", ContainerizedFacts{"false", true, true, false, ""}, false, "", false},
		{"not found: auto, markers", ContainerizedFacts{"auto", true, true, false, ""}, true, notFound, false},
		{"not found: on the host", ContainerizedFacts{"auto", false, true, false, ""}, false, "", false},
		{"no runtime: set false", ContainerizedFacts{"false", true, false, false, ""}, false, "", false},
		{"no runtime: set true", ContainerizedFacts{"true", false, false, false, ""}, true, "", false},
		{"no runtime: auto, markers", ContainerizedFacts{"auto", true, false, false, ""}, true, noSocket, false},
		{"no runtime: default, markers", ContainerizedFacts{"", true, false, false, ""}, true, noSocket, false},
		{"no runtime: auto, no markers", ContainerizedFacts{"auto", false, false, false, ""}, false, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := DecideContainerized(tc.f)
			if d.On != tc.on || (d.Conflict != "") != tc.clashes {
				t.Errorf("DecideContainerized(%+v) = %+v; want On=%v conflict=%v", tc.f, d, tc.on, tc.clashes)
			}
			if (d.Guess != "") != (tc.guess != "") || !strings.Contains(d.Guess, tc.guess) {
				t.Errorf("DecideContainerized(%+v): guess %q, want one naming %q", tc.f, d.Guess, tc.guess)
			}
			if d.Guess != "" && !strings.Contains(d.Guess, "--containerized=false") {
				t.Errorf("guess %q does not name --containerized=false", d.Guess)
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
