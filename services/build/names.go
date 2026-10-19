package build

import (
	"strings"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

const (
	apiHost = "cloudbuild.googleapis.com"
	// resRoot prefixes the full resource names IAM checks are made on.
	resRoot = "//" + apiHost + "/"
	// globalLocation is where Builds made through projects/*/builds run.
	globalLocation = "global"
)

// buildRef names one Build.
type buildRef struct{ Project, Location, ID string }

// parent is the Build's collection owner, "projects/P/locations/L".
func (r buildRef) parent() string { return locationName(r.Project, r.Location) }

// name is the Build's resource name.
func (r buildRef) name() string { return r.parent() + "/builds/" + r.ID }

// key is the Build's store key; keys of one location share a prefix.
func (r buildRef) key() string { return locationKey(r.Project, r.Location) + r.ID }

func (r buildRef) resource() string { return resRoot + r.name() }

func locationName(project, location string) string {
	return "projects/" + project + "/locations/" + location
}

// locationKey is the store key prefix of a location's Builds.
func locationKey(project, location string) string { return project + "/" + location + "/" }

// parseParent resolves the parent of a create or list call: a project ID
// (projects/P/builds, the global location) or projects/P/locations/L.
func parseParent(projectID, parent string) (project, location string, err error) {
	if parent == "" {
		if projectID == "" {
			return "", "", apierr.InvalidArgument("project_id is required.")
		}
		return projectID, globalLocation, nil
	}
	segs := strings.Split(parent, "/")
	if len(segs) != 4 || segs[0] != "projects" || segs[2] != "locations" || segs[1] == "" || segs[3] == "" {
		return "", "", apierr.InvalidArgument("Invalid parent %q: want projects/{project}/locations/{location}.", parent)
	}
	if projectID != "" && projectID != segs[1] {
		return "", "", apierr.InvalidArgument("project_id %q does not match parent %q.", projectID, parent)
	}
	return segs[1], segs[3], nil
}

// parseBuild resolves the Build a get, cancel or retry call names: a
// project ID and Build ID (the global location), or a resource name.
func parseBuild(projectID, id, name string) (buildRef, error) {
	if name == "" {
		if projectID == "" || id == "" {
			return buildRef{}, apierr.InvalidArgument("project_id and id are required.")
		}
		return buildRef{projectID, globalLocation, id}, nil
	}
	segs := strings.Split(name, "/")
	if len(segs) != 6 || segs[0] != "projects" || segs[2] != "locations" || segs[4] != "builds" ||
		segs[1] == "" || segs[3] == "" || segs[5] == "" {
		return buildRef{}, apierr.InvalidArgument("Invalid build name %q: want projects/{project}/locations/{location}/builds/{build}.", name)
	}
	return buildRef{segs[1], segs[3], segs[5]}, nil
}
