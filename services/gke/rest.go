package gke

import (
	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/internal/transcode"
)

// REST surface: the generated google.api.http bindings of the container v1
// ClusterManager service mapped onto its gRPC implementation, so gcloud
// and OpenTofu (REST) and the Go client (gRPC) share one code path. It
// covers both the projects/P/locations/L and the legacy projects/P/zones/Z
// paths.

// newTranscoder builds the REST handler for impl.
func newTranscoder(impl containerpb.ClusterManagerServer) *transcode.Transcoder {
	t := transcode.New()
	containerpb.RegisterClusterManagerServer(t, impl)
	return t
}
