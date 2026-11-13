package gke

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/linuxuser586/gcpemu/internal/store"
)

// A cluster delete that cannot remove the cluster's containers leaves it in
// ERROR with the failure, not in STOPPING; the Service stays ready, and
// deleting it again removes it (ADR 0081, #1145).
func TestFailedDeleteLeavesError(t *testing.T) {
	a := newAPI(t)
	ctx := context.Background()
	c := createRunning(t, a, apParent, &containerpb.Cluster{Name: "gone", InitialNodeCount: 1})
	r, _ := parseRef(apParent, "", "", "", "", 0)
	r.Cluster = c.Name
	var calls atomic.Int32
	a.s.dataPlaneDestroy = func(context.Context, string) error {
		if calls.Add(1) == 1 {
			return errors.New("removing container k3s-server: 409 conflict")
		}
		return nil
	}

	op, err := a.DeleteCluster(ctx, &containerpb.DeleteClusterRequest{Name: r.clusterName()})
	if err != nil {
		t.Fatalf("DeleteCluster: %v", err)
	}
	waitOp(t, a, r.Project, op.Name)
	got, err := a.GetCluster(ctx, &containerpb.GetClusterRequest{Name: r.clusterName()})
	if err != nil {
		t.Fatalf("GetCluster after the failed delete: %v", err)
	}
	if got.Status != containerpb.Cluster_ERROR || !strings.Contains(got.StatusMessage, "409 conflict") {
		t.Fatalf("after the failed delete: status %v, message %q; want ERROR with the failure", got.Status, got.StatusMessage)
	}
	if err := a.s.Ready(); err != nil {
		t.Fatalf("Ready with a cluster in ERROR: %v", err)
	}

	op, err = a.DeleteCluster(ctx, &containerpb.DeleteClusterRequest{Name: r.clusterName()})
	if err != nil {
		t.Fatalf("second DeleteCluster: %v", err)
	}
	waitOp(t, a, r.Project, op.Name)
	if done, _ := a.s.getOp(r.Project, op.Name); done.GetError() != nil {
		t.Fatalf("second delete failed: %v", done.GetError())
	}
	if _, err := a.GetCluster(ctx, &containerpb.GetClusterRequest{Name: r.clusterName()}); status.Code(err) != codes.NotFound {
		t.Fatalf("GetCluster after the second delete: %v, want NotFound", err)
	}
}

// putDeleteFailed stores a cluster in ERROR whose delete failed, as a
// failed delete leaves it.
func putDeleteFailed(t *testing.T, s *Service) string {
	t.Helper()
	key := putTestCluster(t, s, containerpb.Cluster_ERROR)
	if err := s.updateCluster(key, func(rec *clusterRecord, c *containerpb.Cluster) error {
		rec.Int.DeleteFailed = true
		c.StatusMessage = "removing container: 409 conflict"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return key
}

// On start, a cluster whose delete failed has its delete run again instead
// of being brought back up, and is gone once it succeeds.
func TestRecoverResumesFailedDelete(t *testing.T) {
	s := newUnitService(t)
	key := putDeleteFailed(t, s)
	entered := make(chan string, 1)
	release := make(chan struct{})
	s.dataPlaneDestroy = func(ctx context.Context, k string) error {
		entered <- k
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	s.recoverClusters()
	select {
	case k := <-entered:
		if k != key {
			t.Fatalf("delete ran for %q, want %q", k, key)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the failed delete was not run again")
	}
	if c := clusterStatus(t, s, key); c.Status != containerpb.Cluster_STOPPING {
		t.Fatalf("status while the delete runs again = %v, want STOPPING (not brought back up)", c.Status)
	}
	close(release)
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.loadKey(key); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("loading the cluster after the delete ran again: %v, want not found", err)
	}
}

// On start, a cluster in ERROR for another reason, such as a failed create,
// is brought back up as before.
func TestRecoverRestoresOtherError(t *testing.T) {
	s := newUnitService(t)
	key := putTestCluster(t, s, containerpb.Cluster_ERROR)
	var deleted atomic.Bool
	s.dataPlaneDestroy = func(context.Context, string) error {
		deleted.Store(true)
		return nil
	}
	// The restore fails at once with no runtime and marks the cluster
	// ERROR, so it is held until the status is read (#1161).
	started, release := make(chan string, 1), make(chan struct{})
	s.restoring = func(_ context.Context, k string) {
		started <- k
		<-release
	}

	s.recoverClusters()
	select {
	case k := <-started:
		if k != key {
			t.Fatalf("restore ran for %q, want %q", k, key)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the cluster was not restored")
	}
	if c := clusterStatus(t, s, key); c.Status != containerpb.Cluster_RECONCILING {
		close(release)
		t.Fatalf("status while the restore runs = %v, want RECONCILING (brought back up)", c.Status)
	}
	close(release)
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if deleted.Load() {
		t.Fatal("a cluster in ERROR from a failed create was deleted on start")
	}
	if _, err := s.loadKey(key); err != nil {
		t.Fatalf("cluster record after recoverClusters: %v", err)
	}
}
