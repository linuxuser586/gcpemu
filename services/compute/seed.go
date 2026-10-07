package compute

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	computev1 "google.golang.org/api/compute/v1"
	"gopkg.in/yaml.v3"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
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
