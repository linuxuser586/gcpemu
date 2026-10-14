package gke

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/netplane"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// holder is a peer Service that holds loopback addresses.
type holder struct {
	emu.Service
	addrs []string
}

func (h holder) LoopbackAddrs() []string { return h.addrs }

func newUnitService(t *testing.T) *Service {
	t.Helper()
	cfg := config.Defaults()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(&emu.Env{Config: &cfg, Store: store.NewMemory(), Clock: clock.Real{}, IDs: emu.NewIDs(true), Log: log,
		Auth: emu.NewPolicyAuthorizer(config.IAMOff, log), Endpoints: emu.NewEndpoints()}).(*Service)
}

// A LoadBalancer Service's loopback address skips those forwarding rules,
// Cloud SQL instances and other Services hold, and starts at its previous
// one.
func TestLBLoopCandidates(t *testing.T) {
	s := newUnitService(t)
	const name = "p/us-central1-a/c1/default/web"
	first := s.loopCandidates(name, "")[0]
	other := s.loopCandidates("p/us-central1-a/c1/default/api", "")[0]
	s.env.SetServices(map[string]emu.Service{
		"lb":  holder{addrs: []string{first}},
		"sql": holder{addrs: []string{"127.0.0.9"}},
	})
	s.loops["p/us-central1-a/c1/default/api"] = other
	got := s.loopCandidates(name, "")
	held := map[string]bool{first: true, "127.0.0.9": true, other: true}
	for a := range held {
		if slices.Contains(got, a) {
			t.Errorf("candidates include %s, which is held", a)
		}
	}
	if slices.Contains(got, "127.0.0.1") || len(got) != 253-len(held) {
		t.Errorf("%d candidates, want 127.0.0.2-254 less %d held", len(got), len(held))
	}
	if got := s.loopCandidates(name, "127.0.0.200"); got[0] != "127.0.0.200" || slices.Contains(got[1:], "127.0.0.200") {
		t.Errorf("with prev: candidates start %v", got[:2])
	}
	if got := s.LoopbackAddrs(); !slices.Equal(got, []string{other}) {
		t.Errorf("LoopbackAddrs = %v, want [%s]", got, other)
	}
}

func TestLBEnvVars(t *testing.T) {
	s := newUnitService(t)
	got := s.EnvVars("127.0.0.1:4000", map[string]string{
		"gke:c-1/default/web/80": "127.0.0.7:80",
		"lb:fr":                  "127.0.0.8:80",
	})
	if got["GCPEMU_GKE_C_1_DEFAULT_WEB_80"] != "127.0.0.7:80" || len(got) != 3 {
		t.Errorf("EnvVars = %v", got)
	}
}

// Without a container runtime the controller leaves LoadBalancer Services
// alone.
func TestLBServicesLoopWithoutRuntime(t *testing.T) {
	s := newUnitService(t)
	s.lbServicesLoop(context.Background(), "p/l/c")
}

// The Ready nodes' Fallback-range addresses, sorted; a node not on a
// Translated network gives its IP.
func TestLBNodeIngress(t *testing.T) {
	nodes := []nodeRecord{
		{Name: "n1", IP: "10.128.0.4", ContainerIP: "172.29.0.4"},
		{Name: "n2", IP: "10.128.0.3", ContainerIP: "172.29.0.3"},
		{Name: "n3", IP: "10.128.0.5", ContainerIP: "172.29.0.5"},
		{Name: "n4", IP: "10.200.0.9"},
	}
	got := nodeIngress(nodes, map[string]bool{"n1": true, "n2": true, "n4": true})
	if want := []string{"10.200.0.9", "172.29.0.3", "172.29.0.4"}; !slices.Equal(got, want) {
		t.Errorf("nodeIngress = %v, want %v", got, want)
	}
	if got := nodeIngress(nodes, nil); got != nil {
		t.Errorf("no Ready node: nodeIngress = %v, want none", got)
	}
}

// For a Containerized Instance (#276) a LoadBalancer Service reports its
// Ready nodes' Fallback-range addresses and opens no listener; otherwise
// it is served on a loopback address. Endpoints follow, and go with the
// Service.
func TestLBServicesSync(t *testing.T) {
	for _, byNodes := range []bool{true, false} {
		t.Run(map[bool]string{true: "containerized", false: "host"}[byNodes], func(t *testing.T) {
			s := newUnitService(t)
			rec := &clusterRecord{Int: clusterInternal{Project: "p", Location: "us-central1-a", Name: "c0", HostLB: true,
				Nodes: []nodeRecord{
					{Name: "n1", IP: "10.128.0.4", ContainerIP: "172.29.0.4"},
					{Name: "n2", IP: "10.128.0.3", ContainerIP: "172.29.0.3"},
					{Name: "n3", IP: "10.128.0.5", ContainerIP: "172.29.0.5"},
				}}}
			rec.setCluster(&containerpb.Cluster{Name: "c0"})
			if err := s.env.Store.Update(func(tx store.Tx) error { return putCluster(tx, rec) }); err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			svcs := `{"items":[
				{"metadata":{"namespace":"default","name":"web"},"spec":{"type":"LoadBalancer","ports":[{"port":18276,"protocol":"TCP","nodePort":30080}]}},
				{"metadata":{"namespace":"default","name":"internal"},"spec":{"type":"ClusterIP","ports":[{"port":80}]}}]}`
			var patched []string
			kube := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/services":
					_, _ = io.WriteString(w, svcs)
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
					_, _ = io.WriteString(w, `{"items":[
						{"metadata":{"name":"n1"},"status":{"conditions":[{"type":"Ready","status":"True"}]}},
						{"metadata":{"name":"n2"},"status":{"conditions":[{"type":"Ready","status":"True"}]}},
						{"metadata":{"name":"n3"},"status":{"conditions":[{"type":"Ready","status":"False"}]}}]}`)
				case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/namespaces/default/services/web/status":
					var p struct {
						Status struct {
							LoadBalancer struct {
								Ingress []struct{ IP string } `json:"ingress"`
							} `json:"loadBalancer"`
						} `json:"status"`
					}
					_ = json.NewDecoder(r.Body).Decode(&p)
					patched = patched[:0]
					for _, i := range p.Status.LoadBalancer.Ingress {
						patched = append(patched, i.IP)
					}
					_, _ = io.WriteString(w, `{}`)
				default:
					t.Errorf("unexpected kube request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer kube.Close()
			s.rtFor(rec.key()).kube = &kubeClient{base: kube.URL, hc: kube.Client()}

			np := netplane.New(nil, nil, s.env.Log)
			lbs := map[string]*lbService{}
			ctx := context.Background()
			if err := s.syncLBServices(ctx, np, rec.key(), byNodes, lbs); err != nil {
				t.Fatal(err)
			}
			l := lbs["default/web"]
			if l == nil || len(lbs) != 1 {
				t.Fatalf("lbs = %v, want default/web only", lbs)
			}
			ep := s.env.Endpoints.Get("gke:c0/default/web/18276")
			mu.Lock()
			got := slices.Clone(patched)
			mu.Unlock()
			if byNodes {
				if want := []string{"172.29.0.3", "172.29.0.4"}; !slices.Equal(got, want) {
					t.Errorf("ingress = %v, want %v", got, want)
				}
				if ep != "172.29.0.3:18276" {
					t.Errorf("endpoint = %q, want 172.29.0.3:18276", ep)
				}
				if len(l.ex) != 0 || len(s.LoopbackAddrs()) != 0 {
					t.Errorf("listeners %d, loopback addresses %v; want none", len(l.ex), s.LoopbackAddrs())
				}
			} else {
				if len(got) != 1 || !strings.HasPrefix(got[0], "127.0.0.") {
					t.Errorf("ingress = %v, want one loopback address", got)
				}
				if host, _, _ := net.SplitHostPort(ep); host != got[0] || len(l.ex) != 1 {
					t.Errorf("endpoint = %q, listeners %d; want one at the ingress", ep, len(l.ex))
				}
			}

			mu.Lock()
			svcs = `{"items":[]}`
			mu.Unlock()
			if err := s.syncLBServices(ctx, np, rec.key(), byNodes, lbs); err != nil {
				t.Fatal(err)
			}
			if ep := s.env.Endpoints.Get("gke:c0/default/web/18276"); ep != "" || len(lbs) != 0 {
				t.Errorf("after the Service went: endpoint %q, lbs %v", ep, lbs)
			}
		})
	}
}
