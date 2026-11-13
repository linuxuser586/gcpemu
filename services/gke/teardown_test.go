package gke

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/netplane"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/internal/store"
	"github.com/linuxuser586/gcpemu/internal/vmrelay"
)

// slowRemove is a Docker API serving one labelled container whose removal
// takes until the test releases it, and goes on when the client
// disconnects, as dockerd's does. While the container is being removed,
// every other DELETE of it gets 409 "removal … is already in progress".
type slowRemove struct {
	mu       sync.Mutex
	name     string
	labels   map[string]string
	exists   bool
	removing bool
	deletes  int // DELETEs of the container, whatever their answer

	started  chan struct{} // closed when the removal starts
	cut      chan struct{} // closed if the removal's client disconnects
	conflict chan struct{} // closed on a DELETE while it is being removed
	released chan struct{} // closed to end the removal
	once     struct{ started, cut, conflict, released sync.Once }
	m        *runtime.Manager
}

func newSlowRemove(t *testing.T, name string, labels map[string]string) *slowRemove {
	t.Helper()
	d := &slowRemove{name: name, labels: labels, exists: true, started: make(chan struct{}),
		cut: make(chan struct{}), conflict: make(chan struct{}), released: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(d.serve))
	t.Cleanup(srv.Close)
	t.Cleanup(d.release)
	cl, err := runtime.New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(vmrelay.Env, "off")
	d.m = &runtime.Manager{Client: cl, InstanceID: "i1", Ephemeral: true, Log: slog.Default()}
	return d
}

// release ends the removal in progress.
func (d *slowRemove) release() { d.once.released.Do(func() { close(d.released) }) }

func (d *slowRemove) serve(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1.41")
	d.mu.Lock()
	defer d.mu.Unlock()
	ct := "/containers/" + d.name
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/networks/"):
		_ = json.NewEncoder(w).Encode(map[string]any{"Id": "svc", "Name": strings.TrimPrefix(path, "/networks/"),
			"Options": map[string]string{"com.docker.network.bridge.name": "br-svc"},
			"IPAM":    map[string]any{"Config": []map[string]string{{"Subnet": "10.9.0.0/24", "Gateway": "10.9.0.1"}}}})
	case r.Method == http.MethodGet && path == "/containers/json":
		out := []map[string]string{}
		if d.exists {
			out = append(out, map[string]string{"Id": d.name})
		}
		_ = json.NewEncoder(w).Encode(out)
	case r.Method == http.MethodGet && path == ct+"/json" && d.exists:
		_ = json.NewEncoder(w).Encode(map[string]any{"Id": d.name, "Name": "/" + d.name,
			"Config": map[string]any{"Labels": d.labels}, "State": map[string]any{"Status": "running", "Running": true}})
	case r.Method == http.MethodDelete && path == ct:
		d.deletes++
		switch {
		case !d.exists:
			notFound(w)
		case d.removing:
			d.once.conflict.Do(func() { close(d.conflict) })
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "removal of container " + d.name + " is already in progress"})
		default:
			d.removing = true
			d.once.started.Do(func() { close(d.started) })
			d.mu.Unlock()
			select {
			case <-r.Context().Done():
				d.once.cut.Do(func() { close(d.cut) })
				<-d.released // the disconnect does not end the removal
			case <-d.released:
			}
			d.mu.Lock()
			d.exists, d.removing = false, false
			w.WriteHeader(http.StatusNoContent)
		}
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/volumes/"):
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && path == "/volumes":
		_, _ = w.Write([]byte(`{"Volumes":[]}`))
	default:
		notFound(w)
	}
}

func notFound(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"message":"not found"}`))
}

// noSubnets is a VPC with no subnetworks.
type noSubnets struct{ emu.VPC }

func (noSubnets) SubnetNetwork(context.Context, string) (emu.SubnetNet, error) {
	return emu.SubnetNet{}, errors.New("no subnetworks")
}

// A cluster delete that cuts off an Autopilot node removal waits for the
// runtime to finish removing that node's container, rather than removing
// it again and failing with the runtime's 409 (#1144).
func TestClusterDeleteWaitsForCutOffNodeRemoval(t *testing.T) {
	s := newUnitService(t)
	rec := &clusterRecord{Int: clusterInternal{Project: "p", Location: "us-central1-a", Name: "c",
		Nodes: []nodeRecord{{Name: "n0"}, {Name: "n1"}}}}
	rec.setCluster(&containerpb.Cluster{Name: "c", Id: "id1", Location: "us-central1-a", Status: containerpb.Cluster_RUNNING})
	if err := s.env.Store.Update(func(tx store.Tx) error { return putCluster(tx, rec) }); err != nil {
		t.Fatal(err)
	}
	key := rec.key()
	node := "gcpemu-i1-gke-c-id1-n1"
	docker := newSlowRemove(t, node, map[string]string{runtime.LabelInstance: "i1", runtime.LabelService: "gke", runtime.LabelResource: key})
	np := netplane.New(docker.m, func() map[string]string { return nil }, "127.0.0.1", slog.Default())
	t.Cleanup(func() { np.Close(context.Background()) })
	s.env.Containers = planeOnly{np: np}
	s.testVPC = noSubnets{}

	// The autoscaler removes n1 on the controllers' context.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt := s.rtFor(key)
	rt.mu.Lock()
	rt.cancel = cancel
	rt.mu.Unlock()
	a := &autoscaler{s: s, key: key}
	go func() { _ = a.remove(ctx, []nodeRecord{{Name: "n1"}}) }()
	select {
	case <-docker.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the node removal never removed its container")
	}

	destroyed := make(chan error, 1)
	go func() { destroyed <- s.destroyDataPlane(context.Background(), key) }()
	// The delete cancels the controllers. A removal that ends with them
	// disconnects at once, before the delete removes the container again;
	// one that goes on leaves the delete waiting, which only time shows.
	select {
	case <-docker.cut:
		t.Fatal("the cluster delete cut the node removal off while its container was being removed")
	case <-docker.conflict:
		t.Fatal("the cluster delete removed the node's container while its removal was in progress")
	case err := <-destroyed:
		t.Fatalf("the cluster delete finished (%v) while the node's container was being removed", err)
	case <-time.After(time.Second):
	}
	docker.release()
	select {
	case err := <-destroyed:
		if err != nil {
			t.Fatalf("destroyDataPlane = %v; want it to succeed", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the cluster delete never finished")
	}
	docker.mu.Lock()
	defer docker.mu.Unlock()
	if docker.deletes != 1 {
		t.Errorf("container DELETEd %d times; want once", docker.deletes)
	}
}
