package gateway

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// consoleCookies are what a browser sends with the console's same-origin
// requests: the Admin token cookie and an authenticating proxy's session.
const consoleCookies = AdminTokenCookie + "=secret; _oauth2_proxy=session"

// A public API request reaches its Service without the console's cookies
// (#888).
func TestPublicRequestLosesCookies(t *testing.T) {
	g := New(&emu.Env{Config: &config.Config{}, Log: slog.New(slog.DiscardHandler)}, nil)
	g.SetAdminToken("secret")
	var cookie []string
	served := false
	g.mux.HandleFunc("/v1/", func(_ http.ResponseWriter, r *http.Request) { served, cookie = true, r.Header.Values("Cookie") })
	r := httptest.NewRequest(http.MethodGet, "/v1/projects/local-proj/topics", nil)
	r.Header.Set("Cookie", consoleCookies)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if !served || len(cookie) != 0 {
		t.Errorf("served %v with Cookie %q, want served with none (%d %s)", served, cookie, w.Code, w.Body)
	}
}

// cookieLinks delegates every Project as fakeLinks does and keeps the
// Cookie header of the request it forwards.
type cookieLinks struct {
	fakeLinks
	cookie *[]string
}

func (l cookieLinks) Forward(w http.ResponseWriter, r *http.Request, name, ref string, fail func(http.ResponseWriter, error), done func(*http.Response)) {
	*l.cookie = r.Header.Values("Cookie")
	l.fakeLinks.Forward(w, r, name, ref, fail, done)
}

// A request for a Delegated Project reaches its Linked Instance without
// the console's cookies, so this Instance's Admin token stays here (#888).
func TestDelegatedRequestLosesCookies(t *testing.T) {
	g := New(&emu.Env{Config: &config.Config{}, Log: slog.New(slog.DiscardHandler)}, nil)
	g.SetAdminToken("secret")
	var cookie []string
	g.SetLinks(cookieLinks{fakeLinks{delegated: map[string]string{"team-shared": "office"}}, &cookie})
	r := httptest.NewRequest(http.MethodGet, "/v1/projects/team-shared/topics", nil)
	r.Header.Set("Cookie", consoleCookies)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Header().Get("Forwarded-To") != "office" || len(cookie) != 0 {
		t.Errorf("forwarded to %q with Cookie %q, want to office with none (%d %s)", w.Header().Get("Forwarded-To"), cookie, w.Code, w.Body)
	}
}

// An Admin API request that carries the Admin token only in its cookie is
// still admitted (ADR 0006).
func TestAdminRequestKeepsCookies(t *testing.T) {
	g := New(&emu.Env{Config: &config.Config{}, Log: slog.New(slog.DiscardHandler)}, nil)
	g.SetAdminToken("secret")
	served := false
	g.HandleAdmin(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served = true }))
	r := httptest.NewRequest(http.MethodPost, "/_emu/v1/reset", nil)
	r.Header.Set("Cookie", AdminTokenCookie+"=secret")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if !served || w.Code != http.StatusOK {
		t.Errorf("Admin API with the Admin token cookie: served %v, %d %s", served, w.Code, w.Body)
	}
}
