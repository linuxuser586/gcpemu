package gateway

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
)

// Admin token transport (FR-UI-009, ADR 0006).
const (
	AdminTokenHeader = "X-Gcpemu-Admin-Token"
	AdminTokenCookie = "gcpemu_admin"
	// AdminTokenQuery is the console URL parameter that sets the cookie.
	AdminTokenQuery = "token"
)

// SetAdminToken makes every Admin API and Web console request present tok
// (the API gateway is bound beyond loopback); "" turns the guard off.
// Call it before Serve.
func (g *Gateway) SetAdminToken(tok string) { g.adminToken = tok }

// adminGuarded reports whether path needs the Admin token. Liveness and
// readiness stay open so that probes need no secret; the GKE hooks under
// /container/_emu/ have their own per-cluster secret. It looks at the path
// alone; see adminRequest for which requests keep their cookies.
func adminGuarded(r *http.Request) bool {
	p := r.URL.Path
	switch {
	case p == "/_emu/v1/health" || p == "/_emu/v1/ready":
		return r.Method != http.MethodGet && r.Method != http.MethodHead
	case strings.HasPrefix(p, "/_emu/"), p == "/console", strings.HasPrefix(p, "/console/"):
		return true
	}
	return false
}

// adminRequest reports whether r is an Admin API or Web console request,
// the only requests that keep their cookies, where the token may travel
// (#888): an Admin-guarded path on a hostname Host mode does not serve. Host
// mode's hostnames serve only public APIs (#890).
func (g *Gateway) adminRequest(r *http.Request) bool {
	return adminGuarded(r) && !g.publicHost(r.Host)
}

// publicHost reports whether host, with or without a port, is one of the
// hostnames Host mode serves (see Hosts).
func (g *Gateway) publicHost(host string) bool {
	host = hostOnly(host)
	if host == PathHost {
		return true
	}
	g.mu.RLock()
	_, ok := g.hosts[host]
	g.mu.RUnlock()
	return ok
}

// checkAdmin enforces the Admin token on guarded paths and reports
// whether the request may go on; otherwise the response is written.
func (g *Gateway) checkAdmin(w http.ResponseWriter, r *http.Request) bool {
	if g.adminToken == "" || !adminGuarded(r) {
		return true
	}
	console := !strings.HasPrefix(r.URL.Path, "/_emu/")
	if q := r.URL.Query(); console && q.Has(AdminTokenQuery) {
		if !g.adminTokenOK(q.Get(AdminTokenQuery)) {
			denyConsole(w)
			return false
		}
		// Swap the token in the URL for a cookie, then drop it from the
		// address bar and history.
		http.SetCookie(w, &http.Cookie{
			Name: AdminTokenCookie, Value: g.adminToken, Path: "/",
			HttpOnly: true, SameSite: http.SameSiteStrictMode,
		})
		q.Del(AdminTokenQuery)
		u := r.URL.Path
		if len(q) > 0 {
			u += "?" + q.Encode()
		}
		w.Header().Set("Referrer-Policy", "no-referrer")
		http.Redirect(w, r, u, http.StatusSeeOther)
		return false
	}
	tok := r.Header.Get(AdminTokenHeader)
	if tok == "" {
		if c, err := r.Cookie(AdminTokenCookie); err == nil {
			tok = c.Value
		}
	}
	if g.adminTokenOK(tok) {
		return true
	}
	if console {
		denyConsole(w)
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": "the admin API needs the admin token because the API gateway is bound beyond loopback: send it in the " +
			AdminTokenHeader + " header (`gcpemu admin token` prints it)",
	})
	return false
}

func (g *Gateway) adminTokenOK(tok string) bool {
	return tok != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(g.adminToken)) == 1
}

func denyConsole(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(consoleDenied))
}

const consoleDenied = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>gcpemu console: admin token needed</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:40rem;margin:4rem auto;padding:0 1rem}code{font-size:.9em}</style>
</head><body>
<h1>Admin token needed</h1>
<p>This emulator's API gateway is bound beyond loopback, so the Web console needs its admin token.</p>
<p>Open the console URL that <code>gcpemu start</code> printed, or run <code>gcpemu console</code> on the emulator's host.
<code>gcpemu admin token</code> prints the token on its own.</p>
</body></html>
`
