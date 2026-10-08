package webconsole

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestHandler(t *testing.T) {
	h := Handler(fstest.MapFS{
		"index.html":           {Data: []byte("<!doctype html><title>console</title>")},
		"favicon.svg":          {Data: []byte("<svg/>")},
		"assets/index-abc1.js": {Data: []byte("console.log(1)")},
	})
	for _, tc := range []struct {
		method, path string
		code         int
		body, cache  string
		location     string
	}{
		{"GET", "/console", http.StatusMovedPermanently, "", "", "/console/"},
		{"GET", "/console?project=p1", http.StatusMovedPermanently, "", "", "/console/?project=p1"},
		{"GET", "/console/", 200, "<title>console</title>", "no-cache", ""},
		{"GET", "/console/index.html", 200, "<title>console</title>", "no-cache", ""},
		{"GET", "/console/gcs/buckets?project=p1", 200, "<title>console</title>", "no-cache", ""},
		{"GET", "/console/assets/index-abc1.js", 200, "console.log(1)", "public, max-age=31536000, immutable", ""},
		{"GET", "/console/assets/gone-0000.js", 404, "", "", ""},
		{"GET", "/console/favicon.svg", 200, "<svg/>", "no-cache", ""},
		{"GET", "/console/../etc/passwd", 200, "<title>console</title>", "no-cache", ""},
		{"POST", "/console/", http.StatusMethodNotAllowed, "", "", ""},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.code {
			t.Errorf("%s %s: code %d, want %d", tc.method, tc.path, w.Code, tc.code)
			continue
		}
		if tc.body != "" && !strings.Contains(w.Body.String(), tc.body) {
			t.Errorf("%s %s: body %q lacks %q", tc.method, tc.path, w.Body, tc.body)
		}
		if got := w.Header().Get("Cache-Control"); tc.cache != "" && got != tc.cache {
			t.Errorf("%s %s: Cache-Control %q, want %q", tc.method, tc.path, got, tc.cache)
		}
		if got := w.Header().Get("Location"); got != tc.location {
			t.Errorf("%s %s: Location %q, want %q", tc.method, tc.path, got, tc.location)
		}
		if tc.code == 200 && w.Header().Get("Content-Security-Policy") != "default-src 'self'" {
			t.Errorf("%s %s: CSP %q", tc.method, tc.path, w.Header().Get("Content-Security-Policy"))
		}
	}
}
