package secrets

import (
	"net/http"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"

	"github.com/linuxuser586/gcpemu/internal/transcode"
)

// restHandler serves secretmanager v1 REST (gcloud, OpenTofu) through the
// generated google.api.http bindings, so REST and gRPC share one code path.
func (s *Service) restHandler() http.Handler {
	t := transcode.New()
	secretmanagerpb.RegisterSecretManagerServiceServer(t, s.api)
	if err := t.AddServer("google.cloud.location.Locations", &locationsServer{s: s}); err != nil {
		panic(err)
	}
	return t
}
