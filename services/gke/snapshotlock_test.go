package gke

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/linuxuser586/gcpemu/internal/store"
)

const lockTestCluster = "projects/lock-project/locations/us-central1-a/clusters/c"

// putLockCluster stores the RUNNING cluster lockTestCluster names and
// returns its key.
func putLockCluster(t *testing.T, s *Service) string {
	t.Helper()
	rec := &clusterRecord{Int: clusterInternal{Project: "lock-project", Location: "us-central1-a", Name: "c"}}
	rec.setCluster(&containerpb.Cluster{Name: "c", Id: "id1", Status: containerpb.Cluster_RUNNING,
		NodePools: []*containerpb.NodePool{{Name: "np", Status: containerpb.NodePool_RUNNING}}})
	if err := s.env.Store.Update(func(tx store.Tx) error { return putCluster(tx, rec) }); err != nil {
		t.Fatal(err)
	}
	return rec.key()
}

// A mutating call that arrives while a State snapshot save holds the
// cluster waits for the save and then goes on as usual (#294).
func TestMutateWaitsForSnapshotSave(t *testing.T) {
	s := newUnitService(t)
	key := putLockCluster(t, s)
	if _, ok := s.tryLock(key, snapshotOp); !ok {
		t.Fatal("lock taken")
	}
	time.AfterFunc(200*time.Millisecond, func() { s.release(key, snapshotOp) })
	if err := setLabels(context.Background(), s); err != nil {
		t.Fatalf("SetLabels during a State snapshot save: %v", err)
	}
	if got := clusterLabels(t, s, key); got["a"] != "b" {
		t.Errorf("labels %v after the save, want a=b", got)
	}
}

// shortSnapshotWait shortens snapshotWait for the test.
func shortSnapshotWait(t *testing.T, d time.Duration) {
	t.Helper()
	old := snapshotWait
	snapshotWait = d
	t.Cleanup(func() { snapshotWait = old })
}

// setLabels sets a=b on lockTestCluster, a mutating call.
func setLabels(ctx context.Context, s *Service) error {
	_, err := s.api.SetLabels(ctx, &containerpb.SetLabelsRequest{
		Name: lockTestCluster, ResourceLabels: map[string]string{"a": "b"}})
	return err
}

// A save that outlasts snapshotWait fails the call with an error that says
// a State snapshot is being saved, not the lock's internal name.
func TestMutateSnapshotSaveTimeout(t *testing.T) {
	shortSnapshotWait(t, 100*time.Millisecond)
	s := newUnitService(t)
	key := putLockCluster(t, s)
	s.tryLock(key, snapshotOp)
	err := setLabels(context.Background(), s)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("SetLabels: %v, want FailedPrecondition", err)
	}
	msg := status.Convert(err).Message()
	if !strings.Contains(msg, "State snapshot") || !strings.Contains(msg, "retry") {
		t.Errorf("message %q does not say a State snapshot is being saved and to retry", msg)
	}
	if strings.Contains(err.Error(), snapshotOp) {
		t.Errorf("error %q names %s", err, snapshotOp)
	}
}

// Cancelling the request ends the wait with the context's error.
func TestMutateSnapshotSaveCancelled(t *testing.T) {
	s := newUnitService(t)
	key := putLockCluster(t, s)
	s.tryLock(key, snapshotOp)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	err := setLabels(ctx, s)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("SetLabels: %v, want context.Canceled", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("returned after %v", d)
	}
}

// A real Operation holding the cluster fails the call at once, naming it.
func TestMutateRealOperationNoWait(t *testing.T) {
	s := newUnitService(t)
	key := putLockCluster(t, s)
	s.tryLock(key, "operation-1")
	start := time.Now()
	err := setLabels(context.Background(), s)
	if status.Code(err) != codes.FailedPrecondition ||
		status.Convert(err).Message() != "Cluster is running incompatible operation operation-1." {
		t.Fatalf("SetLabels: %v, want the incompatible operation error", err)
	}
	// A wait would poll at least once, every 500ms.
	if d := time.Since(start); d >= 500*time.Millisecond {
		t.Errorf("waited %v", d)
	}
}

// pausingStore pauses after each write commits, holding open any gap
// between a stored change and what its caller does next.
type pausingStore struct{ store.Store }

func (p pausingStore) Update(fn func(store.Tx) error) error {
	err := p.Store.Update(fn)
	time.Sleep(50 * time.Millisecond)
	return err
}

// Once an Operation shows DONE its Operation lock is free, so a client
// that polls and goes straight on is not told it is still running (#974).
func TestOperationDoneFreesLock(t *testing.T) {
	tests := []struct {
		name    string
		service func(*testing.T) *Service
	}{
		{"background", newUnitService},
		{"deterministic", newDeterministicService},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.service(t)
			putLockCluster(t, s)
			s.env.Store = pausingStore{s.env.Store}
			first := make(chan error, 1)
			go func() { first <- setLabels(context.Background(), s) }()
			deadline := time.Now().Add(10 * time.Second)
			for !slices.ContainsFunc(s.listOps("lock-project", "-"), func(op *containerpb.Operation) bool {
				return op.Status == containerpb.Operation_DONE
			}) {
				if time.Now().After(deadline) {
					t.Fatal("no operation finished")
				}
				time.Sleep(5 * time.Millisecond)
			}
			if err := setLabels(context.Background(), s); err != nil {
				t.Errorf("next call after DONE: %v", err)
			}
			if err := <-first; err != nil {
				t.Errorf("first call: %v", err)
			}
		})
	}
}

// An Operation that takes the lock as the save releases it wins: the
// waiting call fails naming that Operation.
func TestMutateSnapshotSaveThenOperation(t *testing.T) {
	s := newUnitService(t)
	key := putLockCluster(t, s)
	s.tryLock(key, snapshotOp)
	time.AfterFunc(200*time.Millisecond, func() {
		s.mu.Lock()
		s.busy[key] = "operation-2"
		s.mu.Unlock()
	})
	err := setLabels(context.Background(), s)
	if status.Convert(err).Message() != "Cluster is running incompatible operation operation-2." {
		t.Fatalf("SetLabels: %v, want the incompatible operation error for operation-2", err)
	}
	if got := clusterLabels(t, s, key); len(got) != 0 {
		t.Errorf("labels %v stored by a call that lost the lock", got)
	}
}

// startOp, which takes the lock, waits for a save the same way and then
// holds the lock for its operation.
func TestStartOpWaitsForSnapshotSave(t *testing.T) {
	s := newUnitService(t)
	key := putLockCluster(t, s)
	s.tryLock(key, snapshotOp)
	time.AfterFunc(200*time.Millisecond, func() { s.release(key, snapshotOp) })
	r := ref{Project: "lock-project", Location: "us-central1-a", Cluster: "c"}
	done := make(chan struct{})
	defer close(done)
	op, err := s.startOp(context.Background(), r, containerpb.Operation_UPDATE_CLUSTER, "t", true,
		func(context.Context) error { <-done; return nil })
	if err != nil {
		t.Fatalf("startOp during a State snapshot save: %v", err)
	}
	s.mu.Lock()
	cur := s.busy[key]
	s.mu.Unlock()
	if cur != op.Name {
		t.Errorf("lock held by %q, want %q", cur, op.Name)
	}
}

func clusterLabels(t *testing.T, s *Service, key string) map[string]string {
	t.Helper()
	rec, err := s.loadKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return rec.cluster().ResourceLabels
}
