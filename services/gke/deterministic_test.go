package gke

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"testing"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// newDeterministicService is newUnitService in a Deterministic Instance.
func newDeterministicService(t *testing.T) *Service {
	t.Helper()
	cfg := config.Defaults()
	cfg.Deterministic = true
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(&emu.Env{Config: &cfg, Store: store.NewMemory(), Clock: clock.NewOffset(clock.NewFake()), IDs: emu.NewIDs(true), Log: log,
		Auth: emu.NewPolicyAuthorizer(config.IAMOff, log), Endpoints: emu.NewEndpoints(), DataDir: t.TempDir()}).(*Service)
}

// In a Deterministic Instance an operation with no latency and no data-plane
// work comes back DONE from the call that started it, so a client never
// polls a number of times that depends on wall time (ADR 0020).
func TestDeterministicOperationDoneInResponse(t *testing.T) {
	s := newDeterministicService(t)
	key := putLockCluster(t, s)
	op, err := s.api.SetLabels(context.Background(), &containerpb.SetLabelsRequest{
		Name: lockTestCluster, ResourceLabels: map[string]string{"a": "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != containerpb.Operation_DONE || op.EndTime == "" {
		t.Fatalf("operation = %v (end %q), want DONE in the response", op.Status, op.EndTime)
	}
	if got, err := s.getOp("lock-project", op.Name); err != nil || got.Status != containerpb.Operation_DONE {
		t.Fatalf("stored operation = %v, %v; want DONE", got.GetStatus(), err)
	}
	// The operation lock is free for the next call.
	if _, ok := s.tryLock(key, "next"); !ok {
		t.Fatal("cluster lock still held after the operation finished")
	}
}

// Operations that start at the same instant, as they do within one request
// on the deterministic clock, list in name order.
func TestListOperationsTieBreak(t *testing.T) {
	s := newUnitService(t)
	var want []string
	for i := range 40 {
		name := fmt.Sprintf("operation-%02d", i)
		want = append(want, name)
		if err := s.putOp("tie-project", &containerpb.Operation{Name: name, Location: "us-central1-a",
			StartTime: "2026-01-01T00:00:00Z"}); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := s.api.ListOperations(context.Background(), &containerpb.ListOperationsRequest{
		Parent: "projects/tie-project/locations/-"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, op := range resp.Operations {
		got = append(got, op.Name)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("operations listed %v, want %v", got, want)
	}
}
