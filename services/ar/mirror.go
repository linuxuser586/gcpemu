package ar

import (
	"context"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// Registry mirror for public registries (FR-GKE-006, FR-AR-005).
//
// containerd (and so k3s/GKE nodes) configured with this registry as a
// mirror sends ordinary distribution requests with the original registry
// in the "ns" query parameter:
//
//	GET /v2/library/busybox/manifests/latest?ns=docker.io
//	GET /v2/pause/blobs/sha256:...?ns=registry.k8s.io
//
// When ns names an Artifact Registry host (LOCATION-docker.pkg.dev) the
// request is served from the emulated repositories, with the location
// taken from ns. Any other registry host is pulled through the cache
// (pullthrough.go). Clients without mirror support (crane, ko, docker
// with an explicit host) can use the path form instead:
//
//	localhost:5000/docker.io/library/busybox:latest
//
// The first path segment then names the upstream registry; it is
// recognised by containing a '.' or ':' (or being "localhost"), which no
// project ID does. Mirror pulls are anonymous and need no credentials,
// also in IAM enforce mode: they only expose public content, and node
// runtimes must be able to pull system images before any credentials
// exist. Mirror access is read-only.

// DefaultMirrorHosts are the public registries GKE nodes reach through the
// emulator, as real nodes do through Private Google Access.
var DefaultMirrorHosts = []string{
	"docker.io",
	"registry.k8s.io",
	"ghcr.io",
	"quay.io",
	"gcr.io",
	"mirror.gcr.io",
}

// MirrorHosts returns the upstream registry hosts that nodes should
// configure as mirrored through the registry (see RegistriesYAML).
// Any other host also works through the "*" mirror entry.
func (s *Service) MirrorHosts() []string {
	return append([]string(nil), DefaultMirrorHosts...)
}

// registryHostRe matches a registry host[:port] as it appears in ns or
// the first path segment.
var registryHostRe = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)*|\[[0-9a-f:.]+\])(?::[0-9]{1,5})?$`)

// mirrorTarget recognises mirror requests (ns parameter or path form) and
// returns their cache target; ok is false for ordinary registry requests.
func (s *Service) mirrorTarget(r *http.Request, name string) (pt *pullThrough, ok bool, rerr *regError) {
	host, repo := "", name
	if ns := strings.ToLower(r.URL.Query().Get("ns")); ns != "" && s.isUpstreamHost(r, ns) {
		host = ns
	} else if seg, rest, found := strings.Cut(name, "/"); found && (strings.ContainsAny(seg, ".:") || seg == "localhost") {
		if _, ar := hostLocation(seg); ar {
			return nil, false, nil
		}
		host, repo = seg, rest
	}
	if host == "" {
		return nil, false, nil
	}
	if !registryHostRe.MatchString(host) {
		return nil, true, regErr(http.StatusBadRequest, "NAME_INVALID", "invalid registry host %q", host)
	}
	host = canonicalHost(host)
	repo = canonicalRepo(host, repo)
	if !validImage(repo) {
		return nil, true, regErr(http.StatusBadRequest, "NAME_INVALID", "invalid repository name %q", name)
	}
	return &pullThrough{
		scope: repoRef{mirrorProject, host, "cache"},
		img:   repo,
		up:    upstreamRef{Host: host, Base: s.upstreamBase(host), Repo: repo},
	}, true, nil
}

// isUpstreamHost reports whether ns names a registry other than this one.
func (s *Service) isUpstreamHost(r *http.Request, ns string) bool {
	if _, ar := hostLocation(ns); ar {
		return false
	}
	return !strings.EqualFold(ns, r.Host) && !strings.EqualFold(ns, s.RegistryAddr())
}

// mirrorRoute serves a mirror request.
func (s *Service) mirrorRoute(w http.ResponseWriter, r *http.Request, pt *pullThrough, kind, arg string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeRegError(w, regErr(http.StatusMethodNotAllowed, "UNSUPPORTED", "the registry mirror for %s is read-only", pt.up.Host))
		return
	}
	switch kind {
	case "manifests":
		s.ptServeManifest(w, r, pt, arg)
	case "blobs":
		s.ptServeBlob(w, r, pt, arg)
	case "tags":
		s.ptTagsList(w, r, pt)
	default:
		writeRegError(w, regErr(http.StatusMethodNotAllowed, "UNSUPPORTED", "the registry mirror for %s is read-only", pt.up.Host))
	}
}

// ptTagsList passes a tags list request through to the upstream.
func (s *Service) ptTagsList(w http.ResponseWriter, r *http.Request, pt *pullThrough) {
	if s.offline() {
		writeRegError(w, regErr(http.StatusServiceUnavailable, "UNAVAILABLE", "tag listing of %s needs the upstream registry and the emulator is offline", pt.up))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), fetchTimeout)
	defer cancel()
	path := "/v2/" + pt.up.Repo + "/tags/list"
	if q := r.URL.Query(); q.Get("n") != "" || q.Get("last") != "" {
		q.Del("ns")
		path += "?" + q.Encode()
	}
	resp, err := s.upstream.do(ctx, http.MethodGet, pt.up, path, nil)
	if err != nil {
		writeRegError(w, ptError(err, "repository", pt.up.String()))
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, io.LimitReader(resp.Body, 16<<20))
	}
}
