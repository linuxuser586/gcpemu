package secrets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"strings"

	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Secret IAM policies (FR-IAM-003) are kept by the iam service through
// emu.IAMPolicyStore; without it they are kept in the "secrets/iam" namespace.

func (s *Service) policyStore() emu.IAMPolicyStore {
	if svc, ok := s.env.Lookup("iam"); ok {
		if ps, ok := svc.(emu.IAMPolicyStore); ok {
			return ps
		}
	}
	return nil
}

// GetIamPolicy returns a secret's IAM policy.
func (a *api) GetIamPolicy(ctx context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	ref, err := a.s.iamSecret(ctx, req.GetResource(), "secretmanager.secrets.getIamPolicy")
	if err != nil {
		return nil, err
	}
	return a.s.getPolicy(ctx, ref.resource())
}

// SetIamPolicy replaces a secret's IAM policy.
func (a *api) SetIamPolicy(ctx context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	ref, err := a.s.iamSecret(ctx, req.GetResource(), "secretmanager.secrets.setIamPolicy")
	if err != nil {
		return nil, err
	}
	in := req.GetPolicy()
	if in == nil {
		in = &iampb.Policy{}
	}
	for _, b := range in.GetBindings() {
		if !strings.HasPrefix(b.GetRole(), "roles/") && !strings.HasPrefix(b.GetRole(), "projects/") && !strings.HasPrefix(b.GetRole(), "organizations/") {
			return nil, apierr.InvalidArgument("Role %s is not supported for this resource.", b.GetRole())
		}
	}
	return a.s.setPolicy(ctx, ref.resource(), in)
}

// TestIamPermissions returns the secretmanager.* permissions the caller
// holds on the secret.
func (a *api) TestIamPermissions(ctx context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	s := a.s
	ref, err := s.parseSecretOnly(req.GetResource())
	if err != nil {
		return nil, err
	}
	if err := s.env.Store.View(func(tx store.Tx) error { _, err := getSecret(tx, ref); return err }); err != nil {
		return nil, err
	}
	for _, p := range req.GetPermissions() {
		if !strings.HasPrefix(p, "secretmanager.") {
			return nil, apierr.InvalidArgument("Permission %s is not valid for this resource.", p)
		}
	}
	return &iampb.TestIamPermissionsResponse{Permissions: s.testPermissions(ctx, ref.resource(), req.GetPermissions())}, nil
}

// iamSecret resolves a policy resource to an existing secret and checks
// perm.
func (s *Service) iamSecret(ctx context.Context, resource, perm string) (secretRef, error) {
	ref, err := s.parseSecretOnly(resource)
	if err != nil {
		return ref, err
	}
	if err := s.check(ctx, perm, ref.resource()); err != nil {
		return ref, err
	}
	return ref, s.env.Store.View(func(tx store.Tx) error { _, err := getSecret(tx, ref); return err })
}

// testPermissions returns the permissions the caller holds on resource:
// all of them with IAM off, else the iam service's policy evaluation
// (emu.IAMPermissionTester), else the authorizer's verdict.
func (s *Service) testPermissions(ctx context.Context, resource string, perms []string) []string {
	if s.env.Auth.Mode() == config.IAMOff {
		return perms
	}
	if svc, ok := s.env.Lookup("iam"); ok {
		if pt, ok := svc.(emu.IAMPermissionTester); ok {
			return pt.TestPermissions(ctx, resource, perms)
		}
	}
	var out []string
	for _, p := range perms {
		if s.env.Auth.Check(ctx, p, resource) == nil {
			out = append(out, p)
		}
	}
	return out
}

func (s *Service) getPolicy(ctx context.Context, resource string) (*iampb.Policy, error) {
	pol := &iampb.Policy{}
	if ps := s.policyStore(); ps != nil {
		b, err := ps.GetPolicyJSON(ctx, resource)
		if err != nil {
			return nil, err
		}
		if len(bytes.TrimSpace(b)) > 0 {
			if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, pol); err != nil {
				return nil, apierr.Internal("decode policy: %v", err)
			}
		}
		return pol, nil
	}
	err := s.env.Store.View(func(tx store.Tx) error {
		if b, ok := tx.Get(nsIAM, resource); ok {
			return protojson.Unmarshal(b, pol)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(pol.Etag) == 0 {
		pol.Etag = policyEtag(pol)
	}
	return pol, nil
}

func (s *Service) setPolicy(ctx context.Context, resource string, in *iampb.Policy) (*iampb.Policy, error) {
	if ps := s.policyStore(); ps != nil {
		b, err := protojson.Marshal(in)
		if err != nil {
			return nil, err
		}
		out, err := ps.SetPolicyJSON(ctx, resource, b)
		if err != nil {
			return nil, err
		}
		pol := &iampb.Policy{}
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(out, pol); err != nil {
			return nil, apierr.Internal("decode policy: %v", err)
		}
		return pol, nil
	}
	pol := proto.Clone(in).(*iampb.Policy)
	err := s.env.Store.Update(func(tx store.Tx) error {
		cur := &iampb.Policy{}
		if b, ok := tx.Get(nsIAM, resource); ok {
			if err := protojson.Unmarshal(b, cur); err != nil {
				return err
			}
		}
		if len(cur.Etag) == 0 {
			cur.Etag = policyEtag(cur)
		}
		if len(in.GetEtag()) > 0 && !bytes.Equal(in.GetEtag(), cur.Etag) {
			return apierr.Aborted("There were concurrent policy changes. Please retry the whole read-modify-write with exponential backoff.")
		}
		if pol.Version == 0 {
			pol.Version = 1
		}
		pol.Etag = nil
		pol.Etag = policyEtag(pol)
		b, err := protojson.Marshal(pol)
		if err != nil {
			return err
		}
		return tx.Put(nsIAM, resource, b)
	})
	if err != nil {
		return nil, err
	}
	return pol, nil
}

// deletePolicy drops a deleted secret's policy from the iam service.
func (s *Service) deletePolicy(ctx context.Context, resource string) {
	if ps := s.policyStore(); ps != nil {
		_ = ps.DeletePolicy(ctx, resource)
	}
}

// policyEtag derives an etag from the policy content.
func policyEtag(p *iampb.Policy) []byte {
	c := proto.Clone(p).(*iampb.Policy)
	c.Etag = nil
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(c)
	sum := sha256.Sum256(b)
	return sum[:8]
}
