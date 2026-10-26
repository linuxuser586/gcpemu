package lb_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	computev1 "google.golang.org/api/compute/v1"
	"google.golang.org/api/option"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/services/lb"
)

// startLBBind starts an instance whose forwarding rules are also served on
// the bind addresses (--lb-bind), by default 127.0.0.1 (the rules' own
// listeners are on 127.0.0.x, x >= 2, or 127.0.0.1 too in single-loopback
// mode).
func startLBBind(t *testing.T, bind ...string) *env {
	t.Helper()
	if len(bind) == 0 {
		bind = []string{"127.0.0.1"}
	}
	inst := emutest.Start(t, []string{"lb", "cdn"}, emutest.WithConfig(func(c *config.Config) { c.LBBind = bind }))
	svc, _ := inst.Internal().Env.Lookup("lb")
	svc.(*lb.Service).SetOptions(lb.Options{DirectDial: true})
	c, err := computev1.NewService(context.Background(), option.WithEndpoint(inst.GatewayURL()+"/compute/v1/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, inst: inst, c: c}
}

// lanLB creates an HTTP load balancer in front of origin, without a
// forwarding rule.
func (e *env) lanLB(origin *backend) {
	c := e.c
	e.do(c.Networks.Insert(proj, &computev1.Network{Name: "vpc", AutoCreateSubnetworks: false, ForceSendFields: []string{"AutoCreateSubnetworks"}}).Do())
	e.do(c.NetworkEndpointGroups.Insert(proj, "us-central1-a", &computev1.NetworkEndpointGroup{
		Name: "neg", NetworkEndpointType: "GCE_VM_IP_PORT", Network: "global/networks/vpc", DefaultPort: origin.port()}).Do())
	e.do(c.NetworkEndpointGroups.AttachNetworkEndpoints(proj, "us-central1-a", "neg", &computev1.NetworkEndpointGroupsAttachEndpointsRequest{
		NetworkEndpoints: []*computev1.NetworkEndpoint{{IpAddress: "127.0.0.1", Port: origin.port()}}}).Do())
	e.do(c.BackendServices.Insert(proj, &computev1.BackendService{Name: "be", LoadBalancingScheme: "EXTERNAL_MANAGED", Protocol: "HTTP",
		Backends: []*computev1.Backend{{Group: "zones/us-central1-a/networkEndpointGroups/neg"}}}).Do())
	e.do(c.UrlMaps.Insert(proj, &computev1.UrlMap{Name: "map", DefaultService: "global/backendServices/be"}).Do())
	e.do(c.TargetHttpProxies.Insert(proj, &computev1.TargetHttpProxy{Name: "p", UrlMap: "global/urlMaps/map"}).Do())
}

func (e *env) forwardingRule(name string, port int) {
	e.do(e.c.GlobalForwardingRules.Insert(proj, &computev1.ForwardingRule{Name: name, LoadBalancingScheme: "EXTERNAL_MANAGED",
		PortRange: strconv.Itoa(port), Target: "global/targetHttpProxies/p"}).Do())
}

// TestLBBind: --lb-bind serves forwarding rules on the extra address and
// their real port while the gateway stays on loopback; of two rules on one
// port, one gets the address, and the other takes it over when the first
// is deleted.
func TestLBBind(t *testing.T) {
	e := startLBBind(t)
	origin := newBackend(t)
	e.lanLB(origin)
	port := 8080 // external ALBs take 80, 8080 or 443
	if c, err := net.DialTimeout("tcp", "127.0.0.1:8080", time.Second); err == nil {
		c.Close()
		t.Skip("127.0.0.1:8080 is in use")
	}
	e.forwardingRule("a", port)
	e.forwardingRule("b", port)
	want := "127.0.0.1:" + strconv.Itoa(port)
	exposed := func() (string, string) {
		a, b := e.inst.Endpoint("lb-lan:a"), e.inst.Endpoint("lb-lan:b")
		if a != "" && b != "" {
			t.Fatalf("both rules exposed on %s and %s", a, b)
		}
		if a != "" {
			return "a", a
		}
		return "b", b
	}
	winner, addr := exposed()
	if addr != want {
		t.Fatalf("lb-lan endpoint %q, want %s", addr, want)
	}
	cl := &http.Client{Timeout: 10 * time.Second}
	resp, body := get(t, cl, "http://"+want+"/hello")
	if resp.StatusCode != 200 || !strings.HasSuffix(body, "/hello") {
		t.Fatalf("via --lb-bind: %d %q", resp.StatusCode, body)
	}
	e.do(e.c.GlobalForwardingRules.Delete(proj, winner).Do())
	if next, addr := exposed(); next == winner || addr != want {
		t.Fatalf("after deleting %s: %s exposed on %q", winner, next, addr)
	}
	if resp, _ := get(t, cl, "http://"+want+"/again"); resp.StatusCode != 200 {
		t.Fatalf("after takeover: %d", resp.StatusCode)
	}
}

// TestLBBindRelay: a privileged port the emulator cannot bind on the
// --lb-bind address is served by the lb-lan container on the host network,
// for a forwarding rule or another Service's frontend, and the container
// is removed with the last of them.
func TestLBBindRelay(t *testing.T) {
	emutest.RequireRuntime(t)
	if b, err := os.ReadFile("/proc/sys/net/ipv4/ip_unprivileged_port_start"); err != nil {
		t.Skip("not Linux")
	} else if n, _ := strconv.Atoi(strings.TrimSpace(string(b))); n <= 80 {
		t.Skip("port 80 is unprivileged here: the emulator binds it directly")
	}
	if c, err := net.DialTimeout("tcp", "127.0.0.1:80", time.Second); err == nil {
		c.Close()
		t.Skip("127.0.0.1:80 is in use")
	}
	e := startLBBind(t)
	origin := newBackend(t)
	e.lanLB(origin)
	e.forwardingRule("fr", 80)
	if got := e.inst.Endpoint("lb-lan:fr"); got != "127.0.0.1:80" {
		t.Fatalf("lb-lan endpoint %q, want 127.0.0.1:80", got)
	}
	cl := &http.Client{Timeout: 10 * time.Second}
	resp, body := get(t, cl, "http://127.0.0.1/relayed")
	if resp.StatusCode != 200 || !strings.HasSuffix(body, "/relayed") {
		t.Fatalf("via lb-lan: %d %q", resp.StatusCode, body)
	}
	relays := func() int {
		cts, err := e.inst.Internal().Containers(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, ct := range cts {
			if ct.Service == "lb" && ct.Role == "lan" {
				n++
			}
		}
		return n
	}
	if relays() != 1 {
		t.Fatalf("lb-lan containers = %d, want 1", relays())
	}
	e.do(e.c.GlobalForwardingRules.Delete(proj, "fr").Do())
	if got := e.inst.Endpoint("lb-lan:fr"); got != "" {
		t.Fatalf("lb-lan endpoint %q after delete", got)
	}

	// Another Service's frontend on the port (a GKE LoadBalancer
	// Service) is relayed the same way.
	svc, _ := e.inst.Internal().Env.Lookup("lb")
	fes := svc.(emu.LANFrontends)
	fes.SetLANFrontends("c", []emu.LANFrontend{{Key: "gke:c/default/web/80", Name: "gke:c/default/web", What: "LoadBalancer Service c/default/web", Port: 80,
		Serve: func(ln net.Listener) {
			_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "web") }))
		}}})
	if got := e.inst.Endpoint("lb-lan:gke:c/default/web"); got != "127.0.0.1:80" {
		t.Fatalf("Service lb-lan endpoint %q, want 127.0.0.1:80", got)
	}
	cl = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	if resp, body := get(t, cl, "http://127.0.0.1/"); resp.StatusCode != 200 || body != "web" {
		t.Fatalf("Service via lb-lan: %d %q", resp.StatusCode, body)
	}
	if relays() != 1 {
		t.Fatalf("lb-lan containers = %d, want 1", relays())
	}
	fes.SetLANFrontends("c", nil)
	eventually(t, 30*time.Second, "lb-lan container removed", func() bool { return relays() == 0 })
}

// TestLBBindOtherFrontend: --lb-bind serves a frontend another Service
// registers (a GKE LoadBalancer Service port) under the same claim rule as
// forwarding rules: a rule on its port, whose key is lower, takes the
// address over, the Service's failure is listed for doctor, and the
// Service gets the address back once the rule goes.
func TestLBBindOtherFrontend(t *testing.T) {
	e := startLBBind(t)
	if c, err := net.DialTimeout("tcp", "127.0.0.1:8080", time.Second); err == nil {
		c.Close()
		t.Skip("127.0.0.1:8080 is in use")
	}
	svc, _ := e.inst.Internal().Env.Lookup("lb")
	fes := svc.(emu.LANFrontends)
	const key, ep, addr = "gke:c/default/web/8080", "lb-lan:gke:c/default/web", "127.0.0.1:8080"
	web := emu.LANFrontend{Key: key, Name: "gke:c/default/web", What: "LoadBalancer Service c/default/web", Port: 8080,
		Serve: func(ln net.Listener) {
			_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "web") }))
		}}
	fes.SetLANFrontends("c", []emu.LANFrontend{web})
	// Each request on a new connection: one open to a frontend stays
	// with it when another takes the address over.
	cl := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	if resp, body := get(t, cl, "http://"+addr+"/"); resp.StatusCode != 200 || body != "web" {
		t.Fatalf("via --lb-bind: %d %q, want the Service", resp.StatusCode, body)
	}
	if got := fes.LANClaims("c"); len(got) != 1 || !slices.Equal(got[key], []string{addr}) {
		t.Fatalf("claims = %v, want %s: [%s]", got, key, addr)
	}
	if got := e.inst.Endpoint(ep); got != addr {
		t.Fatalf("%s = %q, want %s", ep, got, addr)
	}

	origin := newBackend(t)
	e.lanLB(origin)
	e.forwardingRule("fr", 8080)
	if resp, body := get(t, cl, "http://"+addr+"/hello"); resp.StatusCode != 200 || !strings.HasSuffix(body, "/hello") {
		t.Fatalf("via --lb-bind with the rule: %d %q, want the rule's backend", resp.StatusCode, body)
	}
	if got := fes.LANClaims("c"); len(got) != 0 {
		t.Fatalf("claims = %v, want none: the rule's key is lower", got)
	}
	if got, rule := e.inst.Endpoint(ep), e.inst.Endpoint("lb-lan:fr"); got != "" || rule != addr {
		t.Fatalf("%s = %q, lb-lan:fr = %q; want the rule's only", ep, got, rule)
	}
	down := e.rest(http.MethodGet, "/_emu/v1/lb/listeners", nil)["lanUnavailable"].([]any)
	if len(down) != 1 {
		t.Fatalf("lanUnavailable = %v, want the Service's", down)
	}
	if f := down[0].(map[string]any); f["frontend"] != web.What || f["address"] != addr || !strings.Contains(f["error"].(string), "forwarding rule fr") {
		t.Fatalf("lanUnavailable = %v", f)
	}

	e.do(e.c.GlobalForwardingRules.Delete(proj, "fr").Do())
	if got := fes.LANClaims("c"); !slices.Equal(got[key], []string{addr}) {
		t.Fatalf("claims after the rule went = %v, want %s", got, addr)
	}
	if resp, body := get(t, cl, "http://"+addr+"/"); resp.StatusCode != 200 || body != "web" {
		t.Fatalf("after the rule went: %d %q, want the Service", resp.StatusCode, body)
	}
	if down := e.rest(http.MethodGet, "/_emu/v1/lb/listeners", nil)["lanUnavailable"].([]any); len(down) != 0 {
		t.Fatalf("lanUnavailable = %v, want none", down)
	}

	fes.SetLANFrontends("c", nil)
	if got := e.inst.Endpoint(ep); got != "" {
		t.Fatalf("%s = %q after unregistering", ep, got)
	}
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		t.Fatalf("%s still served after unregistering", addr)
	}
}
