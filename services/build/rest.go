package build

import (
	"bytes"
	"encoding/json"
	"io"
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
		if rest, ok := strings.CutPrefix(r.URL.Path, "/_emu/agent/"); ok {
			s.serveAgent(w, r, rest)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v2/") {
			apierr.Write(w, apierr.New(codes.Unimplemented, "Cloud Build API v2 is not emulated."))
			return
		}
		if rest, ok := strings.CutPrefix(r.URL.Path, "/v1/"); ok && s.ops.ServeREST(w, r, rest) {
			return
		}
		if err := absentRESTSource(r); err != nil {
			apierr.Write(w, err)
			return
		}
		t.ServeHTTP(w, r)
	})
}

// maxRESTBody caps a Build created over REST.
const maxRESTBody = 32 << 20

// absentRESTSource refuses a Build created over REST with a source the
// cloudbuildpb in use predates, which transcoding would drop:
// developerConnectConfig. It leaves r's body to be read again.
func absentRESTSource(r *http.Request) error {
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/builds") || r.Body == nil {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRESTBody+1))
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return apierr.InvalidArgument("Reading the request body: %v", err)
	}
	if len(body) > maxRESTBody {
		return apierr.InvalidArgument("The request body is larger than %d bytes.", maxRESTBody)
	}
	var b struct {
		Source map[string]json.RawMessage `json:"source"`
	}
	if json.Unmarshal(body, &b) == nil && b.Source["developerConnectConfig"] != nil {
		return absentSource("developerConnectConfig")
	}
	return nil
}
