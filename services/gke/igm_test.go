package gke

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"cloud.google.com/go/container/apiv1/containerpb"
	computev1 "google.golang.org/api/compute/v1"
	"google.golang.org/grpc"

	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
	"github.com/linuxuser586/gcpemu/services/compute"
)

// TestInstanceGroups: node pools are visible as zonal instance group
// managers / instance groups on the compute API (the OpenTofu provider
// reads them to compute node_count).
func TestInstanceGroups(t *testing.T) {
	cfg := config.Defaults()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	env := &emu.Env{Config: &cfg, Store: store.NewMemory(), Clock: clock.Real{}, IDs: emu.NewIDs(true), Log: log,
		Auth: emu.NewPolicyAuthorizer(config.IAMOff, log), Endpoints: emu.NewEndpoints()}
	s := New(env).(*Service)
	url := "https://www.googleapis.com/compute/v1/projects/p/zones/us-central1-a/instanceGroupManagers/gke-c-np-1234abcd-grp"
	rec := &clusterRecord{Int: clusterInternal{Project: "p", Location: "us-central1-a", Name: "c",
		Nodes: []nodeRecord{{Name: "n1", Pool: "np", Zone: "us-central1-a"}, {Name: "n2", Pool: "np", Zone: "us-central1-a"}}}}
	rec.setCluster(&containerpb.Cluster{Name: "c", Network: "vpc", CreateTime: "2026-01-01T00:00:00Z",
		NodePools: []*containerpb.NodePool{{Name: "np", InstanceGroupUrls: []string{url}}}})
	if err := env.Store.Update(func(tx store.Tx) error { return putCluster(tx, rec) }); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	const z = "/compute/v1/projects/{project}/zones/{zone}/"
	mux.HandleFunc("GET "+z+"instanceGroupManagers", s.listIGMs)
	mux.HandleFunc("GET "+z+"instanceGroupManagers/{name}", s.getIGM)
	mux.HandleFunc("GET "+z+"instanceGroups/{name}", s.getIG)
	get := func(path string, out any) int {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		_ = json.Unmarshal(w.Body.Bytes(), out)
		return w.Code
	}
	var l computev1.InstanceGroupManagerList
	if code := get("/compute/v1/projects/p/zones/us-central1-a/instanceGroupManagers", &l); code != 200 || len(l.Items) != 1 {
		t.Fatalf("list: %d %+v", code, l)
	}
	m := l.Items[0]
	if m.SelfLink != url || m.TargetSize != 2 || m.InstanceGroup != "https://www.googleapis.com/compute/v1/projects/p/zones/us-central1-a/instanceGroups/gke-c-np-1234abcd-grp" {
		t.Fatalf("igm = %+v", m)
	}
	var ig computev1.InstanceGroup
	if code := get("/compute/v1/projects/p/zones/us-central1-a/instanceGroups/gke-c-np-1234abcd-grp", &ig); code != 200 || ig.Size != 2 || ig.Id != m.Id {
		t.Fatalf("ig: %d %+v", code, ig)
	}
	if code := get("/compute/v1/projects/p/zones/us-central1-b/instanceGroupManagers/gke-c-np-1234abcd-grp", &m); code != 404 {
		t.Fatalf("other zone: %d", code)
	}
	l = computev1.InstanceGroupManagerList{}
	if code := get("/compute/v1/projects/p/zones/us-central1-b/instanceGroupManagers", &l); code != 200 || len(l.Items) != 0 {
		t.Fatalf("other zone list: %d %+v", code, l)
	}
}

// TestNamedPorts: setNamedPorts on a node pool group feeds the instance
// group resolver load balancer backends use (FR-LB-005).
func TestNamedPorts(t *testing.T) {
	cfg := config.Defaults()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	env := &emu.Env{Config: &cfg, Store: store.NewMemory(), Clock: clock.Real{}, IDs: emu.NewIDs(true), Log: log,
		Auth: emu.NewPolicyAuthorizer(config.IAMOff, log), Endpoints: emu.NewEndpoints()}
	cmp := compute.New(env).(*compute.Service)
	s := New(env).(*Service)
	env.SetServices(map[string]emu.Service{"compute": cmp, "gke": s})
	s.registerInstanceGroups()

	const grp = "gke-c-np-1234abcd-grp"
	rec := &clusterRecord{Int: clusterInternal{Project: "p", Location: "us-central1", Name: "c",
		Nodes: []nodeRecord{{Name: "n1", Pool: "np", Zone: "us-central1-a", IP: "10.0.0.2"}, {Name: "n2", Pool: "np", Zone: "us-central1-a", IP: "10.0.0.3"}},
		// A stale entry from a deleted pool is dropped on the next write.
		NamedPorts: map[string][]*computev1.NamedPort{"gone-grp": {{Name: "http", Port: 1}}}}}
	rec.setCluster(&containerpb.Cluster{Name: "c", Location: "us-central1", Network: "vpc",
		NodePools: []*containerpb.NodePool{{Name: "np", Etag: "e1",
			InstanceGroupUrls: []string{"https://www.googleapis.com/compute/v1/projects/p/zones/us-central1-a/instanceGroupManagers/" + grp}}}})
	if err := env.Store.Update(func(tx store.Tx) error { return putCluster(tx, rec) }); err != nil {
		t.Fatal(err)
	}
	r := testRouter{http.NewServeMux()}
	if err := cmp.Register(r); err != nil {
		t.Fatal(err)
	}
	h := r.mux
	call := func(method, path string, body any, out any) int {
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, path, rd))
		if out != nil {
			_ = json.Unmarshal(w.Body.Bytes(), out)
		}
		return w.Code
	}
	igPath := "/compute/v1/projects/p/zones/us-central1-a/instanceGroups/" + grp
	var ig computev1.InstanceGroup
	if code := call("GET", igPath, nil, &ig); code != 200 || len(ig.NamedPorts) != 0 || ig.Fingerprint != "e1" {
		t.Fatalf("get: %d %+v", code, ig)
	}
	ctx := context.Background()
	if eps, err := cmp.InstanceGroupEndpoints(ctx, igPath, "http"); err != nil || len(eps) != 0 {
		t.Fatalf("endpoints before named ports: %v %v", eps, err)
	}

	if code := call("POST", igPath+"/setNamedPorts", map[string]any{"namedPorts": []map[string]any{{"name": "Bad", "port": 80}}}, nil); code != 400 {
		t.Fatalf("invalid name: %d", code)
	}
	if code := call("POST", igPath+"/setNamedPorts", map[string]any{"fingerprint": "stale", "namedPorts": []map[string]any{{"name": "http", "port": 30080}}}, nil); code != 412 {
		t.Fatalf("stale fingerprint: %d", code)
	}
	var op computev1.Operation
	if code := call("POST", igPath+"/setNamedPorts", map[string]any{"fingerprint": "e1", "namedPorts": []map[string]any{{"name": "http", "port": 30080}}}, &op); code != 200 || op.OperationType != "setNamedPorts" {
		t.Fatalf("setNamedPorts: %d %+v", code, op)
	}
	if op.Status != "DONE" {
		if code := call("POST", "/compute/v1/projects/p/zones/us-central1-a/operations/"+op.Name+"/wait", nil, &op); code != 200 || op.Status != "DONE" {
			t.Fatalf("wait: %d %+v", code, op)
		}
	}
	ig = computev1.InstanceGroup{}
	if code := call("GET", igPath, nil, &ig); code != 200 || len(ig.NamedPorts) != 1 || ig.NamedPorts[0].Port != 30080 || ig.Fingerprint == "e1" {
		t.Fatalf("after set: %d %+v", code, ig)
	}
	eps, err := cmp.InstanceGroupEndpoints(ctx, igPath, "http")
	if err != nil || len(eps) != 2 || eps[0].IP != "10.0.0.2" || eps[0].Port != 30080 || eps[1].Instance != "n2" {
		t.Fatalf("endpoints: %+v %v", eps, err)
	}
	if _, err := cmp.InstanceGroup(ctx, "projects/p/zones/us-central1-a/instanceGroups/other"); err == nil {
		t.Fatal("unknown group resolved")
	}
	_ = env.Store.View(func(tx store.Tx) error {
		r, _ := getCluster(tx, rec.key())
		if _, ok := r.Int.NamedPorts["gone-grp"]; ok {
			t.Error("stale named ports kept")
		}
		return nil
	})
}

// testRouter serves a service's root routes on a plain mux.
type testRouter struct{ mux *http.ServeMux }

func (r testRouter) Mount(string, []string, http.Handler)  {}
func (r testRouter) Handle(pattern string, h http.Handler) { r.mux.Handle(pattern, h) }
func (r testRouter) GRPC() *grpc.Server                    { return grpc.NewServer() }
func (r testRouter) Fallback(http.Handler)                 {}
