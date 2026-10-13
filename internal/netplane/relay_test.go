package netplane_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	mdns "github.com/miekg/dns"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/netplane"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/internal/vmrelay"
)

// TestVMRelay forces the VM relay (ADR 0005) and checks that it carries
// both ways: a container reaches emulator endpoints over TCP and UDP
// (DNS), and the emulator dials containers, including on a network
// created after the relay started, and serves a host port for one. Reset
// and Close remove the labelled relay container.
func TestVMRelay(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Setenv(vmrelay.Env, "1")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cl, err := runtime.Detect()
	if err != nil {
		t.Fatal(err)
	}
	if cl.Self() != "" {
		t.Skip("the emulator runs in a container: no VM relay")
	}
	m := &runtime.Manager{Client: cl, InstanceID: "nptestrelay", Ephemeral: true, Log: slog.Default()}
	t.Cleanup(func() { _ = m.Cleanup(context.Background(), true) })

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "hello-from-host") })}
	go srv.Serve(l)
	defer srv.Close()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dnsSrv := &mdns.Server{PacketConn: pc, Handler: mdns.HandlerFunc(func(w mdns.ResponseWriter, q *mdns.Msg) {
		a := new(mdns.Msg)
		a.SetReply(q)
		rr, _ := mdns.NewRR(q.Question[0].Name + " 60 IN A 192.0.2.7")
		a.Answer = append(a.Answer, rr)
		_ = w.WriteMsg(a)
	})}
	go dnsSrv.ActivateAndServe()
	defer dnsSrv.Shutdown()
	endpoints := map[string]string{"gateway": l.Addr().String(), "dns": pc.LocalAddr().String()}

	p := netplane.New(m, func() map[string]string { return endpoints }, slog.Default())
	defer p.Close()
	svc, err := p.Services(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r := p.Relay()
	if r == nil {
		t.Fatal("no relay with " + vmrelay.Env + "=1")
	}
	if d, _ := p.RelayDecision(); !d.On || d.Reason != "forced by "+vmrelay.Env {
		t.Fatalf("decision %+v", d)
	}
	ct, err := m.InspectContainer(ctx, r.ID())
	if err != nil || ct.Labels[runtime.LabelInstance] != m.InstanceID {
		t.Fatalf("relay container labels %v, %v", ct.Labels, err)
	}
	gw, err := p.Addr(ctx, "gateway")
	if err != nil || !strings.HasPrefix(gw, r.IP()+":") {
		t.Fatalf("gateway at %s (%v), want on the relay %s", gw, err, r.IP())
	}
	dnsAddr, err := p.Addr(ctx, "dns")
	if err != nil {
		t.Fatal(err)
	}

	// A container on the services network, serving HTTP itself.
	probe := func(name, network string) (id, ip string) {
		id, err := m.CreateContainer(ctx, runtime.ContainerSpec{
			Name: m.Name(name), Image: vmrelay.Image, Entrypoint: []string{"httpd", "-f", "-p", "8080", "-h", "/www"},
			Labels: m.Labels("test", name, ""), Networks: []runtime.Attachment{{Network: network}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := m.CopyTo(ctx, id, "/", []runtime.File{{Name: "www/index.html", Data: []byte("hello-from-" + name)}}); err != nil {
			t.Fatal(err)
		}
		if err := m.StartContainer(ctx, id); err != nil {
			t.Fatal(err)
		}
		c, err := m.InspectContainer(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return id, c.IPs[network]
	}
	id, ip := probe("probe", svc.Name)

	res, err := m.Exec(ctx, id, []string{"wget", "-qO-", "http://" + gw + "/"}, nil)
	if err != nil || !strings.Contains(string(res.Stdout), "hello-from-host") {
		t.Fatalf("container → gateway: %v %+v stderr=%s", err, res, res.Stderr)
	}
	host, port, _ := net.SplitHostPort(dnsAddr)
	res, err = m.Exec(ctx, id, []string{"nslookup", "-port=" + port, "svc.example.test", host}, nil)
	if err != nil || !strings.Contains(string(res.Stdout), "192.0.2.7") {
		t.Fatalf("container → dns: %v %s %s", err, res.Stdout, res.Stderr)
	}

	get := func(url string, dial func(context.Context, string, string) (net.Conn, error)) string {
		t.Helper()
		c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DialContext: dial}}
		var last error
		for range 50 {
			resp, err := c.Get(url)
			if err == nil {
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				return string(b)
			}
			last = err
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("GET %s: %v", url, last)
		return ""
	}
	if got := get("http://"+net.JoinHostPort(ip, "8080")+"/", p.DialContext); got != "hello-from-probe" {
		t.Fatalf("emulator → container: %q", got)
	}

	// A network created after the relay started is joined too.
	late := m.Name("late")
	if _, err := m.CreateNetwork(ctx, runtime.NetworkSpec{Name: late, Internal: true, Labels: m.Labels("test", "late", "network")}); err != nil {
		t.Fatal(err)
	}
	_, lateIP := probe("late", late)
	if got := get("http://"+net.JoinHostPort(lateIP, "8080")+"/", p.DialContext); got != "hello-from-late" {
		t.Fatalf("emulator → container on a later network: %q", got)
	}

	// A host port served through the relay.
	e, err := p.Expose("127.0.0.1:0", net.JoinHostPort(ip, "8080"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if got := get("http://"+e.Addr()+"/", (&net.Dialer{}).DialContext); got != "hello-from-probe" {
		t.Fatalf("host port → container: %q", got)
	}

	st := r.Streams()
	for _, k := range []string{"gateway", "dns/udp", net.JoinHostPort(ip, "8080"), net.JoinHostPort(lateIP, "8080")} {
		if st[k] == 0 {
			t.Errorf("relay carried no stream for %s: %v", k, st)
		}
	}

	// Reset removes the relay; the next use starts one that listens on
	// the same addresses.
	if err := p.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.InspectContainer(ctx, r.ID()); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("relay after Reset: %v", err)
	}
	if again, err := p.Addr(ctx, "gateway"); err != nil || again != gw {
		t.Fatalf("gateway after Reset at %s (%v), want %s", again, err, gw)
	}
	r2 := p.Relay()
	if r2 == nil || r2.ID() == r.ID() {
		t.Fatal("no new relay after Reset")
	}
	res, err = m.Exec(ctx, id, []string{"wget", "-qO-", "http://" + gw + "/"}, nil)
	if err != nil || !strings.Contains(string(res.Stdout), "hello-from-host") {
		t.Fatalf("container → gateway after Reset: %v %+v stderr=%s", err, res, res.Stderr)
	}
	if got := get("http://"+net.JoinHostPort(lateIP, "8080")+"/", p.DialContext); got != "hello-from-late" {
		t.Fatalf("emulator → later network after Reset: %q", got)
	}

	p.Close()
	if _, err := m.InspectContainer(ctx, r2.ID()); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("relay after Close: %v", err)
	}
}
