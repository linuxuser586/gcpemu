package gke

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

type tokenAuthn map[string]emu.Principal

func (a tokenAuthn) Authenticate(_ context.Context, tok string) (emu.Principal, bool) {
	p, ok := a[tok]
	return p, ok
}

func lanTestService(mode string, gkeBind ...string) *Service {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Defaults()
	cfg.GKEBind = gkeBind
	auth := emu.NewPolicyAuthorizer(mode, log)
	auth.Install(tokenAuthn{"good": "user:alice@example.com"}, nil)
	env := &emu.Env{Config: &cfg, Store: store.NewMemory(), Clock: clock.Real{}, IDs: emu.NewIDs(true), Log: log,
		Auth: auth, Endpoints: emu.NewEndpoints()}
	return New(env).(*Service)
}

func reviewToken(t *testing.T, s *Service, tok string) (bool, string) {
	t.Helper()
	body := `{"apiVersion":"authentication.k8s.io/v1","kind":"TokenReview","spec":{"token":"` + tok + `"}}`
	w := httptest.NewRecorder()
	s.serveTokenReview(w, httptest.NewRequest("POST", "/", strings.NewReader(body)), "k")
	var tr tokenReview
	if err := json.Unmarshal(w.Body.Bytes(), &tr); err != nil {
		t.Fatal(err)
	}
	if tr.Status.User == nil {
		return tr.Status.Authenticated, ""
	}
	return tr.Status.Authenticated, tr.Status.User.Username
}

// With --gke-bind the API server is reachable from the LAN, so only real
// credentials authenticate, whatever the IAM enforcement level.
func TestTokenReviewStrictWithGKEBind(t *testing.T) {
	for _, mode := range []string{config.IAMOff, config.IAMAudit, config.IAMEnforce} {
		t.Run(mode, func(t *testing.T) {
			loop := lanTestService(mode)
			lan := lanTestService(mode, "192.168.1.20")
			for _, s := range []*Service{loop, lan} {
				if ok, user := reviewToken(t, s, "good"); !ok || user != "alice@example.com" {
					t.Errorf("good token = %v %q", ok, user)
				}
			}
			ok, user := reviewToken(t, loop, "bogus")
			if want := mode != config.IAMEnforce; ok != want || (ok && user != "dev@example.com") {
				t.Errorf("loopback bogus token = %v %q, want %v", ok, user, want)
			}
			if ok, _ := reviewToken(t, lan, "bogus"); ok {
				t.Error("--gke-bind: bogus token authenticated")
			}
		})
	}
}

func TestLANAddress(t *testing.T) {
	s := lanTestService(config.IAMAudit, "192.168.1.20", "10.0.0.5")
	for v, want := range map[string]string{"": "", "false": "", "true": "192.168.1.20", "1": "192.168.1.20", "10.0.0.5": "10.0.0.5"} {
		if got, err := s.lanAddress(v); err != nil || got != want {
			t.Errorf("lanAddress(%q) = %q, %v; want %q", v, got, err, want)
		}
	}
	if _, err := s.lanAddress("10.9.9.9"); err == nil {
		t.Error("lanAddress accepted an address that is not --gke-bind")
	}
	if _, err := lanTestService(config.IAMAudit).lanAddress("true"); err == nil || !strings.Contains(err.Error(), "--gke-bind") {
		t.Errorf("lan=true without --gke-bind: %v", err)
	}
}
