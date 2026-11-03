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

// The CSP allows exactly the inline scripts of the index.html it serves
// (ADR 0063), and loosens nothing else.
func TestHandlerCSPHashesInlineScripts(t *testing.T) {
	for _, tc := range []struct {
		name, index, csp string
	}{
		{"none", `<!doctype html><script type="module" src="/console/assets/index-abc1.js"></script>`,
			"default-src 'self'"},
		{"one", `<!doctype html><head><script>boot()</script><script type="module" src="/console/assets/index-abc1.js"></script>`,
			"default-src 'self'; script-src 'self' 'sha256-MeZS89WlF0u+o0hCvHTBt4q1WHU+U+sJKgbdRUc36mY='"},
		{"two", "<!doctype html><script>boot()</script><body><SCRIPT>two()</SCRIPT>",
			"default-src 'self'; script-src 'self' 'sha256-MeZS89WlF0u+o0hCvHTBt4q1WHU+U+sJKgbdRUc36mY=' 'sha256-iMrPAWirDC2X0OhZzLsM2MRIWSmepJGI4r7D8ZlwBOw='"},
		// A script's text is hashed as written, not unescaped.
		{"raw", "<script>a&amp;&&b<c</script>",
			"default-src 'self'; script-src 'self' 'sha256-8iig5g4iuUW4seoBf7yOz0hd2kfTxU1KNWztD2VGFCY='"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := Handler(fstest.MapFS{
				"index.html":           {Data: []byte(tc.index)},
				"assets/index-abc1.js": {Data: []byte("console.log(1)")},
			})
			for _, p := range []string{"/console/", "/console/build/projects/p/builds/b", "/console/assets/index-abc1.js"} {
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
				got := w.Header().Get("Content-Security-Policy")
				if got != tc.csp {
					t.Errorf("%s: CSP %q, want %q", p, got, tc.csp)
				}
				if strings.Contains(got, "unsafe-") {
					t.Errorf("%s: CSP %q loosens script-src", p, got)
				}
			}
		})
	}
}
