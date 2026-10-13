package emu

import (
	"context"
	"fmt"
	"log/slog"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/config"
)

// TestDenialLog (FR-UI-014): denied calls are one Denial per Principal,
// permission and resource, listed most recently denied first; off records
// nothing; and the log keeps the denialsKept most recent.
func TestDenialLog(t *testing.T) {
	a := NewPolicyAuthorizer(config.IAMAudit, slog.New(slog.DiscardHandler))
	a.Install(nil, denyAll{})
	var seen []Denial
	a.OnDeny(func(d Denial) { seen = append(seen, d) })
	ctx, ci := WithCallInfo(WithPrincipal(context.Background(), "user:a@example.com"))
	ci.SetCall("gcs", "GET /storage/v1/b/b")
	for _, perm := range []string{"storage.buckets.get", "storage.objects.list", "storage.buckets.get"} {
		_ = a.Check(ctx, perm, "//storage.googleapis.com/projects/_/buckets/b")
	}
	a.SetMode(config.IAMOff)
	_ = a.Check(ctx, "storage.buckets.delete", "//storage.googleapis.com/projects/_/buckets/b")

	ds := a.Denials(context.Background(), "")
	if len(ds) != 2 || len(seen) != 3 {
		t.Fatalf("denials = %+v, events = %d", ds, len(seen))
	}
	if d := ds[0]; d.Permission != "storage.buckets.get" || d.Count != 2 || d.Service != "gcs" || d.Method != "GET /storage/v1/b/b" || d.Level != "audit" {
		t.Errorf("latest denial = %+v", d)
	}
	if ds[0].ID != denialID("user:a@example.com", "storage.buckets.get", "//storage.googleapis.com/projects/_/buckets/b") {
		t.Errorf("ID %s is not stable", ds[0].ID)
	}

	a.SetMode(config.IAMAudit)
	for i := range denialsKept + 1 {
		_ = a.Check(ctx, fmt.Sprintf("svc.things.verb%d", i), "//x/y")
	}
	ds = a.Denials(context.Background(), "")
	if len(ds) != denialsKept || ds[0].Permission != fmt.Sprintf("svc.things.verb%d", denialsKept) {
		t.Errorf("after %d more denials: %d kept, latest %s", denialsKept+1, len(ds), ds[0].Permission)
	}
	a.ClearDenials()
	if ds := a.Denials(context.Background(), ""); len(ds) != 0 {
		t.Errorf("after ClearDenials: %d", len(ds))
	}
}
