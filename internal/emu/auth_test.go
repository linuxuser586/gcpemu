package emu

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/store"
)

type denyAll struct{}

func (denyAll) Allowed(context.Context, Principal, string, string) bool { return false }

// TestAuthorizerModes is FR-IAM-004: off allows silently, audit allows
// and logs the permission real IAM would deny, enforce denies.
func TestAuthorizerModes(t *testing.T) {
	ctx := WithPrincipal(context.Background(), "user:eve@example.com")
	for _, mode := range []string{config.IAMOff, config.IAMAudit, config.IAMEnforce} {
		var log strings.Builder
		a := NewPolicyAuthorizer(mode, slog.New(slog.NewTextHandler(&log, nil)))
		a.Install(nil, denyAll{})
		err := a.Check(ctx, "storage.objects.get", "//storage.googleapis.com/projects/_/buckets/b")
		logged := strings.Contains(log.String(), "would deny") && strings.Contains(log.String(), "permission=storage.objects.get") &&
			strings.Contains(log.String(), "principal=user:eve@example.com") && strings.Contains(log.String(), "buckets/b")
		switch mode {
		case config.IAMOff:
			if err != nil || log.Len() != 0 {
				t.Errorf("off: %v, log %q", err, log.String())
			}
		case config.IAMAudit:
			if err != nil || !logged {
				t.Errorf("audit: %v, log %q", err, log.String())
			}
		case config.IAMEnforce:
			if e := apierr.From(err); err == nil || e.HTTP() != 403 || e.Reason != "IAM_PERMISSION_DENIED" {
				t.Errorf("enforce: %v", err)
			}
		}
	}
}

// TestEnsureProject is FR-CORE-020: projects are auto-created with a
// deterministic number unless --strict-projects requires them to be
// declared.
func TestEnsureProject(t *testing.T) {
	newEnv := func(strict bool, declared ...string) *Env {
		cfg := config.Defaults()
		cfg.StrictProjects, cfg.Projects = strict, declared
		return &Env{Config: &cfg, Store: store.NewMemory(), Clock: clock.Real{}}
	}
	e := newEnv(false)
	if err := e.EnsureProject("auto-created-1"); err != nil {
		t.Fatal(err)
	}
	if err := e.EnsureProject("Bad_ID"); apierr.From(err).HTTP() != 400 {
		t.Errorf("invalid ID: %v", err)
	}
	var rec struct{ ProjectNumber string }
	_ = e.Store.View(func(tx store.Tx) error { return store.GetJSON(tx, projectsNS, "auto-created-1", &rec) })
	var again struct{ ProjectNumber string }
	e2 := newEnv(false)
	_ = e2.EnsureProject("auto-created-1")
	_ = e2.Store.View(func(tx store.Tx) error { return store.GetJSON(tx, projectsNS, "auto-created-1", &again) })
	if rec.ProjectNumber == "" || rec.ProjectNumber != again.ProjectNumber {
		t.Errorf("project numbers %q, %q", rec.ProjectNumber, again.ProjectNumber)
	}

	s := newEnv(true, "declared-proj")
	if err := s.EnsureProject("declared-proj"); err != nil {
		t.Errorf("declared: %v", err)
	}
	err := s.EnsureProject("undeclared-proj")
	if e := apierr.From(err); err == nil || e.HTTP() != 404 || e.Reason != "PROJECT_NOT_FOUND" {
		t.Errorf("undeclared in strict mode: %v", err)
	}
}
