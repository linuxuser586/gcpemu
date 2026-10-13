package dns

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"

	dnsv1 "google.golang.org/api/dns/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Managed zone IAM policies (FR-IAM-003): managedZones getIamPolicy,
// setIamPolicy and testIamPermissions. Policies are kept by the iam service
// through emu.IAMPolicyStore; without it they are kept in nsIAM.

// zoneVerb serves POST .../managedZones/{zone}:verb.
func (s *Service) zoneVerb(w http.ResponseWriter, r *http.Request) {
	ref, verb, _ := strings.Cut(r.PathValue("managedZone"), ":")
	r.SetPathValue("managedZone", ref)
	var (
		out any
		err error
	)
	switch verb {
	case "getIamPolicy":
		out, err = s.getZoneIAM(r)
	case "setIamPolicy":
		out, err = s.setZoneIAM(r)
	case "testIamPermissions":
		out, err = s.testZoneIAM(r)
	default:
		err = apierr.NotFound("The requested URL %s was not found on this server.", r.URL.Path)
	}
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Service) getZoneIAM(r *http.Request) (*dnsv1.GoogleIamV1Policy, error) {
	zr, err := s.zoneFor(r, "dns.managedZones.getIamPolicy")
	if err != nil {
		return nil, err
	}
	return s.zonePolicy(r.Context(), zoneResource(zr.Project, zr.Zone.Name))
}

func (s *Service) setZoneIAM(r *http.Request) (*dnsv1.GoogleIamV1Policy, error) {
	zr, err := s.zoneFor(r, "dns.managedZones.setIamPolicy")
	if err != nil {
		return nil, err
	}
	var req dnsv1.GoogleIamV1SetIamPolicyRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	in := req.Policy
	if in == nil {
		in = &dnsv1.GoogleIamV1Policy{}
	}
	for _, b := range in.Bindings {
		if !strings.HasPrefix(b.Role, "roles/") && !strings.HasPrefix(b.Role, "projects/") && !strings.HasPrefix(b.Role, "organizations/") {
			return nil, apierr.InvalidArgument("Role %s is not supported for this resource.", b.Role)
		}
	}
	return s.setZonePolicy(r.Context(), zoneResource(zr.Project, zr.Zone.Name), in)
}

// testZoneIAM returns the dns.* permissions the caller holds on the zone:
// all of them with IAM off, else the iam service's policy evaluation, else
// the authorizer's verdict.
func (s *Service) testZoneIAM(r *http.Request) (*dnsv1.GoogleIamV1TestIamPermissionsResponse, error) {
	proj, err := s.projectParam(r)
	if err != nil {
		return nil, err
	}
	var zr *zoneRec
	if err := s.env.Store.View(func(tx store.Tx) error {
		zr, err = loadZone(tx, proj, r.PathValue("managedZone"))
		return err
	}); err != nil {
		return nil, err
	}
	var req dnsv1.GoogleIamV1TestIamPermissionsRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	for _, p := range req.Permissions {
		if !strings.HasPrefix(p, "dns.") {
			return nil, apierr.InvalidArgument("Permission %s is not valid for this resource.", p)
		}
	}
	ctx, res := r.Context(), zoneResource(zr.Project, zr.Zone.Name)
	out := &dnsv1.GoogleIamV1TestIamPermissionsResponse{Permissions: []string{}}
	switch {
	case s.env.Auth.Mode() == config.IAMOff:
		out.Permissions = append(out.Permissions, req.Permissions...)
	case s.permissionTester() != nil:
		out.Permissions = s.permissionTester().TestPermissions(ctx, res, req.Permissions)
	default:
		for _, p := range req.Permissions {
			if s.env.Auth.Check(ctx, p, res) == nil {
				out.Permissions = append(out.Permissions, p)
			}
		}
	}
	return out, nil
}

func (s *Service) policyStore() emu.IAMPolicyStore {
	if svc, ok := s.env.Lookup("iam"); ok {
		if ps, ok := svc.(emu.IAMPolicyStore); ok {
			return ps
		}
	}
	return nil
}

func (s *Service) permissionTester() emu.IAMPermissionTester {
	if svc, ok := s.env.Lookup("iam"); ok {
		if pt, ok := svc.(emu.IAMPermissionTester); ok {
			return pt
		}
	}
	return nil
}

func (s *Service) zonePolicy(ctx context.Context, resource string) (*dnsv1.GoogleIamV1Policy, error) {
	var b []byte
	if ps := s.policyStore(); ps != nil {
		var err error
		if b, err = ps.GetPolicyJSON(ctx, resource); err != nil {
			return nil, err
		}
	} else {
		_ = s.env.Store.View(func(tx store.Tx) error { b, _ = tx.Get(nsIAM, resource); return nil })
	}
	pol := &dnsv1.GoogleIamV1Policy{}
	if len(b) > 0 && string(b) != "null" {
		if err := json.Unmarshal(b, pol); err != nil {
			return nil, apierr.Internal("decode policy: %v", err)
		}
	}
	if pol.Etag == "" {
		pol.Etag = policyEtag(pol)
	}
	return pol, nil
}

func (s *Service) setZonePolicy(ctx context.Context, resource string, in *dnsv1.GoogleIamV1Policy) (*dnsv1.GoogleIamV1Policy, error) {
	if ps := s.policyStore(); ps != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		if b, err = ps.SetPolicyJSON(ctx, resource, b); err != nil {
			return nil, err
		}
		pol := &dnsv1.GoogleIamV1Policy{}
		if err := json.Unmarshal(b, pol); err != nil {
			return nil, apierr.Internal("decode policy: %v", err)
		}
		return pol, nil
	}
	pol := *in
	err := s.env.Store.Update(func(tx store.Tx) error {
		cur := &dnsv1.GoogleIamV1Policy{}
		if b, ok := tx.Get(nsIAM, resource); ok {
			if err := json.Unmarshal(b, cur); err != nil {
				return err
			}
		}
		if cur.Etag == "" {
			cur.Etag = policyEtag(cur)
		}
		if in.Etag != "" && in.Etag != cur.Etag {
			return apierr.Aborted("There were concurrent policy changes. Please retry the whole read-modify-write with exponential backoff.")
		}
		if pol.Version == 0 {
			pol.Version = 1
		}
		pol.Etag = ""
		pol.Etag = policyEtag(&pol)
		return store.PutJSON(tx, nsIAM, resource, &pol)
	})
	if err != nil {
		return nil, err
	}
	return &pol, nil
}

// deleteZonePolicy drops a deleted zone's policy from the iam service.
func (s *Service) deleteZonePolicy(ctx context.Context, resource string) {
	if ps := s.policyStore(); ps != nil {
		_ = ps.DeletePolicy(ctx, resource)
	}
}

// policyEtag derives an etag from the policy content.
func policyEtag(p *dnsv1.GoogleIamV1Policy) string {
	c := *p
	c.Etag = ""
	b, _ := json.Marshal(&c)
	sum := sha256.Sum256(b)
	return base64.StdEncoding.EncodeToString(sum[:8])
}
