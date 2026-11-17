package gke

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"go.yaml.in/yaml/v3"

	"github.com/linuxuser586/gcpemu/internal/store"
)

// putTestCluster stores a cluster record with the given status.
func putTestCluster(t *testing.T, s *Service, status containerpb.Cluster_Status) string {
	t.Helper()
	rec := &clusterRecord{Int: clusterInternal{Project: "p", Location: "us-central1-a", Name: "c"}}
	rec.setCluster(&containerpb.Cluster{Name: "c", Id: "id1", Status: status,
		NodePools: []*containerpb.NodePool{{Name: "np", Status: containerpb.NodePool_PROVISIONING}}})
	if err := s.env.Store.Update(func(tx store.Tx) error { return putCluster(tx, rec) }); err != nil {
		t.Fatal(err)
	}
	return rec.key()
}

func clusterStatus(t *testing.T, s *Service, key string) *containerpb.Cluster {
	t.Helper()
	rec, err := s.loadKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return rec.cluster()
}

// Reset cancels an operation still running and waits for it: nothing the
// operation does after its context ends happens after Reset returns
// (#293).
func TestResetWaitsForOperations(t *testing.T) {
	s := newUnitService(t)
	started := make(chan struct{})
	var returned atomic.Bool
	r := ref{Project: "p", Location: "us-central1-a", Cluster: "c"}
	if _, err := s.startOp(context.Background(), r, containerpb.Operation_CREATE_CLUSTER, "t", true, func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		time.Sleep(100 * time.Millisecond) // slow to stop, like removing a container
		returned.Store(true)
		return ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := s.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !returned.Load() {
		t.Fatal("operation still running after Reset returned")
	}
}

// Reset waits for background bring-ups and teardowns (goBackground).
func TestResetWaitsForBackground(t *testing.T) {
	s := newUnitService(t)
	started := make(chan struct{})
	var returned atomic.Bool
	s.goBackground(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		time.Sleep(100 * time.Millisecond)
		returned.Store(true)
	})
	<-started
	if err := s.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !returned.Load() {
		t.Fatal("background work still running after Reset returned")
	}
}

// Work cancelled by Reset or Stop keeps the persisted status; work whose
// own operation was cancelled still reports ERROR.
func TestMarkErrorAfterCancel(t *testing.T) {
	t.Run("reset", func(t *testing.T) {
		s := newUnitService(t)
		key := putTestCluster(t, s, containerpb.Cluster_PROVISIONING)
		started := make(chan struct{})
		r := ref{Project: "p", Location: "us-central1-a", Cluster: "c"}
		if _, err := s.startOp(context.Background(), r, containerpb.Operation_CREATE_CLUSTER, "t", true, func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			s.markError(ctx, key, ctx.Err())
			return ctx.Err()
		}); err != nil {
			t.Fatal(err)
		}
		<-started
		// Reset reports that it could not tear down the data plane (no
		// runtime); the operation has still stopped.
		_ = s.Reset(context.Background())
		if c := clusterStatus(t, s, key); c.Status != containerpb.Cluster_PROVISIONING || c.NodePools[0].Status != containerpb.NodePool_PROVISIONING {
			t.Fatalf("status after Reset = %v / %v", c.Status, c.NodePools[0].Status)
		}
	})
	t.Run("stop", func(t *testing.T) {
		s := newUnitService(t)
		key := putTestCluster(t, s, containerpb.Cluster_PROVISIONING)
		started := make(chan struct{})
		s.goBackground(func(ctx context.Context) {
			close(started)
			<-ctx.Done()
			s.markError(ctx, key, ctx.Err())
		})
		<-started
		if err := s.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		if c := clusterStatus(t, s, key); c.Status != containerpb.Cluster_PROVISIONING {
			t.Fatalf("status after Stop = %v", c.Status)
		}
	})
	t.Run("operation cancelled", func(t *testing.T) {
		s := newUnitService(t)
		key := putTestCluster(t, s, containerpb.Cluster_PROVISIONING)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		s.markError(ctx, key, ctx.Err())
		if c := clusterStatus(t, s, key); c.Status != containerpb.Cluster_ERROR {
			t.Fatalf("status = %v, want ERROR", c.Status)
		}
	})
}

// After Reset, operations and background work run on a live context.
func TestResetRenewsBackground(t *testing.T) {
	s := newUnitService(t)
	if err := s.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	live := make(chan error, 2)
	s.goBackground(func(ctx context.Context) { live <- ctx.Err() })
	r := ref{Project: "p", Location: "us-central1-a", Cluster: "c"}
	if _, err := s.startOp(context.Background(), r, containerpb.Operation_CREATE_CLUSTER, "t", true, func(ctx context.Context) error {
		live <- ctx.Err()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := <-live; err != nil {
			t.Fatalf("work after Reset got a done context: %v", err)
		}
	}
}

// Reset returns when its context ends before background work stops.
func TestResetBounded(t *testing.T) {
	s := newUnitService(t)
	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{})
	s.goBackground(func(ctx context.Context) {
		close(started)
		<-release // ignores cancellation
	})
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Reset(ctx) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Reset = %v, want deadline exceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Reset hung")
	}
}

// A bring-up from before a Reset that reaches startControllers afterwards
// starts no controllers in the new background generation.
func TestResetOldBringUpStartsNoControllers(t *testing.T) {
	s := newUnitService(t)
	key := "p/us-central1-a/c"
	old := make(chan context.Context, 1)
	s.goBackground(func(ctx context.Context) { old <- ctx })
	ctx := <-old
	if err := s.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.startControllers(ctx, key)
	rt := s.rtFor(key)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.cancel != nil {
		t.Fatal("controllers started from a bring-up the Reset cancelled")
	}
}

// After Stop, no operation starts.
func TestStopRefusesOperations(t *testing.T) {
	s := newUnitService(t)
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := ref{Project: "p", Location: "us-central1-a", Cluster: "c"}
	if _, err := s.startOp(context.Background(), r, containerpb.Operation_CREATE_CLUSTER, "t", true, func(ctx context.Context) error { return nil }); err == nil {
		t.Fatal("operation started after Stop")
	}
	if _, ok := s.tryLock(r.key(), "x"); !ok {
		t.Fatal("refused operation kept the cluster lock")
	}
}

// putRunningClusterWithCreds stores putRunningCluster's cluster, served on
// the --gke-bind address 192.168.1.20, with the admin credentials a
// bring-up leaves, and writes the kubeconfigs as the bring-up does. It
// returns the cluster's context name.
func putRunningClusterWithCreds(t *testing.T, s *Service) string {
	t.Helper()
	rec := putRunningCluster(t, s, "127.0.0.1:6443", "192.168.1.20:6443")
	rt := s.rtFor(rec.key())
	rt.mu.Lock()
	rt.creds = adminCreds{CA: []byte("ca"), Cert: []byte("cert"), Key: []byte("key")}
	rt.mu.Unlock()
	s.writeKubeconfig()
	return contextName(rec.Int.Project, rec.Int.Location, rec.Int.Name)
}

func readKubeconfig(t *testing.T, path string) kubeconfigFile {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var kc kubeconfigFile
	if err := yaml.Unmarshal(b, &kc); err != nil {
		t.Fatal(err)
	}
	return kc
}

// wantOnlyContext checks that each kubeconfig in paths holds the one
// context want, and makes it current.
func wantOnlyContext(t *testing.T, stage string, paths []string, want string) {
	t.Helper()
	for _, p := range paths {
		kc := readKubeconfig(t, p)
		if len(kc.Clusters) != 1 || len(kc.Users) != 1 || len(kc.Contexts) != 1 || kc.Contexts[0].Name != want || kc.CurrentContext != want {
			t.Fatalf("%s: %s has contexts %v, current %q, want only %q", stage, p, kc.Contexts, kc.CurrentContext, want)
		}
	}
}

// resetInstance resets s as the Instance does: the Service, then the
// Store, then ResetDone.
func resetInstance(t *testing.T, s *Service) {
	t.Helper()
	_ = s.Reset(context.Background()) // no container runtime to tear down
	if err := s.env.Store.Reset(); err != nil {
		t.Fatal(err)
	}
	s.ResetDone()
}

// After a reset the Admin kubeconfig, and the LAN kubeconfig under
// --gke-bind, list no clusters; a cluster created afterwards is written
// to them as before (ADR 0086, #1170).
func TestResetEmptiesKubeconfigs(t *testing.T) {
	s := newUnitService(t)
	s.env.Config.DataDir = t.TempDir()
	s.env.Config.GKEBind = []string{"192.168.1.20"}
	paths := []string{s.kubeconfigPath(), filepath.Join(s.env.Config.InstanceDir(), lanKubeconfigFile)}

	wantOnlyContext(t, "before reset", paths, putRunningClusterWithCreds(t, s))

	resetInstance(t, s)
	for _, p := range paths {
		kc := readKubeconfig(t, p)
		if len(kc.Clusters) != 0 || len(kc.Users) != 0 || len(kc.Contexts) != 0 || kc.CurrentContext != "" {
			t.Errorf("after reset: %s has clusters %v, users %v, contexts %v, current %q, want none",
				p, kc.Clusters, kc.Users, kc.Contexts, kc.CurrentContext)
		}
	}

	wantOnlyContext(t, "after a new cluster", paths, putRunningClusterWithCreds(t, s))
}
