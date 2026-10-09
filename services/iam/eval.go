package iam

import (
	"context"
	"strings"
	"sync"

	iamv1 "google.golang.org/api/iam/v1"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/project"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// unknownPerms remembers permissions checked by services but absent from
// the catalogue, so each is logged once.
var unknownPerms sync.Map

// Allowed implements emu.PolicyEvaluator (FR-IAM-004, FR-INT-012). A
// binding grants perm on resource when it sits on the resource itself, on
// an ancestor (by name or recorded parent) or on the owning project, its
// role includes perm, a member matches p and its condition holds.
//
// Implicit grants: the configured default principal is owner of every
// project (and so may do anything), as is the host metadata server's
// default service account unless one was configured explicitly; internal
// calls without a principal are trusted; and each project's default
// compute service account is editor of its project, as in GCP.
func (s *Service) Allowed(ctx context.Context, p emu.Principal, perm, resource string) bool {
	if p == "" || strings.EqualFold(string(p), s.env.Config.DefaultPrincipal) {
		return true
	}
	if email, ok := strings.CutPrefix(string(p), "serviceAccount:"); ok && s.implicitHostAccount(email) {
		return true
	}
	if !s.cat.known[perm] {
		if _, seen := unknownPerms.LoadOrStore(perm, true); !seen {
			s.env.Log.Debug("iam: permission not in catalogue", "permission", perm)
		}
	}
	allowed := false
	_ = s.env.Store.View(func(tx store.Tx) error {
		allowed = s.allowedTx(tx, p, perm, resource)
		return nil
	})
	return allowed
}

func (s *Service) allowedTx(tx store.Tx, p emu.Principal, perm, resource string) bool {
	chain := s.policyChain(tx, resource)
	for _, res := range chain {
		var pol iamv1.Policy
		if store.GetJSON(tx, nsPolicies, res, &pol) != nil {
			continue
		}
		for _, b := range pol.Bindings {
			if !s.roleHas(tx, b.Role, perm) || !s.membersMatch(tx, b.Members, p) {
				continue
			}
			if b.Condition != nil && !evalCondition(b.Condition, resource, s.env.Clock.Now()) {
				continue
			}
			return true
		}
	}
	// The default compute SA is editor on its project.
	if email, ok := strings.CutPrefix(string(p), "serviceAccount:"); ok {
		if m := defaultComputeRE.FindStringSubmatch(email); m != nil && basicIncludes("editor", perm) {
			for _, res := range chain {
				if pid, ok := strings.CutPrefix(res, "//cloudresourcemanager.googleapis.com/projects/"); ok && project.NumberString(pid) == m[1] {
					return true
				}
			}
		}
	}
	return false
}

// policyChain lists the full resource names whose policies apply to
// resource: the resource, its name prefixes, recorded parents, and the
// owning project, nearest first.
func (s *Service) policyChain(tx store.Tx, resource string) []string {
	var out []string
	seen := map[string]bool{}
	var add func(r string, depth int)
	add = func(r string, depth int) {
		if depth > 8 || r == "" {
			return
		}
		host, path := splitResource(r)
		segs := strings.Split(path, "/")
		if host == "cloudresourcemanager.googleapis.com" && len(segs) >= 2 && segs[0] == "projects" {
			segs[1] = s.projectID(tx, segs[1])
		}
		for n := len(segs); n >= 1; n-- {
			res := "//" + host + "/" + strings.Join(segs[:n], "/")
			if seen[res] {
				continue
			}
			seen[res] = true
			out = append(out, res)
			if b, ok := tx.Get(nsParents, res); ok {
				add(string(b), depth+1)
			}
		}
		if len(segs) >= 2 && segs[0] == "projects" && segs[1] != "_" && segs[1] != "-" && segs[1] != "" {
			add(projectResource(s.projectID(tx, segs[1])), depth+1)
		}
		// Service accounts addressed as projects/-/serviceAccounts/EMAIL.
		if host == "iam.googleapis.com" && len(segs) >= 4 && segs[2] == "serviceAccounts" {
			if p := projectOfEmail(segs[3]); p != "" {
				add(projectResource(p), depth+1)
			}
		}
	}
	add(resource, 0)
	return out
}

// splitResource splits "//host/path" into host and path. Relative names
// ("projects/p/topics/t") have an empty host.
func splitResource(r string) (host, path string) {
	if rest, ok := strings.CutPrefix(r, "//"); ok {
		host, path, _ = strings.Cut(rest, "/")
		return host, path
	}
	return "", strings.TrimPrefix(r, "/")
}

// roleHas reports whether a predefined or custom role grants perm.
func (s *Service) roleHas(tx store.Tx, role, perm string) bool {
	if r, ok := s.cat.roles[role]; ok {
		return r.has(perm)
	}
	if strings.HasPrefix(role, "projects/") || strings.HasPrefix(role, "organizations/") {
		var r iamv1.Role
		if store.GetJSON(tx, nsRoles, role, &r) != nil || r.Deleted || r.Stage == "DISABLED" {
			return false
		}
		for _, p := range r.IncludedPermissions {
			if p == perm {
				return true
			}
		}
	}
	return false
}

// membersMatch reports whether any member identifies p.
func (s *Service) membersMatch(tx store.Tx, members []string, p emu.Principal) bool {
	for _, m := range members {
		if s.memberMatches(tx, m, p) {
			return true
		}
	}
	return false
}

func (s *Service) memberMatches(tx store.Tx, m string, p emu.Principal) bool {
	switch {
	case m == "allUsers":
		return true
	case m == "allAuthenticatedUsers":
		return p != ""
	case strings.EqualFold(m, string(p)):
		return true
	}
	kind, val, _ := strings.Cut(m, ":")
	switch kind {
	case "domain":
		return strings.HasPrefix(string(p), "user:") && strings.HasSuffix(strings.ToLower(p.Email()), "@"+strings.ToLower(val))
	case "projectOwner", "projectEditor", "projectViewer":
		role := map[string]string{"projectOwner": "roles/owner", "projectEditor": "roles/editor", "projectViewer": "roles/viewer"}[kind]
		if strings.EqualFold(string(p), s.env.Config.DefaultPrincipal) {
			return true
		}
		pol := loadPolicy(tx, projectResource(s.projectID(tx, val)))
		for _, b := range pol.Bindings {
			if b.Role != role {
				continue
			}
			for _, bm := range b.Members {
				if strings.EqualFold(bm, string(p)) {
					return true
				}
			}
		}
	case "principal", "principalSet":
		if pool, ns, ksa, ok := gkeWorkloadMember(m); ok {
			// Workload Identity Federation for GKE: the member names a
			// Kubernetes service account (or a namespace's) of the
			// PROJECT.svc.id.goog pool, which callers present as
			// serviceAccount:POOL[NS/KSA].
			want := "serviceAccount:" + pool + "[" + ns + "/"
			if ksa == "" {
				return strings.HasPrefix(string(p), want) && strings.HasSuffix(string(p), "]")
			}
			return strings.EqualFold(string(p), want+ksa+"]")
		}
		if kind == "principalSet" {
			return s.principalSetMatches(tx, val, p)
		}
	}
	return false
}

// gkeWorkloadMember parses GKE's Workload Identity members
//
//	principal://iam.googleapis.com/projects/NUM/locations/global/workloadIdentityPools/PROJECT.svc.id.goog/subject/ns/NS/sa/KSA
//	principalSet://iam.googleapis.com/projects/NUM/locations/global/workloadIdentityPools/PROJECT.svc.id.goog/namespace/NS
//
// returning the pool, namespace and KSA (empty for a namespace set). NUM
// must be PROJECT's number.
func gkeWorkloadMember(m string) (pool, ns, ksa string, ok bool) {
	kind, rest, _ := strings.Cut(m, "://iam.googleapis.com/")
	segs := strings.Split(rest, "/")
	if len(segs) < 8 || segs[0] != "projects" || segs[2] != "locations" || segs[3] != "global" || segs[4] != "workloadIdentityPools" {
		return "", "", "", false
	}
	pool = segs[5]
	pid, isGKE := strings.CutSuffix(pool, ".svc.id.goog")
	if !isGKE || project.NumberString(pid) != segs[1] {
		return "", "", "", false
	}
	switch {
	case kind == "principal" && len(segs) == 11 && segs[6] == "subject" && segs[7] == "ns" && segs[9] == "sa":
		return pool, segs[8], segs[10], true
	case kind == "principalSet" && len(segs) == 8 && segs[6] == "namespace":
		return pool, segs[7], "", true
	}
	return "", "", "", false
}
