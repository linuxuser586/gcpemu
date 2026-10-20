package nat_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// `--services nat` pulls in compute (routers/NATs) and is ready once the
// container runtime is reachable.
func TestNATServicePullsInCompute(t *testing.T) {
	emutest.RequireRuntime(t)
	inst := emutest.Start(t, []string{"nat"})
	svc, ok := inst.Internal().Env.Lookup("compute")
	if !ok {
		t.Fatal("compute not started with nat")
	}
	if _, ok := svc.(emu.VPC); !ok {
		t.Fatal("compute does not implement emu.VPC")
	}
	n, ok := inst.Internal().Env.Lookup("nat")
	if !ok {
		t.Fatal("nat not running")
	}
	if err := n.Ready(); err != nil {
		t.Fatalf("ready: %v", err)
	}
}

// TestSinkSwitch: GET/PUT /_emu/v1/nat/sink reports and switches the
// offline sink (FR-NAT-005, Web console Cloud NAT view); --offline turns
// it on at start.
func TestSinkSwitch(t *testing.T) {
	emutest.RequireRuntime(t)
	type state struct {
		Enabled bool
		Status  int
		Body    string
	}
	call := func(inst *emutest.Instance, method, body string, code int) state {
		t.Helper()
		req, _ := http.NewRequest(method, inst.GatewayURL()+"/_emu/v1/nat/sink", strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != code {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("%s %s: %d %s, want %d", method, body, resp.StatusCode, b, code)
		}
		var s state
		_ = json.NewDecoder(resp.Body).Decode(&s)
		return s
	}
	inst := emutest.Start(t, []string{"nat"})
	if s := call(inst, "GET", "", 200); s != (state{false, 503, "gcpemu offline: egress blocked"}) {
		t.Fatalf("default: %+v", s)
	}
	if s := call(inst, "PUT", `{"enabled":true}`, 200); !s.Enabled {
		t.Fatalf("switched on: %+v", s)
	}
	if s := call(inst, "GET", "", 200); !s.Enabled {
		t.Fatalf("after switch: %+v", s)
	}
	call(inst, "PUT", `{}`, 400)
	call(inst, "PUT", `not json`, 400)

	off := emutest.Start(t, []string{"nat"}, emutest.WithConfig(func(c *config.Config) { c.Offline = true; c.NATSinkResponse = "418:teapot" }))
	if s := call(off, "GET", "", 200); s != (state{true, 418, "teapot"}) {
		t.Fatalf("--offline: %+v", s)
	}
	if s := call(off, "PUT", `{"enabled":false}`, 200); s.Enabled {
		t.Fatalf("switched off: %+v", s)
	}
}
