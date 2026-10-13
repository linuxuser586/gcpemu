package gcs

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	storage "google.golang.org/api/storage/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Bucket IAM policies (FR-IAM-003). Policies live in the IAM service's
// policy store when iam is running, otherwise in this package's namespace.

// policyStore returns the IAM service's policy store, or nil.
func (s *Service) policyStore() emu.IAMPolicyStore {
	svc, ok := s.env.Lookup("iam")
	if !ok {
		return nil
	}
	ps, _ := svc.(emu.IAMPolicyStore)
	return ps
}

// localPolicy is the fallback stored policy.
type localPolicy struct {
	Policy *storage.Policy `json:"policy"`
	Seq    int64           `json:"seq"`
}

// defaultPolicy is the policy GCS gives a new bucket: legacy bucket roles
// for the project's convenience groups.
func defaultPolicy(rec *bucketRec) *storage.Policy {
	p := rec.Project
	return &storage.Policy{
		Bindings: []*storage.PolicyBindings{
			{Role: "roles/storage.legacyBucketOwner", Members: []string{"projectEditor:" + p, "projectOwner:" + p}},
			{Role: "roles/storage.legacyBucketReader", Members: []string{"projectViewer:" + p}},
		},
		Etag:    bucketEtag(1),
		Version: 1,
	}
}

// policyTarget is a resource with its own IAM policy: a bucket or a
// managed folder.
type policyTarget struct {
	// resource is the IAM full resource name, resourceID the policy's
	// resourceId.
	resource, resourceID string
	// localKey is the nsIAM key used when iam is absent.
	localKey string
	// set records whether setIamPolicy was ever called; until then def is
	// the policy.
	set bool
	def *storage.Policy
	// markSet records that a policy was set, failing when the resource is
	// gone.
	markSet func(tx store.Tx) error
}

func bucketTarget(rec *bucketRec) *policyTarget {
	name := rec.Bucket.Name
	return &policyTarget{
		resource:   bucketResource(name),
		resourceID: "projects/_/buckets/" + name,
		localKey:   name,
		set:        rec.PolicySet,
		def:        defaultPolicy(rec),
		markSet: func(tx store.Tx) error {
			brec, err := loadBucket(tx, name)
			if err != nil || brec.PolicySet {
				return err
			}
			brec.PolicySet = true
			return store.PutJSON(tx, nsBuckets, name, brec)
		},
	}
}

// getPolicy returns the current policy of t.
func (s *Service) getPolicy(ctx context.Context, t *policyTarget) (*storage.Policy, error) {
	if !t.set {
		return t.def, nil
	}
	if ps := s.policyStore(); ps != nil {
		b, err := ps.GetPolicyJSON(ctx, t.resource)
		if err != nil {
			return nil, err
		}
		var p storage.Policy
		if len(b) > 0 {
			if err := json.Unmarshal(b, &p); err != nil {
				return nil, apierr.Internal("decode policy: %v", err)
			}
		}
		return &p, nil
	}
	var lp localPolicy
	err := s.env.Store.View(func(tx store.Tx) error { return store.GetJSON(tx, nsIAM, t.localKey, &lp) })
	if err == store.ErrNotFound {
		return t.def, nil
	}
	if err != nil {
		return nil, err
	}
	return lp.Policy, nil
}

func renderPolicy(p *storage.Policy, resourceID string) *storage.Policy {
	out := *p
	out.Kind = "storage#policy"
	out.ResourceId = resourceID
	if out.Version == 0 {
		out.Version = 1
	}
	return &out
}

func (s *Service) getBucketIAM(w http.ResponseWriter, r *http.Request, bucket string) {
	if err := s.check(r.Context(), "storage.buckets.getIamPolicy", bucketResource(bucket)); err != nil {
		apierr.Write(w, err)
		return
	}
	rec, err := s.getBucketRec(bucket)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	s.writePolicy(w, r, bucketTarget(rec))
}

// writePolicy answers getIamPolicy for t.
func (s *Service) writePolicy(w http.ResponseWriter, r *http.Request, t *policyTarget) {
	p, err := s.getPolicy(r.Context(), t)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, renderPolicy(p, t.resourceID))
}

func (s *Service) setBucketIAM(w http.ResponseWriter, r *http.Request, bucket string) {
	ctx := r.Context()
	if err := s.check(ctx, "storage.buckets.setIamPolicy", bucketResource(bucket)); err != nil {
		apierr.Write(w, err)
		return
	}
	var p storage.Policy
	if err := decodeJSON(r, &p); err != nil {
		apierr.Write(w, err)
		return
	}
	rec, err := s.getBucketRec(bucket)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	t := bucketTarget(rec)
	out, err := s.putPolicy(ctx, t, &p)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, renderPolicy(out, t.resourceID))
}

// setPolicy stores a bucket policy with etag concurrency control.
func (s *Service) setPolicy(ctx context.Context, bucket string, p *storage.Policy) (*storage.Policy, error) {
	rec, err := s.getBucketRec(bucket)
	if err != nil {
		return nil, err
	}
	return s.putPolicy(ctx, bucketTarget(rec), p)
}

// putPolicy stores t's policy with etag concurrency control.
func (s *Service) putPolicy(ctx context.Context, t *policyTarget, p *storage.Policy) (*storage.Policy, error) {
	cur, err := s.getPolicy(ctx, t)
	if err != nil {
		return nil, err
	}
	for _, b := range p.Bindings {
		if !strings.HasPrefix(b.Role, "roles/") && !strings.HasPrefix(b.Role, "projects/") && !strings.HasPrefix(b.Role, "organizations/") {
			return nil, errInvalid("Role %s is not a valid role.", b.Role)
		}
	}
	if p.Etag != "" && p.Etag != cur.Etag {
		return nil, errPrecondition()
	}
	p.Kind, p.ResourceId = "", ""
	var out *storage.Policy
	if ps := s.policyStore(); ps != nil {
		if !t.set {
			p.Etag = ""
		}
		in, _ := json.Marshal(p)
		b, err := ps.SetPolicyJSON(ctx, t.resource, in)
		if err != nil {
			return nil, err
		}
		out = &storage.Policy{}
		if err := json.Unmarshal(b, out); err != nil {
			return nil, apierr.Internal("decode policy: %v", err)
		}
	}
	err = s.env.Store.Update(func(tx store.Tx) error {
		if err := t.markSet(tx); err != nil {
			return err
		}
		if out != nil {
			return nil
		}
		var lp localPolicy
		_ = store.GetJSON(tx, nsIAM, t.localKey, &lp)
		if lp.Seq == 0 {
			lp.Seq = 1
		}
		lp.Seq++
		p.Etag = bucketEtag(lp.Seq)
		lp.Policy = p
		out = p
		return store.PutJSON(tx, nsIAM, t.localKey, &lp)
	})
	return out, err
}

func (s *Service) testBucketIAM(w http.ResponseWriter, r *http.Request, bucket string) {
	if _, err := s.getBucketRec(bucket); err != nil {
		apierr.Write(w, err)
		return
	}
	s.testIAM(w, r, bucketResource(bucket))
}

// testIAM answers testIamPermissions on res.
func (s *Service) testIAM(w http.ResponseWriter, r *http.Request, res string) {
	ctx := r.Context()
	var perms []string
	for _, v := range r.URL.Query()["permissions"] {
		perms = append(perms, strings.Split(v, ",")...)
	}
	out := &storage.TestIamPermissionsResponse{Kind: "storage#testIamPermissionsResponse"}
	eval := s.evaluator()
	var valid []string
	for _, p := range perms {
		if p == "" {
			continue
		}
		if !strings.HasPrefix(p, "storage.") {
			apierr.Write(w, errInvalid("Permission '%s' is not valid for this resource.", p))
			return
		}
		valid = append(valid, p)
	}
	if svc, ok := s.env.Lookup("iam"); ok {
		if pt, ok := svc.(emu.IAMPermissionTester); ok {
			out.Permissions = pt.TestPermissions(ctx, res, valid)
			writeJSON(w, http.StatusOK, out)
			return
		}
	}
	for _, p := range valid {
		allowed := true
		switch {
		case s.env.Auth.Mode() == config.IAMOff:
		case eval != nil:
			allowed = eval.Allowed(ctx, emu.PrincipalFrom(ctx), p, res)
		default:
			allowed = s.check(ctx, p, res) == nil
		}
		if allowed {
			out.Permissions = append(out.Permissions, p)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// evaluator returns the IAM service's policy evaluator when it exposes one.
func (s *Service) evaluator() emu.PolicyEvaluator {
	svc, ok := s.env.Lookup("iam")
	if !ok {
		return nil
	}
	ev, _ := svc.(emu.PolicyEvaluator)
	return ev
}
