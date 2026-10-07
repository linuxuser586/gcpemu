package ar

import (
	"context"
	"encoding/base64"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// API identifiers.
const (
	apiHost  = "artifactregistry.googleapis.com"
	resRoot  = "//artifactregistry.googleapis.com/"
	crmRoot  = "//cloudresourcemanager.googleapis.com/projects/"
	errDom   = "artifactregistry.googleapis.com"
	notFound = "Requested entity was not found."
)

// repoRef identifies a repository.
type repoRef struct {
	Project, Location, Repo string
}

// key is the store key "P/L/R".
func (r repoRef) key() string { return r.Project + "/" + r.Location + "/" + r.Repo }

// name is the API resource name.
func (r repoRef) name() string {
	return "projects/" + r.Project + "/locations/" + r.Location + "/repositories/" + r.Repo
}

// resource is the full resource name used for IAM checks.
func (r repoRef) resource() string { return resRoot + r.name() }

// host is the registry host of the repository's location.
func (r repoRef) host() string { return r.Location + "-docker.pkg.dev" }

// imagePrefix is "LOCATION-docker.pkg.dev/P/R/".
func (r repoRef) imagePrefix() string { return r.host() + "/" + r.Project + "/" + r.Repo + "/" }

func refFromKey(k string) repoRef {
	p := strings.SplitN(k, "/", 3)
	if len(p) != 3 {
		return repoRef{}
	}
	return repoRef{p[0], p[1], p[2]}
}

var (
	repoIDRe = regexp.MustCompile(`^[a-z]([a-z0-9-]*[a-z0-9])?$`)
	// imageRe is the OCI repository path component grammar.
	imageCompRe = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)
	tagRe       = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)
	digestRe    = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

func validImage(img string) bool {
	if img == "" || len(img) > 255 {
		return false
	}
	for _, c := range strings.Split(img, "/") {
		if !imageCompRe.MatchString(c) {
			return false
		}
	}
	return true
}

func cutPrefix(s, p string) (string, bool) { return strings.CutPrefix(s, p) }

func invalidName(name string) error {
	return apierr.InvalidArgument("Invalid resource name %q.", name)
}

// parseLocationName parses "projects/P/locations/L".
func parseLocationName(name string) (project, loc string, err error) {
	p := strings.Split(name, "/")
	if len(p) != 4 || p[0] != "projects" || p[2] != "locations" || p[1] == "" || p[3] == "" {
		return "", "", invalidName(name)
	}
	return p[1], p[3], nil
}

// parseRepoName parses "projects/P/locations/L/repositories/R" and returns
// the remaining path segments after it.
func parseRepoName(name string) (repoRef, []string, error) {
	p := strings.Split(name, "/")
	if len(p) < 6 || p[0] != "projects" || p[2] != "locations" || p[4] != "repositories" || p[1] == "" || p[3] == "" || p[5] == "" {
		return repoRef{}, nil, invalidName(name)
	}
	return repoRef{p[1], p[3], p[5]}, p[6:], nil
}

// escapeImage encodes an image path as an AR package/docker image ID.
func escapeImage(img string) string { return strings.ReplaceAll(img, "/", "%2F") }

// unescapeImage decodes a package ID back into an image path.
func unescapeImage(id string) (string, bool) {
	s, err := url.PathUnescape(id)
	if err != nil || !validImage(s) {
		return "", false
	}
	return s, true
}

// parseChild parses "<repo>/<coll>/<id>[/<sub>/<subid>]" names.
func parseChild(name, coll string, subColl string) (repoRef, string, string, error) {
	r, rest, err := parseRepoName(name)
	if err != nil {
		return r, "", "", err
	}
	want := 2
	if subColl != "" {
		want = 4
	}
	if len(rest) != want || rest[0] != coll || rest[1] == "" || (subColl != "" && (rest[2] != subColl || rest[3] == "")) {
		return r, "", "", invalidName(name)
	}
	if subColl != "" {
		return r, rest[1], rest[3], nil
	}
	return r, rest[1], "", nil
}

// parsePackageName parses ".../packages/PKG" into the image path.
func parsePackageName(name string) (repoRef, string, error) {
	r, id, _, err := parseChild(name, "packages", "")
	if err != nil {
		return r, "", err
	}
	img, ok := unescapeImage(id)
	if !ok {
		return r, "", invalidName(name)
	}
	return r, img, nil
}

// parsePackageSub parses ".../packages/PKG/<sub>/<id>".
func parsePackageSub(name, sub string) (repoRef, string, string, error) {
	r, id, subID, err := parseChild(name, "packages", sub)
	if err != nil {
		return r, "", "", err
	}
	img, ok := unescapeImage(id)
	if !ok {
		return r, "", "", invalidName(name)
	}
	return r, img, subID, nil
}

// parseDockerImageName parses ".../dockerImages/IMG@sha256:...".
func parseDockerImageName(name string) (repoRef, string, string, error) {
	r, id, _, err := parseChild(name, "dockerImages", "")
	if err != nil {
		return r, "", "", err
	}
	i := strings.LastIndex(id, "@")
	if i < 0 {
		return r, "", "", invalidName(name)
	}
	img, ok := unescapeImage(id[:i])
	if !ok || !digestRe.MatchString(id[i+1:]) {
		return r, "", "", invalidName(name)
	}
	return r, img, id[i+1:], nil
}

func packageName(r repoRef, img string) string { return r.name() + "/packages/" + escapeImage(img) }

func versionName(r repoRef, img, digest string) string {
	return packageName(r, img) + "/versions/" + digest
}

func tagName(r repoRef, img, tag string) string { return packageName(r, img) + "/tags/" + tag }

func dockerImageName(r repoRef, img, digest string) string {
	return r.name() + "/dockerImages/" + escapeImage(img) + "@" + digest
}

// pageBounds decodes an offset page token and clamps the page size.
func pageBounds(token string, size int32, total int) (start, n int, err error) {
	if token != "" {
		b, derr := base64.RawURLEncoding.DecodeString(token)
		v, perr := strconv.Atoi(string(b))
		if derr != nil || perr != nil || v < 0 {
			return 0, 0, apierr.InvalidArgument("Invalid page token.")
		}
		start = v
	}
	start = min(start, total)
	n = int(size)
	if n <= 0 || n > 1000 {
		n = 1000
	}
	return start, n, nil
}

// nextToken returns the token for offset next, or "" at the end.
func nextToken(next, total int) string {
	if next >= total {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(next)))
}

// page slices items by token and size.
func page[T any](items []T, token string, size int32) ([]T, string, error) {
	start, n, err := pageBounds(token, size, len(items))
	if err != nil {
		return nil, "", err
	}
	end := min(start+n, len(items))
	return items[start:end], nextToken(end, len(items)), nil
}

// project validates and auto-creates a project and checks a project-level
// permission.
func (s *Service) project(ctx context.Context, p, perm string) error {
	if err := s.env.EnsureProject(p); err != nil {
		return err
	}
	return s.env.Auth.Check(ctx, perm, crmRoot+p)
}

// repoAccess validates the project and location of r and checks perm on
// the repository.
func (s *Service) repoAccess(ctx context.Context, r repoRef, perm string) error {
	if err := s.env.EnsureProject(r.Project); err != nil {
		return err
	}
	if err := checkLocation(r.Location); err != nil {
		return err
	}
	return s.env.Auth.Check(ctx, perm, r.resource())
}
