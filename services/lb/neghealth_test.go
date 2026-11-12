package lb_test

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/emu"
)

// TestNEGEndpointHealth is the load balancer's answer to GKE's NEG
// readiness gate (ADR 0071): a NEG endpoint's state in each backend service
// with a health check that uses the NEG, as getHealth reports it, whether
// or not a forwarding rule routes to the backend service. A backend service
// without a health check is not listed.
func TestNEGEndpointHealth(t *testing.T) {
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
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	downPort := closed.Addr().(*net.TCPAddr).Port
	up := neg("up", &computev1.NetworkEndpoint{IpAddress: "127.0.0.1", Port: origin.port()},
		&computev1.NetworkEndpoint{IpAddress: "127.0.0.1", Port: int64(downPort)})
	plain := neg("plain", &computev1.NetworkEndpoint{IpAddress: "127.0.0.1", Port: origin.port()})
	neg("unused", &computev1.NetworkEndpoint{IpAddress: "127.0.0.1", Port: origin.port()})
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
	global("bare", "", up, plain)
	global("web", "hc", up)

	svc, _ := e.inst.Internal().Env.Lookup("lb")
	h, ok := svc.(emu.NEGHealth)
	if !ok {
		t.Fatal("the lb Service is not an emu.NEGHealth")
	}
	health := func(neg string, port int) string {
		return fmt.Sprint(h.NEGEndpointHealth(context.Background(), proj, "us-central1-a", neg, "127.0.0.1", port))
	}
	bs := func(name, state string) string {
		return fmt.Sprintf("{projects/%s/global/backendServices/%s %s}", proj, name, state)
	}
	// Health checks take a moment to pass, and to fail.
	wantUp := "[" + bs("api", "HEALTHY") + " " + bs("web", "HEALTHY") + "]"
	wantDown := "[" + bs("api", "UNHEALTHY") + " " + bs("web", "UNHEALTHY") + "]"
	for deadline := time.Now().Add(10 * time.Second); health("up", int(origin.port())) != wantUp || health("up", downPort) != wantDown; {
		if time.Now().After(deadline) {
			t.Fatalf("up's serving endpoint: %s, want %s; its closed one: %s, want %s",
				health("up", int(origin.port())), wantUp, health("up", downPort), wantDown)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got, want := health("up", 1), "["+bs("api", "")+" "+bs("web", "")+"]"; got != want {
		t.Errorf("an endpoint not in up: %s, want %s", got, want)
	}
	for _, n := range []string{"plain", "unused", "missing"} {
		if got := health(n, int(origin.port())); got != "[]" {
			t.Errorf("%s, used by no backend service with a health check: %s, want none", n, got)
		}
	}
}
