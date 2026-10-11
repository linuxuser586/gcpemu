package compute

import (
	"encoding/json"
	"net/http"
	"strings"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// Subnetwork IAM policies (FR-IAM-003), the compute resource with IAM that
// the emulator models (google_compute_subnetwork_iam_*). The iam service,
// which compute depends on, keeps them under the subnetwork's full
// resource name, so project bindings apply too.

func (s *Service) policyStore() (emu.IAMPolicyStore, error) {
	if svc, ok := s.env.Lookup("iam"); ok {
		if ps, ok := svc.(emu.IAMPolicyStore); ok {
			return ps, nil
		}
	}
	return nil, apierr.FailedPrecondition("IAM policies need the iam service.")
}

func (s *Service) getSubnetworkIAM(w http.ResponseWriter, r *http.Request) {
	path, _, err := s.loadSubnet(r, "compute.subnetworks.getIamPolicy")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	ps, err := s.policyStore()
	if err != nil {
		apierr.Write(w, err)
		return
	}
	b, err := ps.GetPolicyJSON(r.Context(), "//compute.googleapis.com/"+path)
	reply(w, json.RawMessage(b), err)
}

func (s *Service) setSubnetworkIAM(w http.ResponseWriter, r *http.Request) {
	path, _, err := s.loadSubnet(r, "compute.subnetworks.setIamPolicy")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var req computev1.RegionSetPolicyRequest
	if _, err := decode(r, &req); err != nil {
		apierr.Write(w, err)
		return
	}
	pol := req.Policy
	if pol == nil {
		pol = &computev1.Policy{}
	}
	if req.Bindings != nil {
		pol.Bindings = req.Bindings
	}
	if req.Etag != "" {
		pol.Etag = req.Etag
	}
	ps, err := s.policyStore()
	if err != nil {
		apierr.Write(w, err)
		return
	}
	in, _ := json.Marshal(pol)
	b, err := ps.SetPolicyJSON(r.Context(), "//compute.googleapis.com/"+path, in)
	reply(w, json.RawMessage(b), err)
}

// testSubnetworkIAM returns the compute.* permissions the caller holds on
// the subnetwork.
func (s *Service) testSubnetworkIAM(w http.ResponseWriter, r *http.Request) {
	path, _, err := s.loadSubnet(r, "compute.subnetworks.get")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var req computev1.TestPermissionsRequest
	if _, err := decode(r, &req); err != nil {
		apierr.Write(w, err)
		return
	}
	for _, p := range req.Permissions {
		if !strings.HasPrefix(p, "compute.") {
			apierr.Write(w, apierr.InvalidArgument("Permission %s is not valid for this resource.", p).WithLegacy("invalid"))
			return
		}
	}
	out := &computev1.TestPermissionsResponse{Permissions: []string{}}
	if s.env.Auth.Mode() == config.IAMOff {
		out.Permissions = req.Permissions
	} else if svc, ok := s.env.Lookup("iam"); ok {
		if pt, ok := svc.(emu.IAMPermissionTester); ok {
			out.Permissions = pt.TestPermissions(r.Context(), "//compute.googleapis.com/"+path, req.Permissions)
		}
	}
	writeJSON(w, http.StatusOK, out)
}
