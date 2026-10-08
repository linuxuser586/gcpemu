package secrets

import (
	"context"
	"encoding/base64"
	"regexp"
	"strconv"
	"strings"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/locations"
	"github.com/linuxuser586/gcpemu/internal/project"
)

const (
	apiHost = "secretmanager.googleapis.com"
	resRoot = "//" + apiHost + "/"
	errDom  = apiHost
)

// regionalHost is the regional endpoint for secrets in location loc.
func regionalHost(loc string) string { return "secretmanager." + loc + ".rep.googleapis.com" }

var (
	secretIDRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,255}$`)
	aliasRe    = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,63}$`)
	labelKeyRe = regexp.MustCompile(`^[\p{Ll}\p{Lo}][\p{Ll}\p{Lo}\p{N}_-]{0,62}$`)
	labelValRe = regexp.MustCompile(`^[\p{Ll}\p{Lo}\p{N}_-]{0,63}$`)
)

// secretRef identifies a secret. Project is always the project ID; the
// API reports names with the project number, as GCP does. Location is
// empty for a global secret.
type secretRef struct {
	Project, Location, ID string
}

// key is the store key: "P/global/S" or "P/<region>/S".
func (r secretRef) key() string {
	loc := r.Location
	if loc == "" {
		loc = "global"
	}
	return r.Project + "/" + loc + "/" + r.ID
}

func (r secretRef) parentWith(p string) string {
	if r.Location == "" {
		return "projects/" + p
	}
	return "projects/" + p + "/locations/" + r.Location
}

// name is the secret's API name, with the project number.
func (r secretRef) name() string {
	return r.parentWith(project.NumberString(r.Project)) + "/secrets/" + r.ID
}

// resource is the full resource name IAM checks use (project ID, so that
// project-level bindings apply).
func (r secretRef) resource() string { return resRoot + r.parentWith(r.Project) + "/secrets/" + r.ID }

func (r secretRef) versionName(n int64) string {
	return r.name() + "/versions/" + strconv.FormatInt(n, 10)
}

func (r secretRef) versionResource(v string) string { return r.resource() + "/versions/" + v }

func refFromKey(k string) secretRef {
	p, rest, _ := strings.Cut(k, "/")
	loc, id, _ := strings.Cut(rest, "/")
	if loc == "global" {
		loc = ""
	}
	return secretRef{p, loc, id}
}

func invalidName(name string) error {
	return apierr.InvalidArgument("The provided name %q is not valid.", name)
}

// resolveProject maps a project ID or number to the project ID.
func (s *Service) resolveProject(p string) (string, error) {
	if p != "" && strings.Trim(p, "0123456789") == "" {
		if id, ok := s.env.ProjectByNumber(p); ok {
			return id, nil
		}
		return "", apierr.NotFound("Project %s not found.", p).WithReason("googleapis.com", "PROJECT_NOT_FOUND")
	}
	if err := s.env.EnsureProject(p); err != nil {
		return "", err
	}
	return p, nil
}

// parseParent parses "projects/P" or "projects/P/locations/L".
func (s *Service) parseParent(parent string) (secretRef, error) {
	parts := strings.Split(parent, "/")
	var ref secretRef
	switch {
	case len(parts) == 2 && parts[0] == "projects" && parts[1] != "":
	case len(parts) == 4 && parts[0] == "projects" && parts[1] != "" && parts[2] == "locations":
		ref.Location = parts[3]
		if !locations.IsRegion(ref.Location) {
			return ref, apierr.InvalidArgument("Location %s is not a valid Secret Manager location.", ref.Location)
		}
	default:
		return ref, invalidName(parent)
	}
	p, err := s.resolveProject(parts[1])
	if err != nil {
		return ref, err
	}
	ref.Project = p
	return ref, nil
}

// parseSecret parses a secret name, returning the trailing path segments
// (e.g. ["versions", "3"]).
func (s *Service) parseSecret(name string) (secretRef, []string, error) {
	parts := strings.Split(name, "/")
	n := 2
	if len(parts) >= 4 && parts[2] == "locations" {
		n = 4
	}
	if len(parts) < n+2 || parts[n] != "secrets" || parts[n+1] == "" {
		return secretRef{}, nil, invalidName(name)
	}
	ref, err := s.parseParent(strings.Join(parts[:n], "/"))
	if err != nil {
		return ref, nil, err
	}
	ref.ID = parts[n+1]
	return ref, parts[n+2:], nil
}

// parseSecretOnly parses a name that must be exactly a secret.
func (s *Service) parseSecretOnly(name string) (secretRef, error) {
	ref, rest, err := s.parseSecret(name)
	if err == nil && len(rest) != 0 {
		err = invalidName(name)
	}
	return ref, err
}

// parseVersion parses ".../secrets/S/versions/V"; V is a number, "latest"
// or an alias.
func (s *Service) parseVersion(name string) (secretRef, string, error) {
	ref, rest, err := s.parseSecret(name)
	if err != nil {
		return ref, "", err
	}
	if len(rest) != 2 || rest[0] != "versions" || rest[1] == "" {
		return ref, "", invalidName(name)
	}
	return ref, rest[1], nil
}

// check verifies the caller holds perm on resource.
func (s *Service) check(ctx context.Context, perm, resource string) error {
	return s.env.Auth.Check(ctx, perm, resource)
}

// page slices items by an opaque offset token.
func page[T any](items []T, token string, size int32) ([]T, string, error) {
	start := 0
	if token != "" {
		b, err := base64.RawURLEncoding.DecodeString(token)
		n, err2 := strconv.Atoi(string(b))
		if err != nil || err2 != nil || n < 0 || n > len(items) {
			return nil, "", apierr.InvalidArgument("Invalid page token.")
		}
		start = n
	}
	if size < 0 {
		return nil, "", apierr.InvalidArgument("page_size must not be negative.")
	}
	if size == 0 || size > 25000 {
		size = 25000
	}
	end := min(start+int(size), len(items))
	next := ""
	if end < len(items) {
		next = base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(end)))
	}
	return items[start:end], next, nil
}
