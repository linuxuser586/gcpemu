package emutest_test

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/instance"
)

// TestPortRangeSideBySide is FR-CORE-042 / FR-CORE-033 / FR-CI-005: with
// --port 0 and --port-range, two instances run side by side and take every
// listener port from the range, recorded in endpoints.json.
func TestPortRangeSideBySide(t *testing.T) {
	const lo, hi = 42100, 42299
	withRange := func(c *config.Config) { c.PortRange = strconv.Itoa(lo) + "-" + strconv.Itoa(hi) }
	svcs := []string{"gcs", "pubsub", "dns"}
	a := emutest.Start(t, svcs, withRange)
	b := emutest.Start(t, svcs, withRange)

	seen := map[string]string{}
	for _, inst := range []*emutest.Instance{a, b} {
		eps := inst.Env.Endpoints.All()
		for _, name := range []string{"gateway", "gcs", "pubsub", "dns", "metadata"} {
			addr := eps[name]
			_, ps, err := net.SplitHostPort(addr)
			if err != nil {
				t.Fatalf("%s endpoint %q: %v", name, addr, err)
			}
			if p, _ := strconv.Atoi(ps); p < lo || p > hi {
				t.Errorf("%s on %s, outside %d-%d", name, addr, lo, hi)
			}
			if other, dup := seen[addr]; dup {
				t.Errorf("%s and %s both on %s", name, other, addr)
			}
			seen[addr] = name
		}
		raw, err := os.ReadFile(filepath.Join(inst.Env.Config.DataDir, instance.EndpointsFile))
		if err != nil {
			t.Fatal(err)
		}
		var file map[string]string
		if err := json.Unmarshal(raw, &file); err != nil {
			t.Fatalf("endpoints.json: %v\n%s", err, raw)
		}
		if file["gateway"] != eps["gateway"] || file["dns"] != eps["dns"] {
			t.Errorf("endpoints.json = %v, want %v", file, eps)
		}
	}
}
