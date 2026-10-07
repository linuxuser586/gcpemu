package cdn

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc"

	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// muxRouter is a minimal emu.Router over a ServeMux.
type muxRouter struct{ mux *http.ServeMux }

func (m muxRouter) Mount(api string, hosts []string, h http.Handler) {
	m.mux.Handle("/"+api+"/", http.StripPrefix("/"+api, h))
}
func (m muxRouter) Handle(p string, h http.Handler) { m.mux.Handle(p, h) }
func (m muxRouter) GRPC() *grpc.Server              { return nil }
func (m muxRouter) Fallback(h http.Handler)         { m.mux.Handle("/", h) }

func newEnv(t *testing.T, dir, size string) *emu.Env {
	cfg := config.Defaults()
	cfg.CDNCacheSize = size
	return &emu.Env{Config: &cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DataDir: dir, Clock: clock.Real{}}
}

func TestService(t *testing.T) {
	dir := t.TempDir()
	s := New(newEnv(t, dir, "64MiB")).(*Service)
	if s.Name() != "cdn" || s.Ready() != nil {
		t.Fatal("name/ready")
	}
	if s.Cache().Limit() != 64<<20 {
		t.Fatalf("limit %d", s.Cache().Limit())
	}
	// lb's view of the contract.
	var svc emu.Service = s
	cp, ok := svc.(interface{ Cache() *Cache })
	if !ok || cp.Cache() == nil {
		t.Fatal("Cache() contract")
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	if err := s.Register(muxRouter{mux}); err != nil {
		t.Fatal(err)
	}
	o := &origin{serve: static(200, "x", "Content-Type", "image/png")}
	do(s.Cache(), Backend{ID: "b"}, o, get("http://h/a"))
	do(s.Cache(), Backend{ID: "b"}, o, get("http://h/b"))

	call := func(method, path string) map[string]any {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		if w.Code != 200 {
			t.Fatalf("%s %s: %d", method, path, w.Code)
		}
		m := map[string]any{}
		json.Unmarshal(w.Body.Bytes(), &m)
		return m
	}
	if m := call("GET", "/_emu/v1/cdn"); m["entries"] != float64(2) || m["limitBytes"] != float64(64<<20) {
		t.Fatalf("stats %v", m)
	}
	if m := call("POST", "/_emu/v1/cdn/purge"); m["purged"] != float64(2) {
		t.Fatalf("purge %v", m)
	}
	do(s.Cache(), Backend{ID: "b"}, o, get("http://h/a"))

	// Restart in --data-dir mode keeps the cache.
	s2 := New(newEnv(t, dir, "")).(*Service)
	if n, _ := s2.Cache().Stats(); n != 1 {
		t.Fatalf("restart entries %d", n)
	}
	if s2.Cache().Limit() != DefaultCacheSize {
		t.Fatal("default size")
	}
	var _ emu.Resetter = s2
	if err := s2.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n, _ := s2.Cache().Stats(); n != 0 {
		t.Fatalf("after reset %d", n)
	}
	if err := s2.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	// An invalid size falls back to the default.
	if New(newEnv(t, t.TempDir(), "lots")).(*Service).Cache().Limit() != DefaultCacheSize {
		t.Fatal("invalid size")
	}
}
