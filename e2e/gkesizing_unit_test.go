//go:build e2e

package e2e

import "testing"

func TestParseDockerStats(t *testing.T) {
	for _, tc := range []struct {
		line     string
		name     string
		mem      int64
		cpuMilli int64
	}{
		// Docker prints binary units.
		{`{"Name":"gcpemu-1-gke-n1","MemUsage":"512.5MiB / 62.47GiB","CPUPerc":"12.34%"}`, "gcpemu-1-gke-n1", 537395200, 123},
		{`{"Name":"s","MemUsage":"1.5GiB / 8GiB","CPUPerc":"150.00%"}`, "s", 1610612736, 1500},
		{`{"Name":"s","MemUsage":"900KiB / 8GiB","CPUPerc":"0.00%"}`, "s", 921600, 0},
		// Podman prints decimal units.
		{`{"Name":"p","MemUsage":"512.5MB / 8.2GB","CPUPerc":"2.5%"}`, "p", 512500000, 25},
		{`{"Name":"p","MemUsage":"64kB / 8.2GB","CPUPerc":"--"}`, "p", 64000, 0},
	} {
		name, mem, cpu, err := parseDockerStats([]byte(tc.line))
		if err != nil {
			t.Errorf("%s: %v", tc.line, err)
			continue
		}
		if name != tc.name || mem != tc.mem || cpu != tc.cpuMilli {
			t.Errorf("%s: got %q %d B %dm, want %q %d B %dm", tc.line, name, mem, cpu, tc.name, tc.mem, tc.cpuMilli)
		}
	}
	if _, _, _, err := parseDockerStats([]byte(`{"Name":"x","MemUsage":"lots","CPUPerc":"1%"}`)); err == nil {
		t.Error("unparseable MemUsage: want an error")
	}
}
