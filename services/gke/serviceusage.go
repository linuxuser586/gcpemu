package gke

import (
	"net/http"
	"strings"
)

// A minimal Service Usage API: `gcloud container ...` checks that
// container.googleapis.com is enabled before every call (and would
// otherwise ask the real serviceusage.googleapis.com). Every service is
// reported ENABLED and enabling is a no-op. gcloud reaches it through
// CLOUDSDK_API_ENDPOINT_OVERRIDES_SERVICEUSAGE.
func serviceUsageHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /v1/projects/P/services/NAME[:enable]
		segs := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if len(segs) != 5 || segs[0] != "v1" || segs[1] != "projects" || segs[3] != "services" {
			http.NotFound(w, r)
			return
		}
		name, verb, _ := strings.Cut(segs[4], ":")
		parent := "projects/" + projectNumber(segs[2])
		switch {
		case r.Method == http.MethodGet && verb == "":
			writeJSON(w, map[string]any{
				"name":   parent + "/services/" + name,
				"parent": parent,
				"config": map[string]any{"name": name},
				"state":  "ENABLED",
			})
		case r.Method == http.MethodPost && (verb == "enable" || verb == "disable"):
			writeJSON(w, map[string]any{"name": "operations/noop.DONE_OPERATION", "done": true})
		default:
			http.NotFound(w, r)
		}
	})
}
