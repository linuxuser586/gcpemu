package ar

import (
	"net/http"
	"net/url"
	"sort"
	"strings"

	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// FR-AR-006: remote repositories (pull-through caches of Docker Hub or a
// custom registry) and virtual repositories (one name over several
// upstream repositories, resolved by policy priority).
//
// A remote repository serves LOCATION-docker.pkg.dev/P/REPO/IMAGE from
// the upstream image IMAGE (Docker Hub applies its "library/" prefix to
// single-segment names, so .../dockerhub/busybox is docker.io/library/
// busybox). Cached images are ordinary content of the repository: they are
// listed as its packages and docker images, can be deleted, and go away
// with the repository. Remote and virtual repositories reject pushes.

// targetRepo returns the repository of a resolved target (nil if it
// vanished).
func (s *Service) targetRepo(t *target) *artifactregistrypb.Repository {
	var repo *artifactregistrypb.Repository
	_ = s.env.Store.View(func(tx store.Tx) error { repo, _ = getRepo(tx, t.ref); return nil })
	return repo
}

func errReadOnlyRepo(mode artifactregistrypb.Repository_Mode, ref repoRef) *regError {
	kind := "Remote"
	if mode == artifactregistrypb.Repository_VIRTUAL_REPOSITORY {
		kind = "Virtual"
	}
	return regErr(http.StatusForbidden, "DENIED", "%s repository %s does not support uploading artifacts.", kind, ref.name())
}

// remoteUpstream returns the upstream image a remote repository serves for
// img.
func (s *Service) remoteUpstream(repo *artifactregistrypb.Repository, img string) (upstreamRef, bool) {
	rc := repo.GetRemoteRepositoryConfig()
	raw := rc.GetCommonRepository().GetUri()
	if dr := rc.GetDockerRepository(); dr != nil {
		if dr.GetPublicRepository() == artifactregistrypb.RemoteRepositoryConfig_DockerRepository_DOCKER_HUB {
			return upstreamRef{Host: dockerHub, Base: s.upstreamBase(dockerHub), Repo: canonicalRepo(dockerHub, img)}, true
		}
		raw = dr.GetCustomRepository().GetUri()
	}
	u, ok := parseUpstreamURI(raw)
	if !ok {
		return upstreamRef{}, false
	}
	host := canonicalHost(u.Host)
	base := u.Scheme + "://" + u.Host
	if host == dockerHub {
		base = s.upstreamBase(host)
	}
	repoPath := img
	if p := strings.Trim(u.Path, "/"); p != "" {
		repoPath = p + "/" + img
	}
	return upstreamRef{Host: host, Base: base, Repo: canonicalRepo(host, repoPath)}, true
}

// parseUpstreamURI parses a custom remote URI ("https://host[/prefix]";
// a missing scheme means https).
func parseUpstreamURI(raw string) (*url.URL, bool) {
	if raw == "" {
		return nil, false
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" {
		return nil, false
	}
	return u, true
}

// remoteRoute serves requests to a remote repository.
func (s *Service) remoteRoute(w http.ResponseWriter, r *http.Request, t *target, repo *artifactregistrypb.Repository, kind, arg string) {
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	switch {
	case kind == "uploads" || (kind == "manifests" && r.Method == http.MethodPut):
		if s.authorize(w, r, t, permUpload) {
			writeRegError(w, errReadOnlyRepo(repo.GetMode(), t.ref))
		}
		return
	case (kind == "manifests" || kind == "blobs") && read:
	default:
		s.standardRoute(w, r, t, kind, arg) // deletes, tags and referrers act on the cache
		return
	}
	if !s.authorize(w, r, t, permDownload) {
		return
	}
	up, ok := s.remoteUpstream(repo, t.img)
	if !ok {
		writeRegError(w, regErr(http.StatusInternalServerError, "UNKNOWN", "repository %s has no usable upstream", t.ref.name()))
		return
	}
	pt := &pullThrough{scope: t.ref, img: t.img, up: up}
	if kind == "manifests" {
		s.ptServeManifest(w, r, pt, arg)
	} else {
		s.ptServeBlob(w, r, pt, arg)
	}
}

// virtualMembers returns the upstream repositories of a virtual repository
// in pull order (highest priority first) with their configuration.
func (s *Service) virtualMembers(repo *artifactregistrypb.Repository) ([]repoRef, []*artifactregistrypb.Repository) {
	pols := append([]*artifactregistrypb.UpstreamPolicy(nil), repo.GetVirtualRepositoryConfig().GetUpstreamPolicies()...)
	sort.SliceStable(pols, func(i, j int) bool { return pols[i].GetPriority() > pols[j].GetPriority() })
	var refs []repoRef
	var repos []*artifactregistrypb.Repository
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, p := range pols {
			ref, rest, err := parseRepoName(p.GetRepository())
			if err != nil || len(rest) != 0 {
				continue
			}
			m, err := getRepo(tx, ref)
			if err != nil || m.GetMode() == artifactregistrypb.Repository_VIRTUAL_REPOSITORY {
				continue
			}
			refs = append(refs, ref)
			repos = append(repos, m)
		}
		return nil
	})
	return refs, repos
}

// virtualRoute serves requests to a virtual repository: reads resolve
// across the members in priority order; writes are rejected.
func (s *Service) virtualRoute(w http.ResponseWriter, r *http.Request, t *target, repo *artifactregistrypb.Repository, kind, arg string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		perm := permUpload
		if r.Method == http.MethodDelete {
			perm = permDelete
		}
		if s.authorize(w, r, t, perm) {
			writeRegError(w, errReadOnlyRepo(repo.GetMode(), t.ref))
		}
		return
	}
	if !s.authorize(w, r, t, permDownload) {
		return
	}
	refs, members := s.virtualMembers(repo)
	switch kind {
	case "manifests":
		if !isDigestRef(arg) && !tagRe.MatchString(arg) {
			writeRegError(w, regErr(http.StatusBadRequest, "TAG_INVALID", "invalid tag %q", arg))
			return
		}
		var last *regError
		for i, ref := range refs {
			mt := &target{ref: ref, img: t.img, name: t.name}
			if members[i].GetMode() == artifactregistrypb.Repository_REMOTE_REPOSITORY {
				up, ok := s.remoteUpstream(members[i], t.img)
				if !ok {
					continue
				}
				m, body, err := s.ptManifest(&pullThrough{scope: ref, img: t.img, up: up}, arg)
				if err == nil {
					writeManifest(w, r, m, body)
					return
				}
				if rerr := ptError(err, "manifest", arg); rerr.status != http.StatusNotFound {
					last = rerr
				}
				continue
			}
			if m, rerr := s.lookupManifest(mt, arg); rerr == nil {
				if body, err := s.blobs.read(m.Digest); err == nil {
					writeManifest(w, r, m, body)
					return
				}
			}
		}
		if last != nil {
			writeRegError(w, last)
			return
		}
		writeRegError(w, regErr(http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown: %s", arg))
	case "blobs":
		if _, err := parseDigest(arg); err != nil {
			writeRegError(w, regErr(http.StatusBadRequest, "DIGEST_INVALID", "%v", err))
			return
		}
		var last *regError
		for i, ref := range refs {
			if members[i].GetMode() == artifactregistrypb.Repository_REMOTE_REPOSITORY {
				up, ok := s.remoteUpstream(members[i], t.img)
				if !ok {
					continue
				}
				src, err := s.ptOpenBlob(r.Context(), &pullThrough{scope: ref, img: t.img, up: up}, arg)
				if err == nil {
					defer src.close()
					s.serveBlobSource(w, r, arg, src)
					return
				}
				if rerr := ptError(err, "blob", arg); rerr.status != http.StatusNotFound {
					last = rerr
				}
				continue
			}
			var ok bool
			_ = s.env.Store.View(func(tx store.Tx) error { _, ok = getLink(tx, ref, t.img, arg); return nil })
			if !ok {
				continue
			}
			if f, err := s.blobs.open(arg); err == nil {
				src := &blobSource{file: f}
				defer src.close()
				s.serveBlobSource(w, r, arg, src)
				return
			}
		}
		if last != nil {
			writeRegError(w, last)
			return
		}
		writeRegError(w, regErr(http.StatusNotFound, "BLOB_UNKNOWN", "blob unknown to registry"))
	case "tags":
		seen := map[string]bool{}
		var tags []string
		_ = s.env.Store.View(func(tx store.Tx) error {
			for _, ref := range refs {
				pre := imgPrefix(ref, t.img)
				tx.Scan(nsTags, pre, func(k string, _ []byte) bool {
					if tag := strings.TrimPrefix(k, pre); !seen[tag] {
						seen[tag] = true
						tags = append(tags, tag)
					}
					return true
				})
			}
			return nil
		})
		if len(tags) == 0 {
			writeRegError(w, errNameUnknown(t.name))
			return
		}
		sort.Strings(tags)
		pg, _, rerr := paginate(tags, r)
		if rerr != nil {
			writeRegError(w, rerr)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"name": t.name, "tags": nonNil(pg)})
	default:
		writeRegError(w, regErr(http.StatusNotFound, "NOT_FOUND", "not found"))
	}
}

// checkModeConfig validates the mode-specific configuration of a new
// repository (FR-AR-006).
func checkModeConfig(r *artifactregistrypb.Repository, loc string) error {
	switch r.GetMode() {
	case artifactregistrypb.Repository_MODE_UNSPECIFIED, artifactregistrypb.Repository_STANDARD_REPOSITORY:
		if r.GetModeConfig() != nil {
			return apierr.InvalidArgument("remote_repository_config and virtual_repository_config are only valid for REMOTE_REPOSITORY and VIRTUAL_REPOSITORY repositories.")
		}
	case artifactregistrypb.Repository_REMOTE_REPOSITORY:
		rc := r.GetRemoteRepositoryConfig()
		if rc == nil {
			return apierr.InvalidArgument("remote_repository_config is required for REMOTE_REPOSITORY repositories.")
		}
		switch {
		case rc.GetDockerRepository() != nil:
			dr := rc.GetDockerRepository()
			if dr.GetPublicRepository() == artifactregistrypb.RemoteRepositoryConfig_DockerRepository_DOCKER_HUB {
				return nil
			}
			if dr.GetCustomRepository() != nil {
				if _, ok := parseUpstreamURI(dr.GetCustomRepository().GetUri()); !ok {
					return apierr.InvalidArgument("Invalid remote_repository_config.docker_repository.custom_repository.uri %q.", dr.GetCustomRepository().GetUri())
				}
				return nil
			}
			return apierr.InvalidArgument("remote_repository_config.docker_repository must set public_repository or custom_repository.")
		case rc.GetCommonRepository() != nil:
			if _, ok := parseUpstreamURI(rc.GetCommonRepository().GetUri()); !ok {
				return apierr.InvalidArgument("Invalid remote_repository_config.common_repository.uri %q.", rc.GetCommonRepository().GetUri())
			}
		default:
			return apierr.InvalidArgument("remote_repository_config must set docker_repository or common_repository for DOCKER repositories.")
		}
	case artifactregistrypb.Repository_VIRTUAL_REPOSITORY:
		vc := r.GetVirtualRepositoryConfig()
		if vc == nil {
			return apierr.InvalidArgument("virtual_repository_config is required for VIRTUAL_REPOSITORY repositories.")
		}
		return checkUpstreamPolicies(vc, loc)
	default:
		return apierr.InvalidArgument("Invalid repository mode %v.", r.GetMode())
	}
	return nil
}

// checkUpstreamPolicies validates the members of a virtual repository.
func checkUpstreamPolicies(vc *artifactregistrypb.VirtualRepositoryConfig, loc string) error {
	ids := map[string]bool{}
	for i, p := range vc.GetUpstreamPolicies() {
		ref, rest, err := parseRepoName(p.GetRepository())
		if err != nil || len(rest) != 0 {
			return apierr.InvalidArgument("Invalid virtual_repository_config.upstream_policies[%d].repository %q.", i, p.GetRepository())
		}
		if ref.Location != loc {
			return apierr.InvalidArgument("Upstream repository %q must be in location %s.", p.GetRepository(), loc)
		}
		if id := p.GetId(); id != "" {
			if ids[id] {
				return apierr.InvalidArgument("Duplicate upstream policy id %q.", id)
			}
			ids[id] = true
		}
	}
	return nil
}
