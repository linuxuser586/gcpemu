package gke

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	mdns "github.com/miekg/dns"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/loopback"
	"github.com/linuxuser586/gcpemu/internal/netplane"
	"github.com/linuxuser586/gcpemu/internal/store"
)

func TestParseHostName(t *testing.T) {
	for _, c := range []struct {
		in   string
		want hostName
		ok   bool
	}{
		{"web.default.svc.cluster.local.", hostName{ns: "default", name: "web"}, true},
		{"web.default.svc.c1.us-central1-a.my-proj.gke.internal.", hostName{key: "my-proj/us-central1-a/c1", ns: "default", name: "web"}, true},
		{"default.svc.cluster.local.", hostName{}, false},
		{"a.web.default.svc.cluster.local.", hostName{}, false},
		{"web.default.pod.c1.l.p.gke.internal.", hostName{}, false},
		{"web.default.svc.c1.l.gke.internal.", hostName{}, false},
		{"svc.cluster.local.", hostName{}, false},
	} {
		got, ok := parseHostName(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("parseHostName(%q) = %+v, %v; want %+v, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// planeOnly is a container runtime that offers only a netplane.
type planeOnly struct {
	emu.Containers
	np *netplane.Plane
}

func (p planeOnly) Netplane(context.Context) (*netplane.Plane, error) { return p.np, nil }

// hostFixture is a GKE service whose clusters' nodes are this host, with
// a ClusterIP relay to the Service range 127.0.0.0/8.
func hostFixture(t *testing.T) *Service {
	t.Helper()
	s := newUnitService(t)
	s.env.Containers = planeOnly{np: netplane.New(nil, nil, "127.0.0.1", s.env.Log)}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	_, cidr, _ := net.ParseCIDR("127.0.0.0/8")
	go serveClusterIPRelay(ln, "secret", cidr)
	prev := clusterIPRelayPort
	clusterIPRelayPort = ln.Addr().(*net.TCPAddr).Port
	t.Cleanup(func() { clusterIPRelayPort = prev })
	return s
}

// addHostCluster adds a running cluster p/us-central1-a/name.
func addHostCluster(t *testing.T, s *Service, name string) (string, *fakeKube) {
	t.Helper()
	rec := &clusterRecord{Int: clusterInternal{Project: "p", Location: "us-central1-a", Name: name, Secret: "secret",
		ServiceCIDR: "127.0.0.0/8", Nodes: []nodeRecord{{Name: name + "-n1", IP: "127.0.0.1"}}}}
	rec.setCluster(&containerpb.Cluster{Name: name, Status: containerpb.Cluster_RUNNING})
	if err := s.env.Store.Update(func(tx store.Tx) error { return putCluster(tx, rec) }); err != nil {
		t.Fatal(err)
	}
	k, kc := newFakeKube(t)
	markRunning(s, rec.key(), kc)
	return rec.key(), k
}

// markRunning makes the cluster key run with the API client kc, as its
// controllers would.
func markRunning(s *Service, key string, kc *kubeClient) {
	rt := s.rtFor(key)
	rt.kube, rt.cancel = kc, func() {}
}

// clusterIPService is a ClusterIP Service default/name on ports.
func clusterIPService(name string, ports ...int) string {
	var ps []string
	for _, p := range ports {
		ps = append(ps, `{"port":`+strconv.Itoa(p)+`,"protocol":"TCP"}`)
	}
	return `{"apiVersion":"v1","kind":"Service","metadata":{"namespace":"default","name":"` + name + `"},"spec":{"type":"ClusterIP","clusterIP":"127.0.0.1","ports":[` + strings.Join(ps, ",") + `]}}`
}

// ask answers a host query.
func ask(s *Service, name string, qtype uint16) ([]mdns.RR, int) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.answerHost(ctx, name, qtype)
}

// askA answers an A query with its one address, or fails.
func askA(t *testing.T, s *Service, name string) string {
	t.Helper()
	rrs, rcode := ask(s, name, mdns.TypeA)
	if rcode != mdns.RcodeSuccess || len(rrs) != 1 {
		t.Fatalf("%s: rcode %s, answer %v", name, mdns.RcodeToString[rcode], rrs)
	}
	a := rrs[0].(*mdns.A)
	if a.Hdr.Name != name || a.Hdr.Ttl != hostSvcTTL {
		t.Errorf("%s: record %v", name, a)
	}
	return a.A.String()
}

// echoes reports whether addr echoes what it is sent.
func echoes(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return false
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, "ping"); err != nil {
		return false
	}
	b := make([]byte, 4)
	_, err = io.ReadFull(c, b)
	return err == nil && string(b) == "ping"
}

func hostJSON(t *testing.T, s *Service) hostServicesJSON {
	t.Helper()
	b, _ := json.Marshal(s.hostServices())
	var v hostServicesJSON
	_ = json.Unmarshal(b, &v)
	return v
}

// Host clients reach a cluster's ClusterIP Services by name (ADR 0014): at
// a Loopback alias the Service gets on the first query and keeps until it
// is deleted, across restarts, on which each port reaches the Service
// through a node's ClusterIP relay. The short name answers only while one
// cluster has the Service.
func TestHostServices(t *testing.T) {
	if !loopback.Aliases() {
		t.Skip("needs loopback aliases (sudo gcpemu loopback install)")
	}
	s := hostFixture(t)
	ctx := context.Background()
	k1, kube1 := addHostCluster(t, s, "c1")
	k2, kube2 := addHostCluster(t, s, "c2")
	echo := echoServer(t)
	_, port, _ := net.SplitHostPort(echo)
	p, _ := strconv.Atoi(port)
	kube1.put(clusterIPService("web", p))

	const q1 = "web.default.svc.c1.us-central1-a.p.gke.internal."
	ip := askA(t, s, q1)
	if !strings.HasPrefix(ip, "127.0.0.") || ip == "127.0.0.1" {
		t.Fatalf("alias = %s", ip)
	}
	if !echoes(net.JoinHostPort(ip, port)) {
		t.Errorf("%s:%s does not reach the Service", ip, port)
	}
	if got := askA(t, s, q1); got != ip {
		t.Errorf("second query: %s, want %s", got, ip)
	}
	if got := askA(t, s, "web.default.svc.cluster.local."); got != ip {
		t.Errorf("short name: %s, want %s", got, ip)
	}
	if !slices.Contains(s.LoopbackAddrs(), ip) {
		t.Errorf("LoopbackAddrs = %v, want %s held", s.LoopbackAddrs(), ip)
	}
	np, _ := s.env.Containers.Netplane(ctx)
	if err := s.syncHostServices(ctx, np, k1); err != nil {
		t.Fatal(err)
	}
	if !echoes(net.JoinHostPort(ip, port)) {
		t.Errorf("a sync stopped serving %s:%s", ip, port)
	}
	if v := hostJSON(t, s); len(v.Services) != 1 || v.Services[0].Address != ip || v.Services[0].ShortName != "web.default.svc.cluster.local" ||
		v.Services[0].Name != strings.TrimSuffix(q1, ".") || !slices.Equal(v.Services[0].Ports, []int{p}) {
		t.Errorf("host services = %+v", v)
	}

	// A second cluster with the Service takes the short name away.
	kube2.put(clusterIPService("web", p))
	for _, k := range []string{k1, k2} {
		if err := s.syncHostServices(ctx, np, k); err != nil {
			t.Fatal(err)
		}
	}
	if _, rcode := ask(s, "web.default.svc.cluster.local.", mdns.TypeA); rcode != mdns.RcodeNameError {
		t.Errorf("short name with two clusters: %s", mdns.RcodeToString[rcode])
	}
	ip2 := askA(t, s, "web.default.svc.c2.us-central1-a.p.gke.internal.")
	if ip2 == ip {
		t.Errorf("both clusters' Services at %s", ip)
	}
	if v := hostJSON(t, s); len(v.Collisions) != 1 || v.Collisions[0].Name != "web.default.svc.cluster.local" || len(v.Collisions[0].Clusters) != 2 {
		t.Errorf("collisions = %+v", v.Collisions)
	}
	kube2.remove("/api/v1/namespaces/default/services/web")
	if err := s.syncHostServices(ctx, np, k2); err != nil {
		t.Fatal(err)
	}
	if got := askA(t, s, "web.default.svc.cluster.local."); got != ip {
		t.Errorf("short name after the second cluster's went: %s, want %s", got, ip)
	}
	if slices.Contains(s.LoopbackAddrs(), ip2) {
		t.Errorf("%s still held after its Service went", ip2)
	}

	// A restart serves the Service again at the same alias before any
	// query.
	s.closeHostServices(k1)
	if echoes(net.JoinHostPort(ip, port)) {
		t.Fatal("still served after close")
	}
	r := hostFixture(t)
	r.env.Store = s.env.Store
	markRunning(r, k1, s.rtFor(k1).client())
	np, _ = r.env.Containers.Netplane(ctx)
	// While another socket holds a port on the stored alias, the Service
	// waits for it rather than moving.
	held, err := net.Listen("tcp", net.JoinHostPort(ip, port))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.syncHostServices(ctx, np, k1); err == nil {
		t.Errorf("served with its port held")
	}
	if got := r.hostRecords(k1 + "/")[k1+"/default/web"]; got != ip {
		t.Errorf("stored alias moved to %q while its port was held", got)
	}
	held.Close()
	if err := r.syncHostServices(ctx, np, k1); err != nil {
		t.Fatal(err)
	}
	if !echoes(net.JoinHostPort(ip, port)) {
		t.Errorf("not served at %s after a restart", ip)
	}
	if got := askA(t, r, q1); got != ip {
		t.Errorf("after a restart: %s, want %s", got, ip)
	}

	// Deleting the Service releases its alias.
	kube1.remove("/api/v1/namespaces/default/services/web")
	if err := r.syncHostServices(ctx, np, k1); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(r.LoopbackAddrs(), ip) || len(hostJSON(t, r).Services) != 0 {
		t.Errorf("alias %s still held: %v", ip, r.LoopbackAddrs())
	}
	if echoes(net.JoinHostPort(ip, port)) {
		t.Errorf("still served after the Service went")
	}
	r.closeHostServices(k1)
	r.closeHostServices(k2)
}

// Names that are not answered, ExternalName Services, and the SERVFAIL of
// a host without aliases or an exhausted pool.
func TestHostServicesAnswers(t *testing.T) {
	s := hostFixture(t)
	_, kube := addHostCluster(t, s, "c1")
	kube.put(`{"apiVersion":"v1","kind":"Service","metadata":{"namespace":"default","name":"headless"},"spec":{"type":"ClusterIP","clusterIP":"None","ports":[{"port":80}]}}`)
	kube.put(`{"apiVersion":"v1","kind":"Service","metadata":{"namespace":"default","name":"ext"},"spec":{"type":"ExternalName","externalName":"Example.COM"}}`)
	kube.put(clusterIPService("web", 18080))

	for _, name := range []string{"nope.default.svc.cluster.local.", "headless.default.svc.cluster.local.",
		"web.default.svc.c9.us-central1-a.p.gke.internal.", "a.b.c.svc.cluster.local."} {
		if rrs, rcode := ask(s, name, mdns.TypeA); rcode != mdns.RcodeNameError || len(rrs) != 0 {
			t.Errorf("%s: %s %v, want NXDOMAIN", name, mdns.RcodeToString[rcode], rrs)
		}
	}
	rrs, rcode := ask(s, "ext.default.svc.cluster.local.", mdns.TypeA)
	if c, ok := firstCNAME(rrs); rcode != mdns.RcodeSuccess || !ok || c.Target != "example.com." {
		t.Errorf("ExternalName: %s %v", mdns.RcodeToString[rcode], rrs)
	}
	if rrs, rcode := ask(s, "web.default.svc.cluster.local.", mdns.TypeAAAA); rcode != mdns.RcodeSuccess || len(rrs) != 0 {
		t.Errorf("AAAA: %s %v, want NODATA", mdns.RcodeToString[rcode], rrs)
	}
	if len(hostJSON(t, s).Services) != 0 {
		t.Errorf("an alias without an A query")
	}

	prev := hostAliases
	t.Cleanup(func() { hostAliases = prev })
	hostAliases = func() bool { return false }
	if _, rcode := ask(s, "web.default.svc.cluster.local.", mdns.TypeA); rcode != mdns.RcodeServerFailure {
		t.Errorf("without aliases: %s, want SERVFAIL", mdns.RcodeToString[rcode])
	}
	if v := hostJSON(t, s); len(v.Unserved) != 1 || !strings.Contains(v.Unserved[0].Reason, "loopback install") || v.PoolExhausted {
		t.Errorf("without aliases: %+v", v)
	}

	hostAliases = func() bool { return true }
	s.loopMu.Lock()
	for x := 2; x <= 254; x++ {
		s.loops["p/us-central1-a/c1/default/lb"+strconv.Itoa(x)] = "127.0.0." + strconv.Itoa(x)
	}
	s.loopMu.Unlock()
	if v := hostJSON(t, s); !v.PoolExhausted {
		t.Errorf("a full pool not reported before a query: %+v", v)
	}
	if _, rcode := ask(s, "web.default.svc.cluster.local.", mdns.TypeA); rcode != mdns.RcodeServerFailure {
		t.Errorf("pool exhausted: %s, want SERVFAIL", mdns.RcodeToString[rcode])
	}
	if v := hostJSON(t, s); !v.PoolExhausted || len(v.Unserved) != 1 {
		t.Errorf("pool exhausted: %+v", v)
	}
}

func firstCNAME(rrs []mdns.RR) (*mdns.CNAME, bool) {
	if len(rrs) == 0 {
		return nil, false
	}
	c, ok := rrs[0].(*mdns.CNAME)
	return c, ok
}

// A deleted cluster gives up its Services' aliases and names.
func TestHostServicesForgetCluster(t *testing.T) {
	s := hostFixture(t)
	k1, _ := addHostCluster(t, s, "c1")
	if err := s.env.Store.Update(func(tx store.Tx) error {
		return store.PutJSON(tx, nsHostSvcs, k1+"/default/web", hostSvcRecord{IP: "127.0.0.9"})
	}); err != nil {
		t.Fatal(err)
	}
	s.hostMu.Lock()
	s.setHostNamesLocked(k1, map[string]bool{"default/web": true})
	s.hostMu.Unlock()
	if !slices.Contains(s.LoopbackAddrs(), "127.0.0.9") {
		t.Fatalf("LoopbackAddrs = %v, want the stored alias", s.LoopbackAddrs())
	}
	s.forgetHostCluster(k1)
	if len(s.LoopbackAddrs()) != 0 || len(hostJSON(t, s).Services) != 0 {
		t.Errorf("after the cluster went: %v, %+v", s.LoopbackAddrs(), hostJSON(t, s))
	}
	s.hostMu.Lock()
	defer s.hostMu.Unlock()
	if keys := slices.Collect(func(yield func(string) bool) {
		for k := range s.hostNames {
			yield(k)
		}
	}); len(keys) != 0 {
		sort.Strings(keys)
		t.Errorf("names kept for %v", keys)
	}
}

// A cluster that stops running drops out of short-name lookups and of the
// collisions, alike; its qualified names get SERVFAIL.
func TestHostServicesStoppedCluster(t *testing.T) {
	s := hostFixture(t)
	k1, kube1 := addHostCluster(t, s, "c1")
	k2, kube2 := addHostCluster(t, s, "c2")
	for _, k := range []*fakeKube{kube1, kube2} {
		k.put(`{"apiVersion":"v1","kind":"Service","metadata":{"namespace":"default","name":"ext"},"spec":{"type":"ExternalName","externalName":"example.com"}}`)
	}
	np, _ := s.env.Containers.Netplane(context.Background())
	for _, k := range []string{k1, k2} {
		if err := s.syncHostServices(context.Background(), np, k); err != nil {
			t.Fatal(err)
		}
	}
	if _, rcode := ask(s, "ext.default.svc.cluster.local.", mdns.TypeA); rcode != mdns.RcodeNameError || len(hostJSON(t, s).Collisions) != 1 {
		t.Fatalf("two clusters: %s, %+v", mdns.RcodeToString[rcode], hostJSON(t, s))
	}
	s.stopRT(s.rtFor(k2))
	s.closeHostServices(k2)
	if rrs, rcode := ask(s, "ext.default.svc.cluster.local.", mdns.TypeA); rcode != mdns.RcodeSuccess || len(rrs) != 1 || len(hostJSON(t, s).Collisions) != 0 {
		t.Errorf("c2 stopped: %s %v, %+v", mdns.RcodeToString[rcode], rrs, hostJSON(t, s))
	}
	if _, rcode := ask(s, "ext.default.svc.c2.us-central1-a.p.gke.internal.", mdns.TypeA); rcode != mdns.RcodeServerFailure {
		t.Errorf("qualified name of a stopped cluster: %s", mdns.RcodeToString[rcode])
	}
}
