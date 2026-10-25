package lb_test

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	computev1 "google.golang.org/api/compute/v1"
)

// TestHealthWaitCondition is FR-CI-007's lb/health: a global or regional
// backend service is met once every backend group has a HEALTHY endpoint,
// by the health getHealth reports, and pending while any group has none or
// the service has no backends.
func TestHealthWaitCondition(t *testing.T) {
	e := start(t)
	c := e.c
	origin := newBackend(t)
	e.do(c.Networks.Insert(proj, &computev1.Network{Name: "vpc", AutoCreateSubnetworks: false, ForceSendFields: []string{"AutoCreateSubnetworks"}}).Do())
	neg := func(name string, eps ...*computev1.NetworkEndpoint) string {
		e.do(c.NetworkEndpointGroups.Insert(proj, "us-central1-a", &computev1.NetworkEndpointGroup{Name: name, Network: "global/networks/vpc"}).Do())
		if len(eps) > 0 {
			e.do(c.NetworkEndpointGroups.AttachNetworkEndpoints(proj, "us-central1-a", name, &computev1.NetworkEndpointGroupsAttachEndpointsRequest{NetworkEndpoints: eps}).Do())
		}
		return "zones/us-central1-a/networkEndpointGroups/" + name
	}
	up := neg("up", &computev1.NetworkEndpoint{IpAddress: "127.0.0.1", Port: origin.port()})
	empty := neg("empty")
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	down := neg("down", &computev1.NetworkEndpoint{IpAddress: "127.0.0.1", Port: int64(closed.Addr().(*net.TCPAddr).Port)})
	e.do(c.HealthChecks.Insert(proj, &computev1.HealthCheck{Name: "hc", Type: "HTTP", CheckIntervalSec: 1, TimeoutSec: 1,
		HttpHealthCheck: &computev1.HTTPHealthCheck{PortSpecification: "USE_SERVING_PORT", RequestPath: "/healthz"}}).Do())
	global := func(name, hc string, groups ...string) {
		bs := &computev1.BackendService{Name: name, LoadBalancingScheme: "EXTERNAL_MANAGED", Protocol: "HTTP"}
		if hc != "" {
			bs.HealthChecks = []string{"global/healthChecks/" + hc}
		}
		for _, g := range groups {
			bs.Backends = append(bs.Backends, &computev1.Backend{Group: g})
		}
		e.do(c.BackendServices.Insert(proj, bs).Do())
	}
	global("api", "hc", up)
	global("half", "hc", up, empty)
	global("bare", "")
	global("sick", "hc", up, down)
	op, err := c.RegionBackendServices.Insert(proj, "us-central1", &computev1.BackendService{Name: "reg", LoadBalancingScheme: "INTERNAL_MANAGED", Protocol: "HTTP",
		Backends: []*computev1.Backend{{Group: up}}}).Do()
	if err == nil && op.Status != "DONE" {
		op, err = c.RegionOperations.Wait(proj, "us-central1", op.Name).Do()
	}
	if err != nil || op.Error != nil {
		t.Fatalf("regional backend service: %v %+v", err, op)
	}

	// Health checks take a moment to pass, and each backend service checks
	// its groups itself: sick names up, not down, until its own check of
	// up has passed (#690).
	waitFor := func(target, state, reason string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			code, got := condition(t, e, "lb/health="+target)
			if code == 200 && got.State == state && got.Reason == reason {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("lb/health=%s: %d %+v, want %s %q", target, code, got, state, reason)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	waitFor(proj+"/global/api", "met", "")
	waitFor(proj+"/us-central1/reg", "met", "")

	for _, tc := range []struct{ target, state, reason string }{
		{proj + "/global/half", "pending", "group zones/us-central1-a/networkEndpointGroups/empty has no HEALTHY endpoint"},
		{proj + "/global/sick", "pending", "group zones/us-central1-a/networkEndpointGroups/down has no HEALTHY endpoint"},
		{proj + "/global/bare", "pending", "no backends"},
		{proj + "/global/missing", "pending", "not found"},
		{proj + "/global/reg", "pending", "not found"},
		{proj + "/us-central1/api", "pending", "not found"},
	} {
		waitFor(tc.target, tc.state, tc.reason)
	}
	if code, got := condition(t, e, "lb/health="+proj+"/us-central1-a/api"); code != 400 {
		t.Errorf("lb/health in a zone: %d %+v, want 400", code, got)
	}
}

type waitCondition struct{ State, Reason, Error string }

// condition asks the Admin API for one Wait condition.
func condition(t *testing.T, e *env, cond string) (int, waitCondition) {
	t.Helper()
	resp, err := http.Get(e.inst.GatewayURL() + "/_emu/v1/conditions?" + url.Values{"for": {cond}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out struct {
		Conditions []waitCondition
		Error      string
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	if len(out.Conditions) == 1 {
		return resp.StatusCode, out.Conditions[0]
	}
	return resp.StatusCode, waitCondition{Error: out.Error}
}
