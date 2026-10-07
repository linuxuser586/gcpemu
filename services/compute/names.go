package compute

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/locations"
)

// selfLinkBase is the prefix of every compute selfLink. GCP (and the
// OpenTofu provider, which compares selfLinks) use the www.googleapis.com form.
const selfLinkBase = "https://www.googleapis.com/compute/v1/"

// pacific is the fixed offset GCP renders creationTimestamp in.
var pacific = time.FixedZone("", -7*60*60)

// stamp renders t like GCP's creationTimestamp ("2026-10-07T03:04:05.123-07:00").
func stamp(t time.Time) string { return t.In(pacific).Format("2006-01-02T15:04:05.000-07:00") }

// link returns the selfLink of a relative resource path.
func link(path string) string {
	if path == "" {
		return ""
	}
	return selfLinkBase + path
}

// relPath reduces a full or partial compute URL to its path starting at
// "projects/" (or returns s unchanged when it has none).
func relPath(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "projects/"); i >= 0 {
		return strings.TrimSuffix(s[i:], "/")
	}
	return strings.Trim(s, "/")
}

// lastSeg returns the final path segment of a URL or path.
func lastSeg(s string) string {
	s = strings.TrimSuffix(s, "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// globalRef canonicalises a reference to a global resource of collection
// ("networks", "addresses", ...) given as a short name, partial path
// ("global/networks/n") or full URL, to "projects/P/global/COLL/NAME".
func globalRef(project, coll, ref string) (string, error) {
	p := relPath(ref)
	switch segs := strings.Split(p, "/"); {
	case len(segs) == 1 && segs[0] != "":
		return fmt.Sprintf("projects/%s/global/%s/%s", project, coll, segs[0]), nil
	case len(segs) == 3 && segs[0] == "global" && segs[1] == coll:
		return fmt.Sprintf("projects/%s/global/%s/%s", project, coll, segs[2]), nil
	case len(segs) == 5 && segs[0] == "projects" && segs[2] == "global" && segs[3] == coll:
		return p, nil
	}
	return "", apierr.InvalidArgument("Invalid value for field '%s': '%s'. The URL is malformed.", coll, ref).WithLegacy("invalid")
}

// regionalRef canonicalises a reference to a regional resource to
// "projects/P/regions/R/COLL/NAME"; region applies to short names.
func regionalRef(project, region, coll, ref string) (string, error) {
	p := relPath(ref)
	switch segs := strings.Split(p, "/"); {
	case len(segs) == 1 && segs[0] != "" && region != "":
		return fmt.Sprintf("projects/%s/regions/%s/%s/%s", project, region, coll, segs[0]), nil
	case len(segs) == 4 && segs[0] == "regions" && segs[2] == coll:
		return fmt.Sprintf("projects/%s/regions/%s/%s/%s", project, segs[1], coll, segs[3]), nil
	case len(segs) == 6 && segs[0] == "projects" && segs[2] == "regions" && segs[4] == coll:
		return p, nil
	}
	return "", apierr.InvalidArgument("Invalid value for field '%s': '%s'. The URL is malformed.", coll, ref).WithLegacy("invalid")
}

// pathParts splits "projects/P/regions/R/subnetworks/S" into project,
// location ("global", region or zone) and name.
func pathParts(path string) (project, loc, name string) {
	segs := strings.Split(path, "/")
	if len(segs) >= 2 {
		project = segs[1]
	}
	switch {
	case len(segs) == 5 && segs[2] == "global":
		loc, name = "global", segs[4]
	case len(segs) == 6:
		loc, name = segs[3], segs[5]
	}
	return project, loc, name
}

var nameRE = regexp.MustCompile(`^[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?$`)

// validName checks an RFC 1035 resource name like GCP does.
func validName(field, name string) error {
	if name == "" {
		return apierr.InvalidArgument("Invalid value for field '%s': ''. Must be a match of regex '(?:[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?)'", field).WithLegacy("invalid")
	}
	if !nameRE.MatchString(name) {
		return apierr.InvalidArgument("Invalid value for field '%s': '%s'. Must be a match of regex '(?:[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?)'", field, name).WithLegacy("invalid")
	}
	return nil
}

// errNotFound is GCP's compute 404 for a resource path.
func errNotFound(path string) error {
	return apierr.NotFound("The resource '%s' was not found", path).WithLegacy("notFound")
}

// errExists is GCP's compute 409 for a duplicate insert.
func errExists(path string) error {
	return apierr.AlreadyExists("The resource '%s' already exists", path).WithLegacy("alreadyExists")
}

// errInUse is GCP's compute 400 when a resource is still referenced (FR-CORE-026).
func errInUse(kind, path, user string) error {
	return apierr.FailedPrecondition("The %s resource '%s' is already being used by '%s'", kind, path, user).
		WithLegacy("resourceInUseByAnotherResource")
}

// errInvalidField is the common compute 400 for a bad field value.
func errInvalidField(field string, value any, why string) error {
	msg := fmt.Sprintf("Invalid value for field '%s': '%v'.", field, value)
	if why != "" {
		msg += " " + why
	}
	return apierr.InvalidArgument("%s", msg).WithLegacy("invalid")
}

// errRequired is the compute 400 for a missing required field.
func errRequired(field string) error {
	return apierr.InvalidArgument("Required field '%s' not specified", field).WithLegacy("required")
}

// checkRegion validates a region path parameter (FR-CORE-021).
func checkRegion(project, region string) error {
	if !locations.IsRegion(region) {
		return errNotFound("projects/" + project + "/regions/" + region)
	}
	return nil
}

// checkZone validates a zone path parameter (FR-CORE-021).
func checkZone(project, zone string) error {
	if !locations.IsZone(zone) {
		return errNotFound("projects/" + project + "/zones/" + zone)
	}
	return nil
}
