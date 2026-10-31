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

// A request a Linked Instance forwarded reaches its Service without
// cookies, whatever the Linked Instance let through (#888).
func TestLinkedRequestLosesCookies(t *testing.T) {
	g := New(&emu.Env{Config: &config.Config{}, Log: slog.New(slog.DiscardHandler)}, nil)
	g.SetLinks(fakeLinks{shared: []string{"team-shared"}})
	var cookie []string
	served := false
	g.mux.HandleFunc("/v1/", func(_ http.ResponseWriter, r *http.Request) { served, cookie = true, r.Header.Values("Cookie") })
	r := httptest.NewRequest(http.MethodGet, "/v1/projects/team-shared/topics", nil)
	r.Header.Set("Cookie", consoleCookies)
	w := httptest.NewRecorder()
	g.ServeLinked(w, r, "laptop", "user:a@example.com")
	if !served || len(cookie) != 0 {
		t.Errorf("served %v with Cookie %q, want served with none (%d %s)", served, cookie, w.Code, w.Body)
	}
}

// A request on a Delegated API's host is a public API request whatever its
// path: /_emu/ there reaches the External emulator without the console's
// cookies and without the Admin token check (#890).
func TestDelegatedHostLosesCookies(t *testing.T) {
	g := New(&emu.Env{Config: &config.Config{}, Log: slog.New(slog.DiscardHandler)}, nil)
	g.SetAdminToken("secret")
	var cookie []string
	served := false
	ext := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		served, cookie = true, r.Header.Values("Cookie")
	}))
	defer ext.Close()
	if err := g.Delegate("firestore", ext.Listener.Addr().String()); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "http://firestore.googleapis.com/_emu/v1/anything", nil)
	r.Header.Set("Cookie", consoleCookies)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if !served || len(cookie) != 0 {
		t.Errorf("served %v with Cookie %q, want served with none (%d %s)", served, cookie, w.Code, w.Body)
	}
}

// A request on an in-process Service's host is a public API request
// whatever its path: /console/obj, an object in a bucket named console,
// reaches Cloud Storage without the console's cookies and without needing
// the Admin token (#890).
func TestServiceHostLosesCookies(t *testing.T) {
	g := New(&emu.Env{Config: &config.Config{}, Log: slog.New(slog.DiscardHandler)}, nil)
	g.SetAdminToken("secret")
	var cookie []string
	served := false
	g.Router("gcs").Mount("storage", []string{"storage.googleapis.com"}, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		served, cookie = true, r.Header.Values("Cookie")
	}))
	r := httptest.NewRequest(http.MethodGet, "http://storage.googleapis.com/console/obj", nil)
	r.Header.Set("Cookie", consoleCookies)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if !served || len(cookie) != 0 {
		t.Errorf("served %v with Cookie %q, want served with none (%d %s)", served, cookie, w.Code, w.Body)
	}
}

// www.googleapis.com serves only public APIs: its /_emu/ and /console
// paths answer Not Found, even with the Admin token cookie, and never reach
// the Admin API or the Web console (#890).
func TestPathHostServesNoAdmin(t *testing.T) {
	g := New(&emu.Env{Config: &config.Config{}, Log: slog.New(slog.DiscardHandler)}, nil)
	g.SetAdminToken("secret")
	served := false
	g.HandleAdmin(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served = true }))
	g.HandleConsole(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served = true }))
	for _, path := range []string{"/_emu/v1/reset", "/_emu/v1/health", "/console", "/console/"} {
		served = false
		r := httptest.NewRequest(http.MethodGet, "http://"+PathHost+path, nil)
		r.Header.Set("Cookie", consoleCookies)
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if served || w.Code != http.StatusNotFound {
			t.Errorf("%s: served %v, %d %s, want Not Found", path, served, w.Code, w.Body)
		}
	}
}
