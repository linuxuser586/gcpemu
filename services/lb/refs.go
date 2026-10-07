package lb

import (
	"context"
	"strings"

	"github.com/linuxuser586/gcpemu/services/compute"
)

// Reference handling: canonicalisation of resource references given as
// short names, partial paths or full URLs, and the resource-in-use graph
// (FR-CORE-026).

// relPath reduces a URL or partial path to its part from "projects/".
func relPath(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "projects/"); i >= 0 {
		return strings.TrimSuffix(s[i:], "/")
	}
	return strings.Trim(s, "/")
}

// lastSeg returns the last path segment.
func lastSeg(s string) string {
	s = strings.TrimSuffix(s, "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// canonRef canonicalises a reference to a resource of collection coll to
// its relative path. Short names resolve in sc; partial and full paths keep
// their own scope. field names the request field for errors.
func canonRef(sc scope, coll, ref, field string) (string, error) {
	p := relPath(ref)
	segs := strings.Split(p, "/")
	switch {
	case len(segs) == 1 && segs[0] != "" && nameRE.MatchString(segs[0]):
		return sc.coll(coll) + "/" + segs[0], nil
	case len(segs) == 3 && segs[0] == "global" && segs[1] == coll:
		return scope{project: sc.project}.coll(coll) + "/" + segs[2], nil
	case len(segs) == 4 && segs[0] == "regions" && segs[2] == coll:
		return scope{project: sc.project, region: segs[1]}.coll(coll) + "/" + segs[3], nil
	case len(segs) == 5 && segs[0] == "projects" && segs[2] == "global" && segs[3] == coll:
		return p, nil
	case len(segs) == 6 && segs[0] == "projects" && segs[2] == "regions" && segs[4] == coll:
		return p, nil
	}
	return "", errInvalid(field, ref, "The URL is malformed.")
}

// collOf returns the collection segment of a relative path.
func collOf(path string) string {
	segs := strings.Split(path, "/")
	if len(segs) >= 2 {
		return segs[len(segs)-2]
	}
	return ""
}

// sameScope reports whether path lives in sc (global vs region R).
func sameScope(sc scope, path string) bool {
	o := scopeOfPath(path)
	return o.region == sc.region
}

// refExisting canonicalises ref to one of colls (in order) in sc and
// checks that the resource exists, returning its selfLink.
func (s *Service) refExisting(sc scope, ref, field string, kinds ...*kind) (string, *kind, error) {
	if ref == "" {
		return "", nil, errRequired(field)
	}
	var lastErr error
	for _, k := range kinds {
		p, err := canonRef(sc, k.coll, ref, field)
		if err != nil {
			lastErr = err
			continue
		}
		if !sameScope(sc, p) {
			return "", nil, errInvalid(field, ref, scopeMismatch(sc))
		}
		if _, ok := s.load(k, p); !ok {
			lastErr = errNotFound(p)
			if strings.Contains(relPath(ref), "/"+k.coll+"/") {
				return "", nil, lastErr
			}
			continue
		}
		return compute.SelfLink(p), k, nil
	}
	return "", nil, lastErr
}

func scopeMismatch(sc scope) string {
	if sc.region == "" {
		return "Global resources can only reference global resources."
	}
	return "Regional resources can only reference resources in the same region."
}

// refsOf lists the resources obj (of kind k) references.
func refsOf(k *kind, obj any) []string {
	if k.refs == nil {
		return nil
	}
	var out []string
	for _, r := range k.refs(obj) {
		if r != "" {
			out = append(out, relPath(r))
		}
	}
	return out
}

// usedBy returns a resource that references path, or "" (FR-CORE-026). It
// is also registered with compute so that NEGs, addresses, networks and
// subnetworks referenced by load-balancing resources cannot be deleted.
func (s *Service) usedBy(ctx context.Context, path string) string {
	path = relPath(path)
	sc := scopeOfPath(path)
	for _, k := range allKinds() {
		if k.refs == nil {
			continue
		}
		for _, obj := range s.loadAll(k, "projects/"+sc.project+"/") {
			for _, r := range refsOf(k, obj) {
				if r == path {
					return relPath(getString(obj, "SelfLink"))
				}
			}
		}
	}
	return ""
}
