package compute

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"time"

	computev1 "google.golang.org/api/compute/v1"
	"gopkg.in/yaml.v3"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

var (
	_ emu.Seeder       = (*Service)(nil)
	_ emu.SeedExporter = (*Service)(nil)
)

// Seed file section (FR-CORE-011). Resources use the compute v1 JSON field
// names; references may be short names. Existing resources are left
// unchanged, so applying a seed is idempotent.
//
//	compute:
//	  project: my-project              # default for entries without one
//	  networks:
//	    - name: vpc
//	      autoCreateSubnetworks: false
//	  subnetworks:
//	    - name: nodes
//	      region: us-central1
//	      network: vpc
//	      ipCidrRange: 10.0.0.0/24
//	      secondaryIpRanges: [{rangeName: pods, ipCidrRange: 10.4.0.0/14}]
//	  firewalls:
//	    - name: allow-internal
//	      network: vpc
//	      sourceRanges: [10.0.0.0/8]
//	      allowed: [{IPProtocol: tcp}]
//	  routes: []
//	  addresses:                       # with region: regional, else global
//	    - name: nat-ip
//	      region: us-central1
//	    - name: psa-range
//	      addressType: INTERNAL
//	      purpose: VPC_PEERING
//	      prefixLength: 16
//	      network: vpc
//	  routers:
//	    - name: router
//	      region: us-central1
//	      network: vpc
//	      nats:
//	        - name: nat
//	          natIpAllocateOption: AUTO_ONLY
//	          sourceSubnetworkIpRangesToNat: ALL_SUBNETWORKS_ALL_IP_RANGES
//	  serviceNetworkingConnections:
//	    - network: vpc
//	      reservedPeeringRanges: [psa-range]

type seedSection struct {
	Project     string           `yaml:"project"`
	Networks    []map[string]any `yaml:"networks"`
	Subnetworks []map[string]any `yaml:"subnetworks"`
	Firewalls   []map[string]any `yaml:"firewalls"`
	Routes      []map[string]any `yaml:"routes"`
	Addresses   []map[string]any `yaml:"addresses"`
	Routers     []map[string]any `yaml:"routers"`
	Connections []map[string]any `yaml:"serviceNetworkingConnections"`
}

// ApplySeed implements emu.Seeder by replaying the entries through the
// REST handlers, so they get exactly the API's validation and defaults.
func (s *Service) ApplySeed(ctx context.Context, section *yaml.Node, baseDir string) error {
	var sec seedSection
	if err := section.Decode(&sec); err != nil {
		return err
	}
	type step struct {
		kind     string
		items    []map[string]any
		regional bool
		coll     string
	}
	steps := []step{
		{"networks", sec.Networks, false, "global/networks"},
		{"subnetworks", sec.Subnetworks, true, "subnetworks"},
		{"firewalls", sec.Firewalls, false, "global/firewalls"},
		{"routes", sec.Routes, false, "global/routes"},
		{"addresses", sec.Addresses, false, "addresses"},
		{"routers", sec.Routers, true, "routers"},
	}
	for _, st := range steps {
		for i, item := range st.items {
			proj, _ := item["project"].(string)
			if proj == "" {
				proj = sec.Project
			}
			if proj == "" {
				return fmt.Errorf("compute.%s[%d]: project is required", st.kind, i)
			}
			if err := s.env.EnsureProject(proj); err != nil {
				return fmt.Errorf("compute.%s[%d]: %w", st.kind, i, err)
			}
			region, _ := item["region"].(string)
			body := map[string]any{}
			for k, v := range item {
				if k != "project" && k != "region" {
					body[k] = v
				}
			}
			var coll string
			switch {
			case st.kind == "addresses" && region != "":
				coll = "regions/" + region + "/addresses"
			case st.kind == "addresses":
				coll = "global/addresses"
			case st.regional:
				if region == "" {
					return fmt.Errorf("compute.%s[%d]: region is required", st.kind, i)
				}
				coll = "regions/" + region + "/" + st.coll
			default:
				coll = st.coll
			}
			if st.kind == "networks" {
				if _, ok := body["autoCreateSubnetworks"]; !ok {
					body["autoCreateSubnetworks"] = false
				}
			}
			if err := s.seedInsert(ctx, proj, coll, body); err != nil {
				return fmt.Errorf("compute.%s[%d] %v: %w", st.kind, i, item["name"], err)
			}
		}
	}
	for i, c := range sec.Connections {
		proj, _ := c["project"].(string)
		if proj == "" {
			proj = sec.Project
		}
		netRef, _ := c["network"].(string)
		np, err := resolveNetworkRef(proj, netRef)
		if err != nil {
			return fmt.Errorf("compute.serviceNetworkingConnections[%d]: %w", i, err)
		}
		var ranges []string
		if rs, ok := c["reservedPeeringRanges"].([]any); ok {
			for _, r := range rs {
				ranges = append(ranges, fmt.Sprint(r))
			}
		}
		exists := false
		_ = s.env.Store.View(func(tx store.Tx) error { exists = store.Exists(tx, nsPSA, np); return nil })
		if exists {
			continue
		}
		if err := s.putConnection(proj, np, ranges, false); err != nil {
			return fmt.Errorf("compute.serviceNetworkingConnections[%d]: %w", i, err)
		}
	}
	return nil
}

// seedItem is one exported entry: its addressing keys, then its compute
// v1 JSON fields.
type seedItem struct {
	Name    string         `yaml:"name,omitempty"`
	Project string         `yaml:"project,omitempty"`
	Region  string         `yaml:"region,omitempty"`
	Fields  map[string]any `yaml:",inline"`
}

// seedExport is the exported "compute" section.
type seedExport struct {
	Project     string     `yaml:"project,omitempty"`
	Networks    []seedItem `yaml:"networks,omitempty"`
	Subnetworks []seedItem `yaml:"subnetworks,omitempty"`
	Firewalls   []seedItem `yaml:"firewalls,omitempty"`
	Routes      []seedItem `yaml:"routes,omitempty"`
	Addresses   []seedItem `yaml:"addresses,omitempty"`
	Routers     []seedItem `yaml:"routers,omitempty"`
	Connections []seedItem `yaml:"serviceNetworkingConnections,omitempty"`
}

// serverFields are output-only or server-assigned top-level fields, left
// out of the export.
var serverFields = []string{
	"kind", "id", "creationTimestamp", "selfLink", "selfLinkWithId", "fingerprint", "labelFingerprint",
	"status", "users", "region", "gatewayAddress", "gatewayIPv4", "peerings", "state", "routeType",
}

// ExportSeed implements emu.SeedExporter (FR-CORE-013). Resources a
// network creates itself (an auto-mode network's subnetworks, its default
// route) are left out; reserved addresses keep their IP. VPC Network
// Peerings and network endpoint groups have no seed entries and are not
// exported.
func (s *Service) ExportSeed(ctx context.Context) (any, error) {
	var out seedExport
	projects := map[string]bool{}
	add := func(list *[]seedItem, path string, v any, kind string) {
		proj, loc, name := pathParts(path)
		region := ""
		if loc != "global" {
			region = loc
		}
		// Numbers are decoded exactly (integers stay integers in YAML).
		b, _ := json.Marshal(v)
		var m map[string]any
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		_ = dec.Decode(&m)
		m = shortenRefs(m, proj, region).(map[string]any)
		for _, f := range serverFields {
			delete(m, f)
		}
		delete(m, "name")
		dropDefaults(kind, m, region)
		*list = append(*list, seedItem{Name: name, Project: proj, Region: region, Fields: m})
		projects[proj] = true
	}
	err := s.env.Store.View(func(tx store.Tx) error {
		auto := map[string]bool{} // auto-mode network paths
		for _, n := range list[computev1.Network](tx, nsNetworks, "projects/") {
			if n.AutoCreateSubnetworks {
				auto[relPath(n.SelfLink)] = true
			}
			add(&out.Networks, relPath(n.SelfLink), n, "networks")
		}
		for _, sn := range list[computev1.Subnetwork](tx, nsSubnets, "projects/") {
			if auto[relPath(sn.Network)] && sn.Name == lastSeg(sn.Network) {
				continue
			}
			add(&out.Subnetworks, relPath(sn.SelfLink), sn, "subnetworks")
		}
		for _, fw := range list[computev1.Firewall](tx, nsFirewalls, "projects/") {
			add(&out.Firewalls, relPath(fw.SelfLink), fw, "firewalls")
		}
		for _, rt := range list[computev1.Route](tx, nsRoutes, "projects/") {
			if rt.Description == defaultRouteDesc && strings.HasPrefix(rt.Name, "default-route-") {
				continue
			}
			add(&out.Routes, relPath(rt.SelfLink), rt, "routes")
		}
		for _, ns := range []string{nsGlobalAddresses, nsAddresses} {
			for _, a := range list[computev1.Address](tx, ns, "projects/") {
				add(&out.Addresses, relPath(a.SelfLink), a, "addresses")
			}
		}
		for _, rt := range list[computev1.Router](tx, nsRouters, "projects/") {
			add(&out.Routers, relPath(rt.SelfLink), rt, "routers")
		}
		for _, c := range list[psaConnection](tx, nsPSA, "projects/") {
			proj, _, name := pathParts(c.Network)
			projects[proj] = true
			out.Connections = append(out.Connections, seedItem{Project: proj, Fields: map[string]any{
				"network": name, "reservedPeeringRanges": c.ReservedPeeringRanges,
			}})
		}
		return nil
	})
	if err != nil || len(projects) == 0 {
		return nil, err
	}
	lists := []*[]seedItem{&out.Networks, &out.Subnetworks, &out.Firewalls, &out.Routes, &out.Addresses, &out.Routers, &out.Connections}
	for _, l := range lists {
		sort.SliceStable(*l, func(i, j int) bool {
			a, b := (*l)[i], (*l)[j]
			if a.Project != b.Project {
				return a.Project < b.Project
			}
			if a.Name != b.Name {
				return a.Name < b.Name
			}
			return a.Region < b.Region
		})
	}
	if len(projects) == 1 {
		for p := range projects {
			out.Project = p
		}
		for _, l := range lists {
			for i := range *l {
				(*l)[i].Project = ""
			}
		}
	}
	return out, nil
}

// seedDefaults are the values the API fills in for absent fields, per seed
// list, left out of the export.
var seedDefaults = map[string]map[string]any{
	"networks":    {"mtu": int64(1460), "networkFirewallPolicyEnforcementOrder": "AFTER_CLASSIC_FIREWALL"},
	"subnetworks": {"purpose": "PRIVATE", "stackType": "IPV4_ONLY", "privateIpv6GoogleAccess": "DISABLE_GOOGLE_ACCESS"},
	"firewalls":   {"direction": "INGRESS"},
	"addresses":   {"addressType": "EXTERNAL", "networkTier": "PREMIUM", "ipVersion": "IPV4"},
}

// dropDefaults removes from m, an entry of the seed list kind in region
// ("" when global), the fields the API would fill in the same way, and
// the output-only ones of that kind.
func dropDefaults(kind string, m map[string]any, region string) {
	for k, def := range seedDefaults[kind] {
		if m[k] == def {
			delete(m, k)
		}
	}
	switch kind {
	case "networks":
		delete(m, "subnetworks")
		if rc, ok := m["routingConfig"].(map[string]any); ok {
			if rc["routingMode"] == "REGIONAL" {
				delete(rc, "routingMode")
			}
			if rc["bgpBestPathSelectionMode"] == "LEGACY" {
				delete(rc, "bgpBestPathSelectionMode")
			}
		}
	case "firewalls", "routes":
		// Priority 0 is left out of the stored JSON; absent means 1000.
		switch m["priority"] {
		case nil:
			m["priority"] = int64(0)
		case int64(1000):
			delete(m, "priority")
		}
	case "addresses":
		if region != "" && m["addressType"] == "INTERNAL" && m["purpose"] == "GCE_ENDPOINT" {
			delete(m, "purpose")
		}
	case "routers":
		nats, _ := m["nats"].([]any)
		for _, n := range nats {
			if nat, ok := n.(map[string]any); ok {
				if nat["type"] == "PUBLIC" {
					delete(nat, "type")
				}
				if ets, _ := nat["endpointTypes"].([]any); len(ets) == 1 && ets[0] == "ENDPOINT_TYPE_VM" {
					delete(nat, "endpointTypes")
				}
			}
		}
	}
	for k, v := range m {
		if sub, ok := v.(map[string]any); ok && len(sub) == 0 {
			delete(m, k)
		}
	}
}

// shortenRefs rewrites the compute URLs in v within project proj to the
// partial paths and short names a seed entry in region accepts, and
// converts numbers to int64 or float64.
func shortenRefs(v any, proj, region string) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = shortenRefs(e, proj, region)
		}
	case []any:
		for i, e := range x {
			x[i] = shortenRefs(e, proj, region)
		}
	case string:
		p, ok := strings.CutPrefix(x, selfLinkBase+"projects/"+proj+"/")
		if !ok {
			return x
		}
		switch segs := strings.Split(p, "/"); {
		case len(segs) == 3 && segs[0] == "global" && segs[1] == "networks":
			return segs[2]
		case len(segs) == 4 && segs[0] == "regions" && segs[1] == region && (segs[2] == "subnetworks" || segs[2] == "addresses"):
			return segs[3]
		}
		return p
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		f, _ := x.Float64()
		return f
	}
	return v
}

// seedInsert POSTs body to a collection, ignoring "already exists", and
// waits for the operation.
func (s *Service) seedInsert(ctx context.Context, proj, coll string, body map[string]any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req := httptest.NewRequest(http.MethodPost, "/compute/v1/projects/"+proj+"/"+coll, bytes.NewReader(b)).WithContext(ctx)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code == http.StatusConflict {
		return nil
	}
	if rec.Code != http.StatusOK {
		var e struct {
			Error struct{ Message string } `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		return apierr.InvalidArgument("%s", e.Error.Message)
	}
	var op computev1.Operation
	if err := json.Unmarshal(rec.Body.Bytes(), &op); err != nil {
		return err
	}
	return s.awaitOp(ctx, relPath(op.SelfLink))
}

// awaitOp waits for a stored operation and returns its error.
func (s *Service) awaitOp(ctx context.Context, path string) error {
	for {
		var op *computev1.Operation
		_ = s.env.Store.View(func(tx store.Tx) error { op, _ = get[computev1.Operation](tx, nsOps, path); return nil })
		if op == nil {
			return fmt.Errorf("operation %s vanished", path)
		}
		if op.Status == "DONE" {
			if op.Error != nil && len(op.Error.Errors) > 0 {
				if op.Error.Errors[0].Code == "ALREADY_EXISTS" {
					return nil
				}
				return fmt.Errorf("%s", op.Error.Errors[0].Message)
			}
			return nil
		}
		s.opMu.Lock()
		wake := s.opWake
		s.opMu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wake:
		case <-time.After(time.Second):
		}
	}
}
