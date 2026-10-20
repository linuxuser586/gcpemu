package dns_test

import (
	"encoding/json"
	"net/http"
	"testing"

	mdns "github.com/miekg/dns"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
)

// A fast DNS query reports a latency above zero on every platform:
// Windows' monotonic clock advances only every ~0.5ms, so queries are
// timed with a clock.Stopwatch (#259).

func TestDataPlaneQueryLatency(t *testing.T) {
	inst := emutest.Start(t, []string{"dns"}, emutest.WithConfig(func(c *config.Config) { c.DNSNoForward = true }))
	dataPlaneFixture(t, newClient(t, inst.GatewayURL()+"/"))
	query(t, inst.Endpoint("dns"), "udp", "alias.app.test.", mdns.TypeA)
	var found bool
	for _, e := range inst.Internal().Env.RequestLog.Entries("dns") {
		if e.Protocol == "dns" && e.Method == "UDP A alias.app.test." {
			found = true
			if e.LatencyMS <= 0 {
				t.Errorf("LatencyMS = %v, want > 0", e.LatencyMS)
			}
		}
	}
	if !found {
		t.Fatalf("query not in the request log: %+v", inst.Internal().Env.RequestLog.Entries("dns"))
	}
}

func TestConsoleQueryLatency(t *testing.T) {
	inst := emutest.Start(t, []string{"dns"}, emutest.WithConfig(func(c *config.Config) { c.DNSNoForward = true }))
	dataPlaneFixture(t, newClient(t, inst.GatewayURL()+"/"))
	resp, err := http.Get(inst.GatewayURL() + "/_emu/v1/dns/query?name=alias.app.test.")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Rcode     string
		LatencyMS float64 `json:"latencyMs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != http.StatusOK || out.Rcode != "NOERROR" {
		t.Fatalf("console query: %d %+v %v", resp.StatusCode, out, err)
	}
	if out.LatencyMS <= 0 {
		t.Errorf("latencyMs = %v, want > 0", out.LatencyMS)
	}
}
