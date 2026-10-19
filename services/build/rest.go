package build

import (
	"net/http"
	"strings"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/grpc/codes"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/transcode"
)

// restHandler serves cloudbuild v1 REST (gcloud, OpenTofu) through the
// generated google.api.http bindings, so REST and gRPC share one code
// path, and the Build Operations at
// /v1/projects/*/locations/*/operations/*. Cloud Build v2 (repository
// connections) answers UNIMPLEMENTED.
func (s *Service) restHandler() http.Handler {
	t := transcode.New()
	cloudbuildpb.RegisterCloudBuildServer(t, s.api)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v2/") {
			apierr.Write(w, apierr.New(codes.Unimplemented, "Cloud Build API v2 is not emulated."))
			return
		}
		if rest, ok := strings.CutPrefix(r.URL.Path, "/v1/"); ok && s.ops.ServeREST(w, r, rest) {
			return
		}
		t.ServeHTTP(w, r)
	})
}
