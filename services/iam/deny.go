package iam

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"cel.dev/cel-go/common/types"
	iampb "cloud.google.com/go/iam/apiv2/iampb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/project"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// IAM v2 deny policies (FR-IAM-010). A deny policy is attached to a
// project, folder or organization (its attachment point) and refuses the
// permissions its rules name to the principals they name, whatever allow
// policies grant, on the attachment point and everything under it.
// Policies are kept as protojson google.iam.v2.Policy under
// "ATTACHMENT/denypolicies/ID", where ATTACHMENT names a project by ID;
// the policy's name names it by number, as GCP does.

const (
	crmHost  = "cloudresourcemanager.googleapis.com"
	denyKind = "denypolicies"
	// maxDenyRules is GCP's limit on rules per deny policy.
	maxDenyRules = 500
)

var (
	denyPolicyIDRE   = regexp.MustCompile(`^[a-z][a-z0-9.-]{1,61}[a-z0-9]$`)
	denyPermissionRE = regexp.MustCompile(`^[a-z0-9*][a-z0-9.*-]*/[A-Za-z0-9*]+\.[A-Za-z0-9*]+$`)
	// tagFunctionRE finds the resource tag functions, which deny policy
	// conditions are written with in GCP; the emulator does not model tags.
	tagFunctionRE = regexp.MustCompile(`\b(matchTag|matchTagId|hasTagKey|hasTagKeyId)\s*\(`)
)

// denyAttachment is a resolved attachment point.
type denyAttachment struct {
	key      string // cloudresourcemanager.googleapis.com/projects/ID
	display  string // as names show it: cloudresourcemanager.googleapis.com/projects/NUMBER
	resource string // //cloudresourcemanager.googleapis.com/projects/ID
}

func (a denyAttachment) parent() string {
	return "policies/" + url.PathEscape(a.display) + "/" + denyKind
}

func (a denyAttachment) policyName(id string) string { return a.parent() + "/" + id }

func (a denyAttachment) storeKey(id string) string { return a.key + "/" + denyKind + "/" + id }

// attachment resolves an attachment point such as
// "cloudresourcemanager.googleapis.com/projects/my-project", a project
// given by ID or number, which is created implicitly.
func (s *Service) attachment(raw string) (denyAttachment, error) {
	rest, ok := strings.CutPrefix(raw, crmHost+"/")
	kind, id, _ := strings.Cut(rest, "/")
	if !ok || id == "" || strings.Contains(id, "/") || (kind != "projects" && kind != "folders" && kind != "organizations") {
		return denyAttachment{}, apierr.InvalidArgument("Invalid attachment point %q. It must be %[2]s/projects/PROJECT, %[2]s/folders/FOLDER or %[2]s/organizations/ORGANIZATION, URL-encoded.", raw, crmHost)
	}
	if kind != "projects" {
		k := crmHost + "/" + kind + "/" + id
		return denyAttachment{key: k, display: k, resource: "//" + k}, nil
	}
	_ = s.env.Store.View(func(tx store.Tx) error { id = s.projectID(tx, id); return nil })
	if id[0] >= '0' && id[0] <= '9' {
		return denyAttachment{}, apierr.NotFound("Project %s not found.", id).WithReason("googleapis.com", "PROJECT_NOT_FOUND")
	}
	if err := s.env.EnsureProject(id); err != nil {
		return denyAttachment{}, err
	}
	k := crmHost + "/projects/" + id
	return denyAttachment{key: k, display: crmHost + "/projects/" + project.NumberString(id), resource: "//" + k}, nil
}

// parseDenyPath splits "policies/ATTACHMENT/denypolicies[/...]", an
// escaped path whose attachment point may be URL-encoded or not, into the
// attachment point and the unescaped segments after the kind.
func parseDenyPath(p string) (attach string, rest []string, err error) {
	segs := strings.Split(strings.Trim(p, "/"), "/")
	k := slices.Index(segs, denyKind)
	if segs[0] != "policies" || k < 2 {
		return "", nil, apierr.InvalidArgument("Invalid policy name %q. It must be policies/ATTACHMENT_POINT/denypolicies/POLICY_ID; denypolicies is the only kind.", p)
	}
	if attach, err = url.PathUnescape(strings.Join(segs[1:k], "/")); err != nil {
		return "", nil, apierr.InvalidArgument("Invalid policy name %q: %v", p, err)
	}
	// Clients that escape an already-encoded name send it encoded twice.
	if u, err := url.PathUnescape(attach); err == nil {
		attach = u
	}
	for _, seg := range segs[k+1:] {
		u, err := url.PathUnescape(seg)
		if err != nil {
			return "", nil, apierr.InvalidArgument("Invalid policy name %q: %v", p, err)
		}
		rest = append(rest, u)
	}
	return attach, rest, nil
}

// denyParent resolves "policies/ATTACHMENT/denypolicies".
func (s *Service) denyParent(parent string) (denyAttachment, error) {
	attach, rest, err := parseDenyPath(parent)
	if err == nil && len(rest) != 0 {
		err = apierr.InvalidArgument("Invalid parent %q. It must be policies/ATTACHMENT_POINT/denypolicies.", parent)
	}
	if err != nil {
		return denyAttachment{}, err
	}
	return s.attachment(attach)
}

// denyPolicyRef resolves "policies/ATTACHMENT/denypolicies/ID".
func (s *Service) denyPolicyRef(name string) (denyAttachment, string, error) {
	attach, rest, err := parseDenyPath(name)
	if err == nil && (len(rest) != 1 || rest[0] == "") {
		err = apierr.InvalidArgument("Invalid policy name %q. It must be policies/ATTACHMENT_POINT/denypolicies/POLICY_ID.", name)
	}
	if err != nil {
		return denyAttachment{}, "", err
	}
	att, err := s.attachment(attach)
	return att, rest[0], err
}

// validateDenyRules checks a deny policy's rules as GCP does, except that
// conditions on resource tags are refused, since tags are not modeled.
func validateDenyRules(rules []*iampb.PolicyRule) error {
	if len(rules) > maxDenyRules {
		return apierr.InvalidArgument("A deny policy can have at most %d rules; it has %d.", maxDenyRules, len(rules))
	}
	for i, r := range rules {
		d := r.GetDenyRule()
		if d == nil {
			return apierr.InvalidArgument("Rule %d has no denyRule.", i)
		}
		if len(d.DeniedPrincipals) == 0 {
			return apierr.InvalidArgument("Rule %d must name at least one denied principal.", i)
		}
		if len(d.DeniedPermissions) == 0 {
			return apierr.InvalidArgument("Rule %d must name at least one denied permission.", i)
		}
		for _, p := range slices.Concat(d.DeniedPrincipals, d.ExceptionPrincipals) {
			if !strings.HasPrefix(strings.TrimPrefix(p, "deleted:"), "principal://") && !strings.HasPrefix(p, "principalSet://") {
				return apierr.InvalidArgument("Rule %d: invalid principal %q. Deny policies name principals as principal://... or principalSet://..., e.g. principal://goog/subject/USER_EMAIL or principalSet://goog/public:all.", i, p)
			}
		}
		for _, p := range slices.Concat(d.DeniedPermissions, d.ExceptionPermissions) {
			if !denyPermissionRE.MatchString(p) {
				return apierr.InvalidArgument("Rule %d: invalid permission %q. Deny policies name permissions as SERVICE_FQDN/RESOURCE.ACTION, e.g. secretmanager.googleapis.com/versions.destroy.", i, p)
			}
		}
		if c := d.DenialCondition; c != nil {
			if err := validateDenialCondition(i, c); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateDenialCondition(i int, c *expr.Expr) error {
	if strings.TrimSpace(c.Expression) == "" {
		return apierr.InvalidArgument("Rule %d: the denial condition's expression must not be empty.", i)
	}
	if tagFunctionRE.MatchString(c.Expression) {
		return apierr.InvalidArgument("Rule %d: denial condition %q uses resource tags, which gcpemu does not model; it supports conditions on request.time, resource.name, resource.type and resource.service only.", i, c.Expression).WithReason(iamDomain, "INVALID_CONDITION")
	}
	if _, err := compileCondition(c.Expression); err != nil {
		return apierr.InvalidArgument("Rule %d: denial condition %q is invalid: %v", i, c.Expression, err).WithReason(iamDomain, "INVALID_CONDITION")
	}
	return nil
}

// --- storage ---

func getDenyPolicy(tx store.Tx, key string) (*iampb.Policy, bool) {
	b, ok := tx.Get(nsDenyPolicies, key)
	if !ok {
		return nil, false
	}
	pol := &iampb.Policy{}
	return pol, protojson.Unmarshal(b, pol) == nil
}

func putDenyPolicy(tx store.Tx, key string, pol *iampb.Policy) error {
	b, err := protojson.Marshal(pol)
	if err != nil {
		return err
	}
	return tx.Put(nsDenyPolicies, key, b)
}

func denyPolicyNotFound(name string) error {
	return apierr.NotFound("Policy %s not found.", name)
}

// checkDenyEtag fails when etag is set and is not pol's.
func checkDenyEtag(pol *iampb.Policy, etag string) error {
	if etag != "" && etag != pol.Etag {
		return apierr.Aborted("The etag %q does not match the current etag of policy %s.", etag, pol.Name)
	}
	return nil
}

func (s *Service) newUUID() string {
	h := s.env.IDs.Hex(16)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// --- methods (gRPC and REST share them) ---

func (s *Service) createDenyPolicy(ctx context.Context, parent, id string, in *iampb.Policy) (*longrunningpb.Operation, error) {
	att, err := s.denyParent(parent)
	if err != nil {
		return nil, err
	}
	if err := s.check(ctx, "iam.denypolicies.create", att.resource); err != nil {
		return nil, err
	}
	if !denyPolicyIDRE.MatchString(id) {
		return nil, apierr.InvalidArgument("Invalid policy ID %q. It must be 3 to 63 lowercase letters, digits, hyphens and periods, start with a letter and not end with a hyphen or period.", id)
	}
	if in == nil {
		in = &iampb.Policy{}
	}
	if err := validateDenyRules(in.Rules); err != nil {
		return nil, err
	}
	key, name := att.storeKey(id), att.policyName(id)
	exists := func(tx store.Tx) error {
		if _, ok := getDenyPolicy(tx, key); ok {
			return apierr.AlreadyExists("Policy %s already exists.", name)
		}
		return nil
	}
	if err := s.env.Store.View(exists); err != nil {
		return nil, err
	}
	now := timestamppb.New(s.env.Clock.Now())
	pol := &iampb.Policy{
		Name: name, Uid: s.newUUID(), Kind: "DenyPolicy",
		DisplayName: in.DisplayName, Annotations: in.Annotations, Rules: in.Rules,
		Etag: s.newEtag(), CreateTime: now, UpdateTime: now,
	}
	return s.ops.Run(ctx, name, &iampb.PolicyOperationMetadata{CreateTime: now}, func(context.Context) (proto.Message, error) {
		return pol, s.env.Store.Update(func(tx store.Tx) error {
			if err := exists(tx); err != nil {
				return err
			}
			return putDenyPolicy(tx, key, pol)
		})
	})
}

func (s *Service) getDenyPolicy(ctx context.Context, name string) (*iampb.Policy, error) {
	att, id, err := s.denyPolicyRef(name)
	if err != nil {
		return nil, err
	}
	if err := s.check(ctx, "iam.denypolicies.get", att.resource); err != nil {
		return nil, err
	}
	var pol *iampb.Policy
	var ok bool
	_ = s.env.Store.View(func(tx store.Tx) error { pol, ok = getDenyPolicy(tx, att.storeKey(id)); return nil })
	if !ok {
		return nil, denyPolicyNotFound(att.policyName(id))
	}
	return pol, nil
}

// listDenyPolicies lists an attachment point's deny policies without
// their rules, as GCP does. The page token is a decimal offset.
func (s *Service) listDenyPolicies(ctx context.Context, parent string, size int, token string) (*iampb.ListPoliciesResponse, error) {
	att, err := s.denyParent(parent)
	if err != nil {
		return nil, err
	}
	if err := s.check(ctx, "iam.denypolicies.list", att.resource); err != nil {
		return nil, err
	}
	offset := 0
	if token != "" {
		if offset, err = strconv.Atoi(token); err != nil || offset < 0 {
			return nil, apierr.InvalidArgument("Invalid pageToken %q.", token)
		}
	}
	if size <= 0 || size > 1000 {
		size = 1000
	}
	var all []*iampb.Policy
	_ = s.env.Store.View(func(tx store.Tx) error {
		tx.Scan(nsDenyPolicies, att.key+"/"+denyKind+"/", func(_ string, b []byte) bool {
			pol := &iampb.Policy{}
			if protojson.Unmarshal(b, pol) == nil {
				pol.Rules = nil
				all = append(all, pol)
			}
			return true
		})
		return nil
	})
	pols, next := page(all, size, offset)
	return &iampb.ListPoliciesResponse{Policies: pols, NextPageToken: next}, nil
}

// updateDenyPolicy replaces a deny policy's display name, annotations and
// rules; a non-empty etag must be the current one.
func (s *Service) updateDenyPolicy(ctx context.Context, in *iampb.Policy) (*longrunningpb.Operation, error) {
	if in == nil {
		return nil, apierr.InvalidArgument("A policy is required.")
	}
	att, id, err := s.denyPolicyRef(in.Name)
	if err != nil {
		return nil, err
	}
	if err := s.check(ctx, "iam.denypolicies.update", att.resource); err != nil {
		return nil, err
	}
	if err := validateDenyRules(in.Rules); err != nil {
		return nil, err
	}
	key := att.storeKey(id)
	var cur *iampb.Policy
	var ok bool
	_ = s.env.Store.View(func(tx store.Tx) error { cur, ok = getDenyPolicy(tx, key); return nil })
	if !ok {
		return nil, denyPolicyNotFound(att.policyName(id))
	}
	if err := checkDenyEtag(cur, in.Etag); err != nil {
		return nil, err
	}
	now := timestamppb.New(s.env.Clock.Now())
	pol := proto.Clone(cur).(*iampb.Policy)
	pol.DisplayName, pol.Annotations, pol.Rules = in.DisplayName, in.Annotations, in.Rules
	pol.Etag, pol.UpdateTime = s.newEtag(), now
	return s.ops.Run(ctx, pol.Name, &iampb.PolicyOperationMetadata{CreateTime: now}, func(context.Context) (proto.Message, error) {
		return pol, s.env.Store.Update(func(tx store.Tx) error {
			if _, ok := getDenyPolicy(tx, key); !ok {
				return denyPolicyNotFound(pol.Name)
			}
			return putDenyPolicy(tx, key, pol)
		})
	})
}

// deleteDenyPolicy deletes a deny policy; a non-empty etag must be the
// current one. The operation's response is the policy as deleted.
func (s *Service) deleteDenyPolicy(ctx context.Context, name, etag string) (*longrunningpb.Operation, error) {
	att, id, err := s.denyPolicyRef(name)
	if err != nil {
		return nil, err
	}
	if err := s.check(ctx, "iam.denypolicies.delete", att.resource); err != nil {
		return nil, err
	}
	key := att.storeKey(id)
	var cur *iampb.Policy
	var ok bool
	_ = s.env.Store.View(func(tx store.Tx) error { cur, ok = getDenyPolicy(tx, key); return nil })
	if !ok {
		return nil, denyPolicyNotFound(att.policyName(id))
	}
	if err := checkDenyEtag(cur, etag); err != nil {
		return nil, err
	}
	now := timestamppb.New(s.env.Clock.Now())
	cur.DeleteTime = now
	return s.ops.Run(ctx, cur.Name, &iampb.PolicyOperationMetadata{CreateTime: now}, func(context.Context) (proto.Message, error) {
		return cur, s.env.Store.Update(func(tx store.Tx) error {
			if _, ok := getDenyPolicy(tx, key); !ok {
				return denyPolicyNotFound(cur.Name)
			}
			return tx.Delete(nsDenyPolicies, key)
		})
	})
}

// --- evaluation ---

// Denied implements emu.DenyEvaluator: the first deny policy, on an
// attachment point above resource, with a rule that names perm and a
// principal p is, and names neither as an exception, and whose condition
// holds. Internal calls without a principal are never denied.
func (s *Service) Denied(ctx context.Context, p emu.Principal, perm, resource string) (string, bool) {
	if p == "" {
		return "", false
	}
	v2 := v2Permission(perm)
	now := s.env.Clock.Now()
	var name string
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, res := range s.policyChain(tx, resource) {
			key, ok := denyAttachmentKey(res)
			if !ok {
				continue
			}
			tx.Scan(nsDenyPolicies, key+"/"+denyKind+"/", func(_ string, b []byte) bool {
				pol := &iampb.Policy{}
				if protojson.Unmarshal(b, pol) == nil && s.denies(tx, pol, p, v2, resource, now) {
					name = pol.Name
				}
				return name == ""
			})
			if name != "" {
				return nil
			}
		}
		return nil
	})
	return name, name != ""
}

// denyAttachmentKey returns the attachment point a full resource name of
// a project, folder or organization stands for.
func denyAttachmentKey(res string) (string, bool) {
	rest, ok := strings.CutPrefix(res, "//"+crmHost+"/")
	kind, id, _ := strings.Cut(rest, "/")
	ok = ok && id != "" && !strings.Contains(id, "/") && (kind == "projects" || kind == "folders" || kind == "organizations")
	return strings.TrimPrefix(res, "//"), ok
}

// denies reports whether a rule of pol refuses perm (in v2 form) to p on
// resource.
func (s *Service) denies(tx store.Tx, pol *iampb.Policy, p emu.Principal, perm, resource string, now time.Time) bool {
	permIn := func(pats []string) bool {
		return slices.ContainsFunc(pats, func(pat string) bool { return denyPermissionMatches(pat, perm) })
	}
	principalIn := func(ms []string) bool {
		return slices.ContainsFunc(ms, func(m string) bool { return s.denyPrincipalMatches(tx, m, p) })
	}
	for _, r := range pol.Rules {
		d := r.GetDenyRule()
		if d == nil || !permIn(d.DeniedPermissions) || permIn(d.ExceptionPermissions) ||
			!principalIn(d.DeniedPrincipals) || principalIn(d.ExceptionPrincipals) {
			continue
		}
		if c := d.DenialCondition; c != nil && !denialConditionHolds(c.Expression, resource, now) {
			continue
		}
		return true
	}
	return false
}

// v2Permission writes a permission as deny policies name it:
// secretmanager.versions.destroy is
// secretmanager.googleapis.com/versions.destroy.
func v2Permission(perm string) string {
	svc, rest, _ := strings.Cut(perm, ".")
	if svc == "resourcemanager" {
		svc = "cloudresourcemanager"
	}
	return svc + ".googleapis.com/" + rest
}

// denyPermPatterns caches compiled permission groups by pattern.
var denyPermPatterns sync.Map

// denyPermissionMatches matches a v2 permission against a permission or a
// permission group, in which "*" stands for any part between dots, e.g.
// secretmanager.googleapis.com/versions.* or storage.googleapis.com/*.delete.
func denyPermissionMatches(pattern, perm string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == perm
	}
	re, ok := denyPermPatterns.Load(pattern)
	if !ok {
		re = regexp.MustCompile("^" + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, `[^./]*`) + "$")
		denyPermPatterns.Store(pattern, re)
	}
	return re.(*regexp.Regexp).MatchString(perm)
}

// denyPrincipalMatches reports whether a deny policy's principal
// identifier names p. Principals deleted since the policy was written
// match no one.
func (s *Service) denyPrincipalMatches(tx store.Tx, m string, p emu.Principal) bool {
	if m == "principalSet://goog/public:all" {
		return true
	}
	if email, ok := strings.CutPrefix(m, "principal://goog/subject/"); ok {
		return strings.EqualFold("user:"+email, string(p))
	}
	if email, ok := strings.CutPrefix(m, "principal://iam.googleapis.com/projects/-/serviceAccounts/"); ok {
		return strings.EqualFold("serviceAccount:"+email, string(p))
	}
	if group, ok := strings.CutPrefix(m, "principalSet://goog/group/"); ok {
		return s.memberMatches(tx, "group:"+group, p)
	}
	if rest, ok := strings.CutPrefix(m, "principalSet://"+crmHost+"/projects/"); ok {
		// Every service account of a project.
		pid, typ, _ := strings.Cut(rest, "/")
		email, isSA := strings.CutPrefix(string(p), "serviceAccount:")
		if typ != "type/ServiceAccount" || !isSA {
			return false
		}
		pid = s.projectID(tx, pid)
		if c := defaultComputeRE.FindStringSubmatch(email); c != nil {
			return project.NumberString(pid) == c[1]
		}
		return projectOfEmail(email) == pid
	}
	if strings.HasPrefix(m, "deleted:") {
		return false
	}
	// Workload Identity Federation principals read as in allow policies.
	return s.memberMatches(tx, m, p)
}

// denialConditionHolds evaluates a denial condition. One that cannot be
// evaluated denies, so that a deny policy fails closed.
func denialConditionHolds(expression, resource string, now time.Time) bool {
	p, err := compileCondition(expression)
	if err != nil {
		return true
	}
	v, _, err := p.Eval(conditionVars(resource, now))
	return err != nil || v == types.True
}

// --- REST ---

// serveIAMv2 serves iam.googleapis.com/v2 deny policies and their
// operations (FR-IAM-010). Names carry the URL-encoded attachment point,
// so routing reads the escaped path.
func (s *Service) serveIAMv2(w http.ResponseWriter, r *http.Request) {
	path, _ := strings.CutPrefix(r.URL.EscapedPath(), "/v2/")
	if !strings.HasPrefix(path, "policies/") {
		notFoundRoute(w, r)
		return
	}
	if err := s.routeDenyPolicies(w, r, path); err != nil {
		apierr.Write(w, err)
	}
}

func (s *Service) routeDenyPolicies(w http.ResponseWriter, r *http.Request, path string) error {
	attach, rest, err := parseDenyPath(path)
	if err != nil {
		return err
	}
	ctx, q := r.Context(), r.URL.Query()
	parent := "policies/" + url.PathEscape(attach) + "/" + denyKind
	if len(rest) >= 2 && rest[1] == "operations" {
		segs := make([]string, len(rest))
		for i, seg := range rest {
			segs[i] = url.PathEscape(seg)
		}
		if !s.ops.ServeREST(w, r, parent+"/"+strings.Join(segs, "/")) {
			notFoundRoute(w, r)
		}
		return nil
	}
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		size, _ := strconv.Atoi(q.Get("pageSize"))
		resp, err := s.listDenyPolicies(ctx, parent, size, q.Get("pageToken"))
		return writeProtoJSON(w, resp, err)
	case len(rest) == 0 && r.Method == http.MethodPost:
		pol := &iampb.Policy{}
		if err := readProtoJSON(r, pol); err != nil {
			return err
		}
		op, err := s.createDenyPolicy(ctx, parent, q.Get("policyId"), pol)
		return writeProtoJSON(w, op, err)
	case len(rest) == 1 && r.Method == http.MethodGet:
		pol, err := s.getDenyPolicy(ctx, parent+"/"+rest[0])
		return writeProtoJSON(w, pol, err)
	case len(rest) == 1 && r.Method == http.MethodPut:
		pol := &iampb.Policy{}
		if err := readProtoJSON(r, pol); err != nil {
			return err
		}
		pol.Name = parent + "/" + rest[0]
		op, err := s.updateDenyPolicy(ctx, pol)
		return writeProtoJSON(w, op, err)
	case len(rest) == 1 && r.Method == http.MethodDelete:
		op, err := s.deleteDenyPolicy(ctx, parent+"/"+rest[0], q.Get("etag"))
		return writeProtoJSON(w, op, err)
	}
	notFoundRoute(w, r)
	return nil
}

// readProtoJSON decodes a JSON request body into msg; an empty body is
// allowed.
func readProtoJSON(r *http.Request, msg proto.Message) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return apierr.InvalidArgument("read body: %v", err)
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, msg); err != nil {
		return apierr.InvalidArgument("Invalid JSON payload received. %v", err)
	}
	return nil
}

// writeProtoJSON writes msg as a 200 JSON response, or returns err.
func writeProtoJSON(w http.ResponseWriter, msg proto.Message, err error) error {
	if err != nil {
		return err
	}
	b, err := protojson.Marshal(msg)
	if err != nil {
		return apierr.Internal("encode response: %v", err)
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_, _ = w.Write(b)
	return nil
}

// --- gRPC ---

// policiesServer serves google.iam.v2.Policies.
type policiesServer struct {
	iampb.UnimplementedPoliciesServer
	s *Service
}

func (g *policiesServer) ListPolicies(ctx context.Context, req *iampb.ListPoliciesRequest) (*iampb.ListPoliciesResponse, error) {
	return g.s.listDenyPolicies(ctx, req.GetParent(), int(req.GetPageSize()), req.GetPageToken())
}

func (g *policiesServer) GetPolicy(ctx context.Context, req *iampb.GetPolicyRequest) (*iampb.Policy, error) {
	return g.s.getDenyPolicy(ctx, req.GetName())
}

func (g *policiesServer) CreatePolicy(ctx context.Context, req *iampb.CreatePolicyRequest) (*longrunningpb.Operation, error) {
	return g.s.createDenyPolicy(ctx, req.GetParent(), req.GetPolicyId(), req.GetPolicy())
}

func (g *policiesServer) UpdatePolicy(ctx context.Context, req *iampb.UpdatePolicyRequest) (*longrunningpb.Operation, error) {
	return g.s.updateDenyPolicy(ctx, req.GetPolicy())
}

func (g *policiesServer) DeletePolicy(ctx context.Context, req *iampb.DeletePolicyRequest) (*longrunningpb.Operation, error) {
	return g.s.deleteDenyPolicy(ctx, req.GetName(), req.GetEtag())
}
