package gke

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/netplane"
	"github.com/linuxuser586/gcpemu/internal/published"
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
		Auth: emu.NewPolicyAuthorizer(config.IAMOff, log), Endpoints: emu.NewEndpoints(), DataDir: t.TempDir()}).(*Service)
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
		mode := map[bool]lbMode{true: lbNodes, false: lbLoopback}[byNodes]
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

			np := netplane.New(nil, nil, "127.0.0.1", s.env.Log)
			lbs := map[string]*lbService{}
			ctx := context.Background()
			if err := s.syncLBServices(ctx, np, rec.key(), mode, nil, lbs); err != nil {
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
			if err := s.syncLBServices(ctx, np, rec.key(), mode, nil, lbs); err != nil {
				t.Fatal(err)
			}
			if ep := s.env.Endpoints.Get("gke:c0/default/web/18276"); ep != "" || len(lbs) != 0 {
				t.Errorf("after the Service went: endpoint %q, lbs %v", ep, lbs)
			}
		})
	}
}

// publishedPorts is the Ports of a container publishing ports, under the
// config of s.
func publishedPorts(s *Service, ports map[int]string) *published.Ports {
	return published.NewPorts(ports, s.env.Config)
}

// On a VM runtime (ADR 0012) a LoadBalancer Service is served on its
// container's published ports and reports the host side: its own port
// when published at the same host port, otherwise a free one of
// --port-range. With none free its ingress stays empty and a warning
// names what to publish; it is served once a port frees.
func TestLBServicesPublished(t *testing.T) {
	s := newUnitService(t)
	var logs bytes.Buffer
	var logMu sync.Mutex
	s.env.Log = slog.New(slog.NewTextHandler(writerFunc(func(b []byte) (int, error) {
		logMu.Lock()
		defer logMu.Unlock()
		return logs.Write(b)
	}), nil))
	publishedHost = "127.0.0.1"
	t.Cleanup(func() { publishedHost = "" })
	own, ranged := freePort(t), freePort(t)
	s.env.Config.PortRange = fmt.Sprintf("%d-%d", ranged, ranged)
	published := map[int]string{own: net.JoinHostPort("127.0.0.1", strconv.Itoa(own)), ranged: "127.0.0.1:40001"}

	rec := &clusterRecord{Int: clusterInternal{Project: "p", Location: "us-central1-a", Name: "c0", HostLB: true,
		Nodes: []nodeRecord{{Name: "n1", IP: "10.128.0.4", ContainerIP: "172.29.0.4"}}}}
	rec.setCluster(&containerpb.Cluster{Name: "c0"})
	if err := s.env.Store.Update(func(tx store.Tx) error { return putCluster(tx, rec) }); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	item := func(name string, port, nodePort int) string {
		return fmt.Sprintf(`{"metadata":{"namespace":"default","name":%q},"spec":{"type":"LoadBalancer","ports":[{"port":%d,"protocol":"TCP","nodePort":%d}]}}`, name, port, nodePort)
	}
	web, api, full := item("web", own, 30080), item("api", 8443, 30081), item("full", 18277, 30082)
	svcs := `{"items":[` + web + `,` + api + `,` + full + `]}`
	patched := map[string][]string{}
	kube := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/services":
			_, _ = io.WriteString(w, svcs)
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/default/services/"):
			var p struct {
				Status struct {
					LoadBalancer struct {
						Ingress []struct{ IP string } `json:"ingress"`
					} `json:"loadBalancer"`
				} `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/namespaces/default/services/"), "/status")
			patched[name] = []string{}
			for _, i := range p.Status.LoadBalancer.Ingress {
				patched[name] = append(patched[name], i.IP)
			}
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected kube request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer kube.Close()
	s.rtFor(rec.key()).kube = &kubeClient{base: kube.URL, hc: kube.Client()}

	np := netplane.New(nil, nil, "127.0.0.1", s.env.Log)
	lbs := map[string]*lbService{}
	defer func() {
		for id, l := range lbs {
			s.closeLB(rec.key(), id, l)
		}
	}()
	ctx := context.Background()
	if err := s.syncLBServices(ctx, np, rec.key(), lbPublished, publishedPorts(s, published), lbs); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if !slices.Equal(patched["web"], []string{"127.0.0.1"}) || !slices.Equal(patched["api"], []string{"127.0.0.1"}) {
		t.Errorf("ingress web %v, api %v; want [127.0.0.1] each", patched["web"], patched["api"])
	}
	if got, ok := patched["full"]; ok {
		t.Errorf("full: ingress patched to %v, want left empty", got)
	}
	mu.Unlock()
	for ep, want := range map[string]string{
		fmt.Sprintf("gke:c0/default/web/%d", own): published[own],
		"gke:c0/default/api/8443":                 "127.0.0.1:40001",
		"gke:c0/default/full/18277":               "",
	} {
		if got := s.env.Endpoints.Get(ep); got != want {
			t.Errorf("endpoint %s = %q, want %q", ep, got, want)
		}
	}
	warnings := func() int {
		logMu.Lock()
		defer logMu.Unlock()
		return strings.Count(logs.String(), "no free published port")
	}
	if n := warnings(); n != 1 || !strings.Contains(logs.String(), "--port-range") {
		t.Errorf("%d warnings, want one naming --port-range:\n%s", n, logs.String())
	}

	// api goes; full takes its port, and warns no more.
	mu.Lock()
	svcs = `{"items":[` + web + `,` + full + `]}`
	mu.Unlock()
	if err := s.syncLBServices(ctx, np, rec.key(), lbPublished, publishedPorts(s, published), lbs); err != nil {
		t.Fatal(err)
	}
	if got := s.env.Endpoints.Get("gke:c0/default/full/18277"); got != "127.0.0.1:40001" {
		t.Errorf("after api went: full's endpoint = %q, want 127.0.0.1:40001", got)
	}
	mu.Lock()
	if !slices.Equal(patched["full"], []string{"127.0.0.1"}) {
		t.Errorf("full: ingress %v, want [127.0.0.1]", patched["full"])
	}
	mu.Unlock()
	if n := warnings(); n != 1 {
		t.Errorf("%d warnings after the retry, want still one", n)
	}
}

// writerFunc is an io.Writer made of a function.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

// freePort returns a TCP port free on 127.0.0.1.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// fakeLAN is the lb Service's emu.LANFrontends: it records the frontends
// registered, and grants the claims set.
type fakeLAN struct {
	emu.Service
	mu     sync.Mutex
	fes    map[string][]emu.LANFrontend
	calls  int
	claims map[string][]string
}

func (f *fakeLAN) SetLANFrontends(owner string, fes []emu.LANFrontend) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fes == nil {
		f.fes = map[string][]emu.LANFrontend{}
	}
	f.fes[owner] = fes
}

func (f *fakeLAN) LANClaims(string) map[string][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.claims
}

func (f *fakeLAN) LoopbackAddrs() []string { return nil }

// lbBindKube is a Kubernetes API with one LoadBalancer Service,
// default/web, on two ports while present, and one Ready node. It keeps
// the ingress patched, and counts patches.
type lbBindKube struct {
	mu      sync.Mutex
	present bool
	ingress []string
	patches int
}

func (k *lbBindKube) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	k.mu.Lock()
	defer k.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/services":
		items := []any{}
		if k.present {
			ingress := []map[string]string{}
			for _, ip := range k.ingress {
				ingress = append(ingress, map[string]string{"ip": ip})
			}
			items = append(items, map[string]any{
				"metadata": map[string]string{"namespace": "default", "name": "web"},
				"spec": map[string]any{"type": "LoadBalancer", "ports": []map[string]any{
					{"port": 18276, "protocol": "TCP", "nodePort": 30080}, {"port": 18277, "protocol": "TCP", "nodePort": 30081}}},
				"status": map[string]any{"loadBalancer": map[string]any{"ingress": ingress}},
			})
		}
		list := map[string]any{"items": items}
		_ = json.NewEncoder(w).Encode(list)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
		_, _ = io.WriteString(w, `{"items":[{"metadata":{"name":"n1"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}`)
	case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/namespaces/default/services/web/status":
		var p struct {
			Status struct {
				LoadBalancer struct {
					Ingress []struct{ IP string } `json:"ingress"`
				} `json:"loadBalancer"`
			} `json:"status"`
		}
		_ = json.NewDecoder(r.Body).Decode(&p)
		k.patches++
		k.ingress = k.ingress[:0]
		for _, i := range p.Status.LoadBalancer.Ingress {
			k.ingress = append(k.ingress, i.IP)
		}
		_, _ = io.WriteString(w, `{}`)
	default:
		http.Error(w, "unexpected", http.StatusNotFound)
	}
}

// lbBindCluster stores cluster c0 on one node, with servicelb unless
// hostLB, and serves its Kubernetes API from k.
func lbBindCluster(t *testing.T, s *Service, hostLB bool, k *lbBindKube) string {
	t.Helper()
	rec := &clusterRecord{Int: clusterInternal{Project: "p", Location: "us-central1-a", Name: "c0", HostLB: hostLB,
		Nodes: []nodeRecord{{Name: "n1", IP: "10.128.0.4", ContainerIP: "172.29.0.4"}}}}
	rec.setCluster(&containerpb.Cluster{Name: "c0"})
	if err := s.env.Store.Update(func(tx store.Tx) error { return putCluster(tx, rec) }); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(k)
	t.Cleanup(srv.Close)
	s.rtFor(rec.key()).kube = &kubeClient{base: srv.URL, hc: srv.Client()}
	return rec.key()
}

// With --lb-bind, each LoadBalancer Service port is registered with the
// lb Service, and the Service's ingress lists the --lb-bind addresses it
// claimed after its own, one per IP in --lb-bind order, until it loses
// them. A cluster running servicelb, which owns the ingress, only
// registers its ports.
func TestLBServicesLBBind(t *testing.T) {
	for _, mode := range []lbMode{lbLoopback, lbNodes, lbServicelb} {
		t.Run(map[lbMode]string{lbLoopback: "host", lbNodes: "containerized", lbServicelb: "servicelb"}[mode], func(t *testing.T) {
			s := newUnitService(t)
			s.env.Config.LBBind = []string{"192.0.2.7", "192.0.2.8"}
			lan := &fakeLAN{}
			s.env.SetServices(map[string]emu.Service{"lb": lan})
			k := &lbBindKube{present: true}
			key := lbBindCluster(t, s, mode != lbServicelb, k)
			np := netplane.New(nil, nil, "127.0.0.1", s.env.Log)
			lbs := map[string]*lbService{}
			defer func() {
				for id, l := range lbs {
					s.closeLB(key, id, l)
				}
			}()
			sync := func() []string {
				t.Helper()
				if err := s.syncLBServices(context.Background(), np, key, mode, nil, lbs); err != nil {
					t.Fatal(err)
				}
				k.mu.Lock()
				defer k.mu.Unlock()
				return slices.Clone(k.ingress)
			}
			const k1, k2 = "gke:p/us-central1-a/c0/default/web/18276", "gke:p/us-central1-a/c0/default/web/18277"
			lan.claims = map[string][]string{k1: {"192.0.2.7:18276", "192.0.2.8:18276"}, k2: {"192.0.2.7:18277"}}
			got := sync()
			fes := lan.fes[key]
			if len(fes) != 2 || fes[0].Key != k1 || fes[0].Port != 18276 || fes[1].Key != k2 || fes[1].Port != 18277 ||
				fes[0].Name != "gke:p/us-central1-a/c0/default/web" || fes[0].Serve == nil {
				t.Fatalf("frontends = %+v, want one per port", fes)
			}
			if mode == lbServicelb {
				if k.patches != 0 {
					t.Fatalf("servicelb cluster patched the ingress to %v", got)
				}
				return
			}
			if len(got) != 3 || got[0] == "192.0.2.7" || got[1] != "192.0.2.7" || got[2] != "192.0.2.8" {
				t.Fatalf("ingress = %v, want its own address, then 192.0.2.7 and 192.0.2.8", got)
			}
			own := got[0]
			if sync(); k.patches != 1 {
				t.Fatalf("%d patches, want 1: nothing changed", k.patches)
			}
			lan.claims = map[string][]string{k2: {"192.0.2.8:18277"}}
			if got := sync(); !slices.Equal(got, []string{own, "192.0.2.8"}) {
				t.Fatalf("ingress = %v, want [%s 192.0.2.8]", got, own)
			}
			lan.claims = nil
			if got := sync(); !slices.Equal(got, []string{own}) {
				t.Fatalf("ingress = %v after losing every claim, want [%s]", got, own)
			}

			k.mu.Lock()
			k.present = false
			k.mu.Unlock()
			sync()
			if fes := lan.fes[key]; len(fes) != 0 {
				t.Fatalf("frontends after the Service went = %+v", fes)
			}
		})
	}
}

// Without --lb-bind, no frontend is registered with the lb Service, and
// the ingress is the Service's own address.
func TestLBServicesNoLBBind(t *testing.T) {
	s := newUnitService(t)
	lan := &fakeLAN{claims: map[string][]string{"gke:p/us-central1-a/c0/default/web/18276": {"192.0.2.7:18276"}}}
	s.env.SetServices(map[string]emu.Service{"lb": lan})
	k := &lbBindKube{present: true}
	key := lbBindCluster(t, s, true, k)
	lbs := map[string]*lbService{}
	defer func() {
		for id, l := range lbs {
			s.closeLB(key, id, l)
		}
	}()
	if err := s.syncLBServices(context.Background(), netplane.New(nil, nil, "127.0.0.1", s.env.Log), key, lbNodes, nil, lbs); err != nil {
		t.Fatal(err)
	}
	if lan.calls != 0 || !slices.Equal(k.ingress, []string{"172.29.0.4"}) {
		t.Fatalf("%d registrations, ingress %v; want none, [172.29.0.4]", lan.calls, k.ingress)
	}
}
