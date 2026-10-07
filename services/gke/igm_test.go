package gke

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"cloud.google.com/go/container/apiv1/containerpb"
	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
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
