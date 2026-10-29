package compute_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	computev1 "google.golang.org/api/compute/v1"
	"google.golang.org/api/option"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/emutestx"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// hostOffers is a subnetwork's entry in the /_emu/v1/host/offers API.
type hostOffers struct {
	Subnetwork  string `json:"subnetwork"`
	HostAddress string `json:"hostAddress"`
	Offers      []struct {
		Port     int    `json:"port"`
		Protocol string `json:"protocol"`
	} `json:"offers"`
	Error string `json:"error"`
}

// admin calls the admin API and decodes the answer into out.
func admin(t *testing.T, inst *emutest.Instance, method, path string, body, out any) int {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, inst.GatewayURL()+path, rd)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 3 * time.Minute}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s %s: %s: %v", method, path, b, err)
		}
	}
	return resp.StatusCode
}

// hostListener serves "hello-from-host" over HTTP on 127.0.0.1 and
// returns its port.
func hostListener(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello-from-host")
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return l.Addr().(*net.TCPAddr).Port
}

// offer offers ports on sp and returns the subnetwork's entry.
func offer(t *testing.T, inst *emutest.Instance, sp string, ports ...string) hostOffers {
	t.Helper()
	var got hostOffers
	if code := admin(t, inst, http.MethodPost, "/_emu/v1/host/offers", map[string]any{"subnetwork": sp, "ports": ports}, &got); code != http.StatusOK {
		t.Fatalf("offer %v on %s: %d %s", ports, sp, code, got.Error)
	}
	return got
}

// get fetches http://addr/ from the workload and returns wget's output.
func get(t *testing.T, rt *runtime.Manager, id, addr string) string {
	t.Helper()
	res, err := rt.Exec(context.Background(), id, []string{"sh", "-c", "wget -qO- -T 3 http://" + addr + "/ 2>&1; true"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return string(res.Stdout) + string(res.Stderr)
}

// TestHostAttachmentOffers is the inward half of the Host attachment (ADR
// 0014): a VPC workload reaches a host listener bound to 127.0.0.1 once
// its port is offered, at the subnetwork's Host address, and only then.
func TestHostAttachmentOffers(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	inst, c, _ := newClients(t)
	customNet(t, c, "vpc")
	subnet(t, c, "vpc", "s", "us-central1", "10.119.0.0/24")
	sp := "projects/" + proj + "/regions/us-central1/subnetworks/s"
	offered, other := hostListener(t), hostListener(t)

	got := offer(t, inst, sp, strconv.Itoa(offered))
	if got.Subnetwork != sp || net.ParseIP(got.HostAddress) == nil || len(got.Offers) != 1 ||
		got.Offers[0].Port != offered || got.Offers[0].Protocol != "tcp" {
		t.Fatalf("offer answered %+v", got)
	}
	var addr hostOffers
	if code := admin(t, inst, http.MethodGet, "/_emu/v1/host/address?subnetwork="+url.QueryEscape(sp), nil, &addr); code != http.StatusOK || addr.HostAddress != got.HostAddress {
		t.Fatalf("host address: %d %+v, want %s", code, addr, got.HostAddress)
	}

	svc, _ := inst.Internal().Env.Lookup("compute")
	vpc := svc.(emu.VPC)
	ctx := context.Background()
	sn, err := vpc.SubnetNetwork(ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	ip, err := vpc.AllocateIP(ctx, sn, "vm")
	if err != nil {
		t.Fatal(err)
	}
	rt, id := workload(t, inst, sn, ip, "")
	at := func(port int) string { return net.JoinHostPort(got.HostAddress, strconv.Itoa(port)) }
	eventually(t, "the offered port", func() (bool, string) {
		out := get(t, rt, id, at(offered))
		return strings.Contains(out, "hello-from-host"), out
	})
	emutestx.RequireRelayStream(t, inst, "the offered port", ":"+strconv.Itoa(offered)+"/tcp")
	if out := get(t, rt, id, at(other)); !strings.Contains(out, "refused") {
		t.Fatalf("a port that is not offered: %s", out)
	}

	var list struct{ Subnetworks []hostOffers }
	admin(t, inst, http.MethodGet, "/_emu/v1/host/offers", nil, &list)
	if len(list.Subnetworks) != 1 || list.Subnetworks[0].HostAddress != got.HostAddress || len(list.Subnetworks[0].Offers) != 1 {
		t.Fatalf("offers listed: %+v", list)
	}

	var after hostOffers
	if code := admin(t, inst, http.MethodDelete, "/_emu/v1/host/offers?subnetwork="+url.QueryEscape(sp)+"&port="+strconv.Itoa(offered), nil, &after); code != http.StatusOK || len(after.Offers) != 0 {
		t.Fatalf("withdraw: %d %+v", code, after)
	}
	if out := get(t, rt, id, at(offered)); !strings.Contains(out, "refused") {
		t.Fatalf("a withdrawn port: %s", out)
	}
}

// TestHostAttachmentSurvivesRestart: a Persistent Instance keeps its Host
// address and its Offered ports across a stop and start (ADR 0014).
func TestHostAttachmentSurvivesRestart(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	dir := t.TempDir()
	persistent := emutest.WithTestDataDir(t, dir)
	ctx := context.Background()
	sp := "projects/" + proj + "/regions/us-central1/subnetworks/s"
	port := hostListener(t)
	var first hostOffers
	{
		inst := emutest.Start(t, []string{"compute"}, persistent)
		c, _ := computev1.NewService(ctx, option.WithEndpoint(inst.GatewayURL()+"/compute/v1/"), option.WithoutAuthentication())
		customNet(t, c, "vpc")
		subnet(t, c, "vpc", "s", "us-central1", "10.118.0.0/24")
		first = offer(t, inst, sp, strconv.Itoa(port))
		svc, _ := inst.Internal().Env.Lookup("compute")
		sn, err := svc.(emu.VPC).SubnetNetwork(ctx, sp)
		if err != nil {
			t.Fatal(err)
		}
		rt, _ := inst.Internal().Env.Containers.Runtime(ctx)
		t.Cleanup(func() { _ = rt.RemoveNetwork(context.Background(), sn.Name) })
		sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := inst.Internal().Shutdown(sctx); err != nil {
			t.Fatal(err)
		}
	}
	inst := emutest.Start(t, []string{"compute"}, persistent)
	var list struct{ Subnetworks []hostOffers }
	admin(t, inst, http.MethodGet, "/_emu/v1/host/offers", nil, &list)
	if len(list.Subnetworks) != 1 || list.Subnetworks[0].HostAddress != first.HostAddress || len(list.Subnetworks[0].Offers) != 1 {
		t.Fatalf("offers after a restart: %+v, want %+v", list, first)
	}
	svc, _ := inst.Internal().Env.Lookup("compute")
	vpc := svc.(emu.VPC)
	sn, err := vpc.SubnetNetwork(ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	ip, err := vpc.AllocateIP(ctx, sn, "vm")
	if err != nil {
		t.Fatal(err)
	}
	if ip == first.HostAddress {
		t.Fatalf("a workload was given the Host address %s", ip)
	}
	rt, id := workload(t, inst, sn, ip, "")
	eventually(t, "the offered port after a restart", func() (bool, string) {
		out := get(t, rt, id, net.JoinHostPort(first.HostAddress, strconv.Itoa(port)))
		return strings.Contains(out, "hello-from-host"), out
	})
}

// TestOfferHeldOnWildcard: where the host routes to the network, offering
// a port that a host process holds on the wildcard address fails, naming
// that bind, rather than half-working (ADR 0014).
func TestOfferHeldOnWildcard(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	inst, c, _ := newClients(t)
	p, err := inst.Internal().Env.Containers.Netplane(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Services(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.Relay() != nil {
		t.Skip("the VM relay holds the Host address: no host bind to collide with")
	}
	customNet(t, c, "vpc")
	subnet(t, c, "vpc", "s", "us-central1", "10.117.0.0/24")
	sp := "projects/" + proj + "/regions/us-central1/subnetworks/s"
	l, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	var got hostOffers
	code := admin(t, inst, http.MethodPost, "/_emu/v1/host/offers", map[string]any{"subnetwork": sp, "ports": []string{port}}, &got)
	if code != http.StatusBadRequest || !strings.Contains(got.Error, "0.0.0.0:"+port) {
		t.Fatalf("offering a port held on 0.0.0.0: %d %q", code, got.Error)
	}
}
