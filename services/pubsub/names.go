package pubsub

import (
	"encoding/base64"
	"regexp"
	"sort"
	"strings"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// Resource kinds as they appear in resource names.
const (
	kindTopics        = "topics"
	kindSubscriptions = "subscriptions"
	kindSnapshots     = "snapshots"
	kindSchemas       = "schemas"
)

// deletedTopic is the topic value of subscriptions whose topic was deleted.
const deletedTopic = "_deleted-topic_"

// resourceID is the Pub/Sub resource ID syntax: a letter, then 2–254 of
// letters, digits and - _ . ~ + %, not starting with "goog".
var resourceID = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9\-_.~+%]{2,254}$`)

// schemaID follows the same rules as other resource IDs.
var schemaID = resourceID

// parseName splits "projects/P/<kind>/ID" and validates it.
func parseName(name, kind string) (project, id string, err error) {
	parts := strings.Split(name, "/")
	if len(parts) != 4 || parts[0] != "projects" || parts[2] != kind || parts[1] == "" {
		return "", "", invalidName(kind, name)
	}
	if !resourceID.MatchString(parts[3]) || strings.HasPrefix(strings.ToLower(parts[3]), "goog") {
		return "", "", invalidName(kind, name)
	}
	return parts[1], parts[3], nil
}

func invalidName(kind, name string) error {
	return apierr.InvalidArgument("Invalid [%s] name: (name=%s)", kind, name)
}

// parseProject validates "projects/P" and returns P.
func parseProject(s string) (string, error) {
	p, ok := strings.CutPrefix(s, "projects/")
	if !ok || p == "" || strings.Contains(p, "/") {
		return "", apierr.InvalidArgument("Invalid [projects] name: (name=%s)", s)
	}
	return p, nil
}

// lastSegment returns the ID part of a resource name.
func lastSegment(name string) string {
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		return name[i+1:]
	}
	return name
}

// projectOf returns the project of a resource name ("" if malformed).
func projectOf(name string) string {
	parts := strings.SplitN(name, "/", 3)
	if len(parts) < 2 || parts[0] != "projects" {
		return ""
	}
	return parts[1]
}

// fullName returns the IAM full resource name for a Pub/Sub resource.
func fullName(name string) string { return "//pubsub.googleapis.com/" + name }

// projectResource returns the IAM full resource name of a project.
func projectResource(project string) string {
	return "//cloudresourcemanager.googleapis.com/projects/" + project
}

func notFound(name string) error {
	return apierr.NotFound("Resource not found (resource=%s).", lastSegment(name)).
		WithReason("pubsub.googleapis.com", "RESOURCE_NOT_FOUND")
}

func alreadyExists(name string) error {
	return apierr.AlreadyExists("Resource already exists in the project (resource=%s).", lastSegment(name)).
		WithReason("pubsub.googleapis.com", "RESOURCE_ALREADY_EXISTS")
}

// paginate returns one page of sorted names (FR-CORE-024). The page token
// is the opaque encoding of the last name returned.
func paginate(names []string, pageSize int32, token string) (page []string, next string, err error) {
	sort.Strings(names)
	size := int(pageSize)
	if size <= 0 || size > 1000 {
		size = 1000
		if pageSize <= 0 {
			size = 100
		}
	}
	start := 0
	if token != "" {
		b, derr := base64.RawURLEncoding.DecodeString(token)
		if derr != nil {
			return nil, "", apierr.InvalidArgument("Invalid page token: %s", token)
		}
		last := string(b)
		start = sort.SearchStrings(names, last)
		if start < len(names) && names[start] == last {
			start++
		}
	}
	end := min(start+size, len(names))
	page = names[start:end]
	if end < len(names) && len(page) > 0 {
		next = base64.RawURLEncoding.EncodeToString([]byte(page[len(page)-1]))
	}
	return page, next, nil
}
