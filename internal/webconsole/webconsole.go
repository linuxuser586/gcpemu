// Package webconsole serves the Web console's bundle on the gateway under
// /console (FR-UI-002). The console is a single-page app: it talks only to
// the public APIs and the admin API (FR-UI-003), so this handler serves
// nothing but files.
package webconsole

import (
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// Prefix is the path the console is served under.
const Prefix = "/console/"

// csp allows the console to load only its own bundle and call only the
// gateway it was served from.
const csp = "default-src 'self'"

// Handler serves fsys (the bundle, with index.html at its root) under
// Prefix. /console redirects to /console/; hashed files under assets/ are
// cached for good, index.html never is; any other path that is not a file
// gets index.html, so that deep links survive a reload.
func Handler(fsys fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == strings.TrimSuffix(Prefix, "/") {
			u := Prefix
			if r.URL.RawQuery != "" {
				u += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, u, http.StatusMovedPermanently)
			return
		}
		name, ok := strings.CutPrefix(r.URL.Path, Prefix)
		if !ok {
			http.NotFound(w, r)
			return
		}
		name = strings.TrimPrefix(path.Clean("/"+name), "/")
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(name, "assets/") {
			// A missing asset must not turn into HTML the browser would
			// try to run as a script.
			if !serveFile(w, r, fsys, name) {
				http.NotFound(w, r)
			}
			return
		}
		if name != "" && name != "index.html" && serveFile(w, r, fsys, name) {
			return
		}
		h.Set("Cache-Control", "no-cache")
		if !serveFile(w, r, fsys, "index.html") {
			http.NotFound(w, r)
		}
	})
}

// serveFile writes the regular file name from fsys and reports whether
// there was one. Files under assets/ have content-hashed names.
func serveFile(w http.ResponseWriter, r *http.Request, fsys fs.FS, name string) bool {
	f, err := fsys.Open(name)
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		return false
	}
	switch {
	case strings.HasPrefix(name, "assets/"):
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	case w.Header().Get("Cache-Control") == "":
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, name, time.Time{}, rs)
	return true
}
