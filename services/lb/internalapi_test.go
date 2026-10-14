package lb_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// GKE's Ingress and Gateway controllers (FR-GKE-010) manage load
// balancers through emu.LoadBalancerResources: resources are created and
// replaced with the API's validation and defaults, appear on the compute
// API, keep their IDs and addresses when replaced, and are deleted only
// once nothing uses them.
func TestLoadBalancerResources(t *testing.T) {
	e := start(t)
	svc, _ := e.inst.Env.Lookup("lb")
	lbr, ok := svc.(emu.LoadBalancerResources)
	if !ok {
		t.Fatal("the lb service does not implement emu.LoadBalancerResources")
	}
	ctx := context.Background()
	g := "projects/" + proj + "/global/"
	put := func(path string, body map[string]any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(body)
		out, err := lbr.PutLBResource(ctx, path, b)
		if err != nil {
			t.Fatalf("PutLBResource %s: %v", path, err)
		}
		var m map[string]any
		_ = json.Unmarshal(out, &m)
		return m
	}

	put(g+"backendServices/be", map[string]any{"loadBalancingScheme": "EXTERNAL_MANAGED"})
	put(g+"urlMaps/map", map[string]any{"defaultService": "global/backendServices/be"})
	put(g+"targetHttpProxies/p", map[string]any{"urlMap": "global/urlMaps/map"})
	fr := put(g+"forwardingRules/fr", map[string]any{"loadBalancingScheme": "EXTERNAL_MANAGED", "portRange": "80", "target": "global/targetHttpProxies/p"})
	if fr["IPAddress"] == "" || fr["IPAddress"] == nil || fr["name"] != "fr" {
		t.Fatalf("forwarding rule = %v, want a name and an ephemeral address", fr)
	}

	// The compute API serves them.
	um, err := e.c.UrlMaps.Get(proj, "map").Do()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(um.DefaultService, g+"backendServices/be") {
		t.Errorf("url map defaultService = %q", um.DefaultService)
	}

	// Replacing keeps the ID and, unless given, the address.
	put(g+"backendServices/be2", map[string]any{"loadBalancingScheme": "EXTERNAL_MANAGED"})
	um2 := put(g+"urlMaps/map", map[string]any{"defaultService": "global/backendServices/be2"})
	if id, _ := um2["id"].(string); id != strconv.FormatUint(um.Id, 10) {
		t.Errorf("replaced url map id = %v, want %d", um2["id"], um.Id)
	}
	fr2 := put(g+"forwardingRules/fr", map[string]any{"loadBalancingScheme": "EXTERNAL_MANAGED", "portRange": "80", "target": "global/targetHttpProxies/p"})
	if fr2["IPAddress"] != fr["IPAddress"] {
		t.Errorf("replaced forwarding rule address %v, want %v", fr2["IPAddress"], fr["IPAddress"])
	}
	if got, _ := e.c.UrlMaps.Get(proj, "map").Do(); got == nil || !strings.HasSuffix(got.DefaultService, "/be2") {
		t.Errorf("url map after replace: %+v", got)
	}

	// Validation applies.
	if _, err := lbr.PutLBResource(ctx, g+"urlMaps/bad", []byte(`{"defaultService":"global/backendServices/missing"}`)); err == nil {
		t.Error("url map to a missing backend service: no error")
	}
	if _, err := lbr.PutLBResource(ctx, g+"widgets/x", []byte(`{}`)); err == nil {
		t.Error("unknown collection: no error")
	}

	// Get, and delete in dependency order.
	b, err := lbr.LBResource(ctx, g+"targetHttpProxies/p")
	if err != nil || len(b) == 0 {
		t.Fatalf("LBResource: %v", err)
	}
	if _, err := lbr.LBResource(ctx, g+"targetHttpProxies/missing"); err == nil || apierr.From(err).Code != codes.NotFound {
		t.Errorf("LBResource of a missing resource: %v, want NOT_FOUND", err)
	}
	if err := lbr.DeleteLBResource(ctx, g+"urlMaps/map"); err == nil {
		t.Error("deleting a url map a proxy uses: no error")
	}
	for _, p := range []string{"forwardingRules/fr", "targetHttpProxies/p", "urlMaps/map", "backendServices/be", "backendServices/be2", "backendServices/be"} {
		if err := lbr.DeleteLBResource(ctx, g+p); err != nil {
			t.Errorf("DeleteLBResource %s: %v", p, err)
		}
	}
	if _, err := e.c.GlobalForwardingRules.Get(proj, "fr").Do(); err == nil {
		t.Error("forwarding rule still served after delete")
	}
}
