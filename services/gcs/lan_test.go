package gcs_test

import (
	"context"
	"net/http/httptest"
	"testing"
)

// The Google front end's LAN listener reads buckets the Instance doesn't
// have through from real GCS (#462, ADR 0021), so it must tell them from
// the Instance's own on every API's paths.
func TestForeignBucket(t *testing.T) {
	inst := start(t)
	c := emulatorClient(t, inst)
	if err := c.Bucket("ours").Create(context.Background(), testProject, nil); err != nil {
		t.Fatal(err)
	}
	svc := gcsService(t, inst)
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/proxy-golang-org-prod/abc123?X-Goog-Signature=x", true},
		{"/storage/v1/b/theirs/o/obj", true},
		{"/storage/v1/b/theirs", true},
		{"/download/storage/v1/b/theirs/o/obj", true},
		{"/ours/obj", false},
		{"/ours", false},
		{"/storage/v1/b/ours/o/obj", false},
		{"/download/storage/v1/b/ours/o/obj", false},
		{"/storage/v1/b", false}, // listing names no bucket
		{"/storage/v1/b?project=p", false},
		{"/", false},
		{"/upload/storage/v1/b/theirs/o", false}, // only reads go through
		{"/batch/storage/v1", false},
	} {
		if got := svc.ForeignBucket(httptest.NewRequest("GET", tc.path, nil)); got != tc.want {
			t.Errorf("ForeignBucket(%s) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// Signed admits on the LAN only what the XML API verifies itself.
func TestSigned(t *testing.T) {
	svc := gcsService(t, start(t))
	for _, tc := range []struct {
		path, authz string
		want        bool
	}{
		{path: "/ours/obj?X-Goog-Algorithm=GOOG4-RSA-SHA256&X-Goog-Signature=ab", want: true},
		{path: "/ours/obj?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=ab", want: true},
		{path: "/ours/obj", authz: "GOOG4-HMAC-SHA256 Credential=GOOG1x/20260101/auto/storage/goog4_request, SignedHeaders=host, Signature=ab", want: true},
		{path: "/ours/obj", authz: "Bearer x"},
		{path: "/ours/obj"},
		// The JSON API doesn't verify signatures.
		{path: "/storage/v1/b/ours/o/obj?X-Goog-Algorithm=GOOG4-RSA-SHA256&X-Goog-Signature=ab"},
		{path: "/download/storage/v1/b/ours/o/obj?X-Goog-Algorithm=GOOG4-RSA-SHA256"},
	} {
		r := httptest.NewRequest("GET", tc.path, nil)
		if tc.authz != "" {
			r.Header.Set("Authorization", tc.authz)
		}
		if got := svc.Signed(r); got != tc.want {
			t.Errorf("Signed(%s, %q) = %v, want %v", tc.path, tc.authz, got, tc.want)
		}
	}
}
