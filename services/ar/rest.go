package ar

import (
	"net/http"
	"strings"

	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"

	"github.com/linuxuser586/gcpemu/internal/transcode"
)

// restHandler serves artifactregistry v1 REST (gcloud, OpenTofu, the Go
// REST client) through the generated google.api.http bindings, so REST
// and gRPC share one code path.
func (s *Service) restHandler() http.Handler {
	t := transcode.New()
	artifactregistrypb.RegisterArtifactRegistryServer(t, s.api)
	if err := t.AddServer("google.cloud.location.Locations", &locationsServer{s: s}); err != nil {
		panic(err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rest, ok := strings.CutPrefix(r.URL.Path, "/v1/"); ok && s.ops.ServeREST(w, r, rest) {
			return
		}
		t.ServeHTTP(w, r)
	})
}
