package iam

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"slices"
	"strings"

	iamv1 "google.golang.org/api/iam/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// emptyEtag is the etag GCP reports for a resource that never had a policy.
const emptyEtag = "ACAB"

var roleNameRE = regexp.MustCompile(`^[a-zA-Z0-9_]+\.[a-zA-Z0-9_.]+$`)

var memberRE = regexp.MustCompile(`^(allUsers|allAuthenticatedUsers|(user|serviceAccount|group|domain|projectOwner|projectEditor|projectViewer|principal|principalSet):.+|deleted:(user|serviceAccount|group):.+)$`)

// loadPolicy returns the stored policy of resource, or an empty one.
func loadPolicy(tx store.Tx, resource string) *iamv1.Policy {
	var p iamv1.Policy
	if store.GetJSON(tx, nsPolicies, resource, &p) != nil {
		return &iamv1.Policy{Version: 1, Etag: emptyEtag}
	}
	return &p
}

// newEtag returns a fresh base64 policy etag.
func (s *Service) newEtag() string {
	b, _ := hex.DecodeString(s.env.IDs.Hex(8))
	return base64.StdEncoding.EncodeToString(b)
}

// storePolicy implements setIamPolicy semantics (FR-IAM-003): etag
// concurrency, updateMask, validation and normalisation. project scopes
// custom-role lookups ("" for none).
func (s *Service) storePolicy(tx store.Tx, resource string, in *iamv1.Policy, updateMask string) (*iamv1.Policy, error) {
	if in == nil {
		return nil, apierr.InvalidArgument("Request contains an invalid argument.").WithReason("googleapis.com", "INVALID_ARGUMENT")
	}
	cur := loadPolicy(tx, resource)
	if in.Etag != "" && in.Etag != cur.Etag {
		return nil, apierr.Aborted("There were concurrent policy changes. Please retry the whole read-modify-write with exponential backoff.").
			WithReason(iamDomain, "ETAG_MISMATCH")
	}
	out := &iamv1.Policy{Bindings: cur.Bindings, AuditConfigs: cur.AuditConfigs}
	mask := map[string]bool{"bindings": true, "etag": true}
	if updateMask != "" {
		mask = map[string]bool{}
		for _, f := range strings.Split(updateMask, ",") {
			mask[strings.TrimSpace(f)] = true
		}
	}
	if mask["bindings"] {
		out.Bindings = in.Bindings
	}
	if mask["auditConfigs"] || (updateMask == "" && in.AuditConfigs != nil) {
		out.AuditConfigs = in.AuditConfigs
	}
	bindings, err := s.normaliseBindings(tx, resource, out.Bindings)
	if err != nil {
		return nil, err
	}
	out.Bindings = bindings
	out.Version = 1
	for _, b := range out.Bindings {
		if b.Condition != nil {
			out.Version = 3
		}
	}
	if in.Version > out.Version && in.Version <= 3 {
		out.Version = in.Version
	}
	out.Etag = s.newEtag()
	if err := store.PutJSON(tx, nsPolicies, resource, out); err != nil {
		return nil, err
	}
	return out, nil
}

// normaliseBindings validates roles and members, merges bindings with the
// same role and condition, de-duplicates and sorts members and drops
// bindings without members, as GCP does.
func (s *Service) normaliseBindings(tx store.Tx, resource string, in []*iamv1.Binding) ([]*iamv1.Binding, error) {
	var out []*iamv1.Binding
	index := map[string]*iamv1.Binding{}
	for _, b := range in {
		if b == nil {
			continue
		}
		if b.Role == "" {
			return nil, apierr.InvalidArgument("Role must be specified in a binding.")
		}
		if err := s.validateRole(tx, b.Role); err != nil {
			return nil, err
		}
		key := b.Role
		if b.Condition != nil {
			if err := validateCondition(b.Condition); err != nil {
				return nil, err
			}
			c, _ := json.Marshal(b.Condition)
			key += "\x00" + string(c)
		}
		nb, ok := index[key]
		if !ok {
			nb = &iamv1.Binding{Role: b.Role, Condition: b.Condition}
			index[key] = nb
			out = append(out, nb)
		}
		for _, m := range b.Members {
			if !memberRE.MatchString(m) {
				return nil, apierr.InvalidArgument("Invalid member: %s", m).WithReason(iamDomain, "INVALID_MEMBER")
			}
			if err := s.validateMemberExists(tx, m); err != nil {
				return nil, err
			}
			if !slices.Contains(nb.Members, m) {
				nb.Members = append(nb.Members, m)
			}
		}
	}
	out = slices.DeleteFunc(out, func(b *iamv1.Binding) bool { return len(b.Members) == 0 })
	for _, b := range out {
		slices.Sort(b.Members)
	}
	return out, nil
}

// validateRole checks a role is predefined or an existing custom role.
func (s *Service) validateRole(tx store.Tx, role string) error {
	if name, ok := strings.CutPrefix(role, "roles/"); ok {
		if _, ok := s.cat.roles[role]; ok {
			return nil
		}
		// Roles of services the emulator does not model (logging,
		// monitoring, run, ...) are accepted and grant nothing; unknown
		// roles of emulated services are rejected as GCP would.
		svc, _, _ := strings.Cut(name, ".")
		if roleNameRE.MatchString(name) && !s.cat.services[svc] {
			return nil
		}
		return apierr.InvalidArgument("Role %s is not supported for this resource.", role).WithReason(iamDomain, "ROLE_NOT_SUPPORTED")
	}
	if strings.HasPrefix(role, "projects/") || strings.HasPrefix(role, "organizations/") {
		var r iamv1.Role
		if store.GetJSON(tx, nsRoles, role, &r) == nil && !r.Deleted {
			return nil
		}
		return apierr.InvalidArgument("Role (%s) does not exist in the resource's hierarchy.", role).WithReason(iamDomain, "ROLE_NOT_FOUND")
	}
	return apierr.InvalidArgument("Role %s is not supported for this resource.", role).WithReason(iamDomain, "ROLE_NOT_SUPPORTED")
}

// validateMemberExists rejects bindings to service accounts that do not
// exist in a project the emulator manages.
func (s *Service) validateMemberExists(tx store.Tx, member string) error {
	email, ok := strings.CutPrefix(member, "serviceAccount:")
	if !ok || projectOfEmail(email) == "" || isServiceAgent(email) {
		return nil
	}
	if _, ok := s.getAccount(tx, email); ok {
		return nil
	}
	return apierr.InvalidArgument("Service account %s does not exist.", email).WithReason(iamDomain, "SERVICE_ACCOUNT_NOT_FOUND")
}

// isServiceAgent reports whether email is a Google-managed service agent
// (service-NUMBER@gs-project-accounts.iam.gserviceaccount.com,
// service-NUMBER@gcp-sa-SERVICE.iam.gserviceaccount.com, ...). They live
// in Google's projects, always exist and are bound in user policies, e.g.
// the Cloud Storage agent publishing bucket notifications.
func isServiceAgent(email string) bool {
	local, dom, _ := strings.Cut(email, "@")
	p := strings.TrimSuffix(dom, ".iam.gserviceaccount.com")
	if p == "gs-project-accounts" || strings.HasPrefix(p, "gcp-sa-") {
		return true
	}
	n, ok := strings.CutPrefix(local, "service-")
	return ok && n != "" && strings.Trim(n, "0123456789") == "" && p != dom
}

// GetPolicyJSON implements emu.IAMPolicyStore.
func (s *Service) GetPolicyJSON(ctx context.Context, resource string) ([]byte, error) {
	var p *iamv1.Policy
	_ = s.env.Store.View(func(tx store.Tx) error { p = loadPolicy(tx, resource); return nil })
	return json.Marshal(p)
}

// SetPolicyJSON implements emu.IAMPolicyStore: it replaces the policy's
// bindings (and audit configs if present), enforcing etag concurrency, and
// returns the stored policy.
func (s *Service) SetPolicyJSON(ctx context.Context, resource string, policy []byte) ([]byte, error) {
	var in iamv1.Policy
	if err := json.Unmarshal(policy, &in); err != nil {
		return nil, apierr.InvalidArgument("Invalid IAM policy: %v", err)
	}
	var out *iamv1.Policy
	err := s.env.Store.Update(func(tx store.Tx) error {
		var err error
		out, err = s.storePolicy(tx, resource, &in, "")
		return err
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

// DeletePolicy implements emu.IAMPolicyStore; it also forgets the parent.
func (s *Service) DeletePolicy(ctx context.Context, resource string) error {
	return s.env.Store.Update(func(tx store.Tx) error {
		if err := tx.Delete(nsPolicies, resource); err != nil {
			return err
		}
		return tx.Delete(nsParents, resource)
	})
}

// SetResourceParent implements emu.IAMResourceParents.
func (s *Service) SetResourceParent(ctx context.Context, resource, parent string) error {
	if strings.HasPrefix(parent, "projects/") {
		parent = projectResource(strings.TrimPrefix(parent, "projects/"))
	}
	return s.env.Store.Update(func(tx store.Tx) error {
		return tx.Put(nsParents, resource, []byte(parent))
	})
}

// TestPermissions implements emu.IAMPermissionTester.
func (s *Service) TestPermissions(ctx context.Context, resource string, permissions []string) []string {
	p := emu.PrincipalFrom(ctx)
	out := []string{}
	for _, perm := range permissions {
		if s.Allowed(ctx, p, perm, resource) {
			out = append(out, perm)
		}
	}
	return out
}
