package runtime

import (
	"strings"
	"testing"
)

// A Containerized Instance guesses a VM runtime from the runtime's info,
// and the setting overrides the guess (ADR 0012). On the host the decision
// is always "no": the VM relay covers VM runtimes there.
func TestDecideVM(t *testing.T) {
	desktop := Info{OS: "Docker Desktop", Kernel: "6.10.14-linuxkit"}
	orb := Info{OS: "OrbStack", Kernel: "6.12.5-orbstack-00287-gf8da5d508983"}
	linux := Info{OS: "Ubuntu 24.04.1 LTS", Kernel: "6.8.0-45-generic"}
	for _, tc := range []struct {
		name, setting string
		inContainer   bool
		info          Info
		want          bool
		reason        string
	}{
		{"docker desktop", "auto", true, desktop, true, "Docker Desktop"},
		{"docker desktop by kernel", "", true, Info{Kernel: "6.10.14-linuxkit"}, true, "linuxkit"},
		{"orbstack", "auto", true, orb, true, "OrbStack"},
		{"unknown os", "auto", true, linux, false, "Ubuntu"},
		{"no info", "", true, Info{}, false, "unknown"},
		{"forced on", "true", true, linux, true, VMRuntimeEnv},
		{"forced off", "false", true, desktop, false, VMRuntimeEnv},
		{"on the host", "true", false, desktop, false, "host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := DecideVM(tc.setting, tc.inContainer, tc.info)
			if d.On != tc.want || !strings.Contains(d.Reason, tc.reason) {
				t.Errorf("DecideVM(%q, %v, %+v) = %+v; want On=%v, reason naming %q", tc.setting, tc.inContainer, tc.info, d, tc.want, tc.reason)
			}
		})
	}
	if got := (VMDecision{On: true, Reason: "x"}).String(); got != "yes (x)" {
		t.Errorf("String() = %q", got)
	}
	if got := (VMDecision{Reason: "x"}).String(); got != "no (x)" {
		t.Errorf("String() = %q", got)
	}
}
