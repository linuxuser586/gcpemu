package lb

import (
	"net/http"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/services/gcs"
)

// Backend buckets serve objects from the emulated Cloud Storage bucket
// (FR-INT-003) through gcs.ServeObject (website index/404 pages, Range,
// conditional requests).

func (d *dataplane) bucketHandler(rs *reqState, b *computev1.BackendBucket) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		svc, ok := d.s.env.Lookup("gcs")
		g, ok2 := svc.(*gcs.Service)
		if !ok || !ok2 {
			rs.details = "failed_to_connect_to_backend"
			http.Error(w, "Cloud Storage is not running", http.StatusBadGateway)
			return
		}
		// Set before serving: custom error responses check it when the
		// status is written.
		rs.details = "response_sent_by_backend"
		sw := clock.StartStopwatch()
		g.ServeObject(w, r, b.BucketName, r.URL.Path)
		rs.bLat = sw.Elapsed()
		rs.backend = "storage.googleapis.com/" + b.BucketName
	})
}
