package ar

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// OCI Distribution v1.1 registry (FR-AR-002). See the package comment for
// the accepted image name forms.

// Registry permissions checked on the repository.
const (
	permDownload = "artifactregistry.repositories.downloadArtifacts"
	permUpload   = "artifactregistry.repositories.uploadArtifacts"
	permDelete   = "artifactregistry.repositories.deleteArtifacts"
)

// regError is an OCI distribution error.
type regError struct {
	status int
	code   string
	msg    string
	detail any
}

func (e *regError) Error() string { return e.code + ": " + e.msg }

func regErr(status int, code, format string, args ...any) *regError {
	return &regError{status: status, code: code, msg: fmt.Sprintf(format, args...)}
}

func writeRegError(w http.ResponseWriter, e *regError) {
	type item struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Detail  any    `json:"detail,omitempty"`
	}
	b, _ := json.Marshal(map[string][]item{"errors": {{Code: e.code, Message: e.msg, Detail: e.detail}}})
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(e.status)
	_, _ = w.Write(b)
}

func errNameUnknown(name string) *regError {
	return regErr(http.StatusNotFound, "NAME_UNKNOWN", "Repository %q not found", name)
}

// target is a resolved OCI repository: an image path in an AR repository.
type target struct {
	ref  repoRef
	img  string
	name string // the name as it appeared in the URL, for Location headers
}

func (t *target) path(suffix string) string { return "/v2/" + t.name + suffix }

// hostLocation extracts LOCATION from "LOCATION-docker.pkg.dev".
func hostLocation(host string) (string, bool) {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.CutSuffix(strings.ToLower(host), "-docker.pkg.dev")
}

// resolve maps an OCI repository name to an AR repository and image.
func (s *Service) resolve(r *http.Request, name string) (*target, *regError) {
	segs := strings.Split(name, "/")
	loc, hostMode := hostLocation(r.Host)
	if l, ok := hostLocation(r.URL.Query().Get("ns")); ok {
		// containerd mirror request for an Artifact Registry host.
		loc, hostMode = l, true
	}
	if len(segs) > 0 {
		if l, ok := hostLocation(segs[0]); ok {
			loc, hostMode = l, true
			segs = segs[1:]
		}
	}
	if len(segs) < 3 {
		return nil, regErr(http.StatusBadRequest, "NAME_INVALID", "invalid repository name %q: expected [LOCATION-docker.pkg.dev/]PROJECT/REPOSITORY/IMAGE", name)
	}
	p, repo, img := segs[0], segs[1], strings.Join(segs[2:], "/")
	if !validImage(img) {
		return nil, regErr(http.StatusBadRequest, "NAME_INVALID", "invalid repository name %q", name)
	}
	t := &target{img: img, name: name}
	if hostMode {
		if !validLocation(loc) {
			return nil, errNameUnknown(name)
		}
		t.ref = repoRef{p, loc, repo}
		var ok bool
		_ = s.env.Store.View(func(tx store.Tx) error { ok = store.Exists(tx, nsRepos, t.ref.key()); return nil })
		if !ok {
			return nil, errNameUnknown(name)
		}
		return t, nil
	}
	var found []repoRef
	_ = s.env.Store.View(func(tx store.Tx) error {
		tx.Scan(nsRepos, p+"/", func(k string, _ []byte) bool {
			if ref := refFromKey(k); ref.Repo == repo && ref.Project == p {
				found = append(found, ref)
			}
			return true
		})
		return nil
	})
	switch len(found) {
	case 0:
		return nil, errNameUnknown(name)
	case 1:
		t.ref = found[0]
		return t, nil
	}
	locs := make([]string, len(found))
	for i, f := range found {
		locs[i] = f.Location
	}
	sort.Strings(locs)
	return nil, regErr(http.StatusBadRequest, "NAME_INVALID",
		"repository %s/%s exists in multiple locations (%s); use LOCATION-docker.pkg.dev/%s/%s/%s",
		p, repo, strings.Join(locs, ", "), p, repo, img)
}

// serveRegistry routes /v2/ requests.
func (s *Service) serveRegistry(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	p := r.URL.Path
	switch p {
	case "/v2", "/v2/":
		s.ping(w, r)
		return
	case "/v2/token":
		s.token(w, r)
		return
	case "/v2/_catalog":
		s.catalog(w, r)
		return
	}
	rest, ok := strings.CutPrefix(p, "/v2/")
	if !ok {
		writeRegError(w, regErr(http.StatusNotFound, "NOT_FOUND", "not found"))
		return
	}
	var name, kind, arg string
	switch {
	case cut(rest, "/manifests/", &name, &arg):
		kind = "manifests"
	case cut(rest, "/blobs/uploads/", &name, &arg) || strings.HasSuffix(rest, "/blobs/uploads"):
		if strings.HasSuffix(rest, "/blobs/uploads") {
			name, arg = strings.TrimSuffix(rest, "/blobs/uploads"), ""
		}
		kind = "uploads"
	case cut(rest, "/blobs/", &name, &arg):
		kind = "blobs"
	case strings.HasSuffix(rest, "/tags/list"):
		name, kind = strings.TrimSuffix(rest, "/tags/list"), "tags"
	case cut(rest, "/referrers/", &name, &arg):
		kind = "referrers"
	default:
		writeRegError(w, regErr(http.StatusNotFound, "NOT_FOUND", "not found"))
		return
	}
	if pt, ok, rerr := s.mirrorTarget(r, name); ok {
		if rerr != nil {
			writeRegError(w, rerr)
			return
		}
		s.mirrorRoute(w, r, pt, kind, arg)
		return
	}
	t, rerr := s.resolve(r, name)
	if rerr != nil {
		if !s.authenticated(r) && s.enforcing() {
			s.challenge(w, r, name, "pull")
			return
		}
		writeRegError(w, rerr)
		return
	}
	switch repo := s.targetRepo(t); repo.GetMode() {
	case artifactregistrypb.Repository_REMOTE_REPOSITORY:
		s.remoteRoute(w, r, t, repo, kind, arg)
	case artifactregistrypb.Repository_VIRTUAL_REPOSITORY:
		s.virtualRoute(w, r, t, repo, kind, arg)
	default:
		s.standardRoute(w, r, t, kind, arg)
	}
}

// standardRoute serves a request to a standard repository (and the
// cache-management requests of a remote one).
func (s *Service) standardRoute(w http.ResponseWriter, r *http.Request, t *target, kind, arg string) {
	switch kind {
	case "manifests":
		s.manifests(w, r, t, arg)
	case "uploads":
		s.uploadsRoute(w, r, t, arg)
	case "blobs":
		s.blobsRoute(w, r, t, arg)
	case "tags":
		s.tagsList(w, r, t)
	case "referrers":
		s.referrers(w, r, t, arg)
	}
}

// cut splits s at the last occurrence of sep.
func cut(s, sep string, before, after *string) bool {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return false
	}
	*before, *after = s[:i], s[i+len(sep):]
	return true
}

func (s *Service) enforcing() bool { return s.env.Auth.Mode() == config.IAMEnforce }

// authenticated reports whether the request carried credentials.
func (s *Service) authenticated(r *http.Request) bool { return r.Header.Get("Authorization") != "" }

// authorize checks perm on the target repository, writing a challenge or
// DENIED and returning false when the caller may not proceed.
func (s *Service) authorize(w http.ResponseWriter, r *http.Request, t *target, perm string) bool {
	action := "pull"
	if perm != permDownload {
		action = "push,pull"
	}
	if s.enforcing() && !s.authenticated(r) {
		s.challenge(w, r, t.name, action)
		return false
	}
	if err := s.env.Auth.Check(r.Context(), perm, t.ref.resource()); err != nil {
		if !s.authenticated(r) {
			s.challenge(w, r, t.name, action)
			return false
		}
		writeRegError(w, regErr(http.StatusForbidden, "DENIED", "Permission %q denied on resource %q (or it may not exist).", perm, t.ref.name()))
		return false
	}
	return true
}

// challenge writes 401 with a Bearer challenge pointing at /v2/token.
func (s *Service) challenge(w http.ResponseWriter, r *http.Request, name, action string) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	c := fmt.Sprintf(`Bearer realm="%s://%s/v2/token",service="%s"`, scheme, realmHost(r.Host), r.Host)
	if name != "" {
		c += fmt.Sprintf(`,scope="repository:%s:%s"`, name, action)
	}
	w.Header().Set("WWW-Authenticate", c)
	writeRegError(w, regErr(http.StatusUnauthorized, "UNAUTHORIZED", "authentication required"))
}

// realmHost returns the host for the token realm. Loopback IP literals are
// replaced by "localhost": go-containerregistry (crane, ko) refuses token
// realms on loopback or private IP literals as an SSRF guard.
func realmHost(host string) string {
	h, port, err := net.SplitHostPort(host)
	if err != nil {
		h, port = host, ""
	}
	if ip := net.ParseIP(strings.Trim(h, "[]")); ip != nil && ip.IsLoopback() {
		if port == "" {
			return "localhost"
		}
		return net.JoinHostPort("localhost", port)
	}
	return host
}

// ping implements GET /v2/: unauthenticated clients are challenged, like
// the real registry, so docker and crane fetch a token first.
func (s *Service) ping(w http.ResponseWriter, r *http.Request) {
	if !s.authenticated(r) {
		s.challenge(w, r, "", "")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{}"))
}

// catalog implements GET /v2/_catalog with n/last pagination.
func (s *Service) catalog(w http.ResponseWriter, r *http.Request) {
	if s.enforcing() && !s.authenticated(r) {
		s.challenge(w, r, "", "")
		return
	}
	loc, hostMode := hostLocation(r.Host)
	var names []string
	_ = s.env.Store.View(func(tx store.Tx) error {
		refs, _, _ := listRepos(tx, "")
		for _, ref := range refs {
			if hostMode && ref.Location != loc {
				continue
			}
			for _, e := range listImages(tx, ref) {
				names = append(names, ref.Project+"/"+ref.Repo+"/"+e.Image)
			}
		}
		return nil
	})
	sort.Strings(names)
	names = dedupe(names)
	pg, next, rerr := paginate(names, r)
	if rerr != nil {
		writeRegError(w, rerr)
		return
	}
	if next != "" {
		w.Header().Set("Link", fmt.Sprintf(`</v2/_catalog?n=%d&last=%s>; rel="next"`, len(pg), next))
	}
	writeJSON(w, http.StatusOK, map[string]any{"repositories": nonNil(pg)})
}

func dedupe(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// paginate applies the n and last query parameters to a sorted list and
// returns the page and the value of last for the next page ("" if none).
func paginate(items []string, r *http.Request) ([]string, string, *regError) {
	q := r.URL.Query()
	if last := q.Get("last"); last != "" {
		i := sort.SearchStrings(items, last)
		if i < len(items) && items[i] == last {
			i++
		}
		items = items[i:]
	}
	if ns := q.Get("n"); ns != "" {
		n, err := strconv.Atoi(ns)
		if err != nil || n < 0 {
			return nil, "", regErr(http.StatusBadRequest, "PAGINATION_NUMBER_INVALID", "invalid number of results requested")
		}
		if n < len(items) {
			if n == 0 {
				return []string{}, "", nil
			}
			return items[:n], items[n-1], nil
		}
	}
	return items, "", nil
}

// tagsList implements GET /v2/<name>/tags/list.
func (s *Service) tagsList(w http.ResponseWriter, r *http.Request, t *target) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeRegError(w, regErr(http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed"))
		return
	}
	if !s.authorize(w, r, t, permDownload) {
		return
	}
	var tags []string
	var known bool
	_ = s.env.Store.View(func(tx store.Tx) error {
		pre := imgPrefix(t.ref, t.img)
		known = store.HasPrefix(tx, nsManifests, pre)
		tx.Scan(nsTags, pre, func(k string, _ []byte) bool {
			tags = append(tags, strings.TrimPrefix(k, pre))
			return true
		})
		return nil
	})
	if !known {
		writeRegError(w, errNameUnknown(t.name))
		return
	}
	sort.Strings(tags)
	pg, next, rerr := paginate(tags, r)
	if rerr != nil {
		writeRegError(w, rerr)
		return
	}
	if next != "" {
		w.Header().Set("Link", fmt.Sprintf(`<%s?n=%d&last=%s>; rel="next"`, t.path("/tags/list"), len(pg), next))
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": t.name, "tags": nonNil(pg)})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}
