package vmrelay_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/internal/vmrelay"
)

// TestControlPortMoves: a network the relay joins whose name sorts
// before the external network's becomes the relay's gateway endpoint, and
// the runtime publishes the control port on a new host port, as the
// first Build's network does (#555). The relay reconnects there, and
// dials and inward connections through it work again.
func TestControlPortMoves(t *testing.T) {
	emutest.RequireRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cl, err := runtime.Detect()
	if err != nil {
		t.Fatal(err)
	}
	if cl.Self() != "" {
		t.Skip("the emulator runs in a container: no VM relay")
	}
	m := &runtime.Manager{Client: cl, InstanceID: "vmrelaymove", Ephemeral: true, Log: slog.Default()}
	t.Cleanup(func() { _ = m.Cleanup(context.Background(), true) })
	network := func(suffix string, internal bool) string {
		name := m.Name(suffix)
		if _, err := m.CreateNetwork(ctx, runtime.NetworkSpec{Name: name, Internal: internal, Labels: m.Labels("test", suffix, "network")}); err != nil {
			t.Fatal(err)
		}
		return name
	}
	svc, ext := network("svc", true), network("ext", false)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "hello-from-host") })}
	go srv.Serve(l)
	defer srv.Close()

	r, err := vmrelay.Start(ctx, m, vmrelay.Options{
		Services: svc, External: ext, Log: slog.Default(),
		Target: func(name string) string {
			if name == "gateway" {
				return l.Addr().String()
			}
			return ""
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(context.Background())
	gw, err := r.Listen(ctx, "gateway", net.JoinHostPort(r.IP(), "8080"), false)
	if err != nil {
		t.Fatal(err)
	}
	ctrl := func() string {
		ct, err := m.InspectContainer(ctx, r.ID())
		if err != nil {
			t.Fatal(err)
		}
		return ct.Ports[strconv.Itoa(vmrelay.ControlPort)+"/tcp"]
	}
	before := ctrl()

	// "build" sorts before "ext" and, like the Build Service's network,
	// is not internal, so it can take the relay's default route.
	build := network("build", false)
	if strings.Compare(build, ext) >= 0 {
		t.Fatalf("%s does not sort before %s", build, ext)
	}
	after := ctrl()
	if after == before {
		t.Fatalf("the runtime kept the control port at %s after %s took the default route", before, build)
	}

	id, err := m.CreateContainer(ctx, runtime.ContainerSpec{
		Name: m.Name("probe"), Image: vmrelay.Image, Entrypoint: []string{"httpd", "-f", "-p", "8080", "-h", "/www"},
		Labels: m.Labels("test", "probe", ""), Networks: []runtime.Attachment{{Network: svc}, {Network: build}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CopyTo(ctx, id, "/", []runtime.File{{Name: "www/index.html", Data: []byte("hello-from-probe")}}); err != nil {
		t.Fatal(err)
	}
	if err := m.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	ct, err := m.InspectContainer(ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	// Through the relay, both ways, within the reconnect's back-off.
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DialContext: r.DialContext}}
	url := "http://" + net.JoinHostPort(ct.IPs[build], "8080") + "/"
	var last error
	for deadline := time.Now().Add(30 * time.Second); ; {
		resp, err := c.Get(url)
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(b) != "hello-from-probe" {
				t.Fatalf("emulator → container: %q", b)
			}
			break
		}
		last = err
		if time.Now().After(deadline) {
			t.Fatalf("emulator → container after the control port moved from %s to %s: %v", before, after, last)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if got := r.Control(); got != after {
		t.Fatalf("Control() = %s, want %s", got, after)
	}
	res, err := m.Exec(ctx, id, []string{"wget", "-qO-", "http://" + gw + "/"}, nil)
	if err != nil || !strings.Contains(string(res.Stdout), "hello-from-host") {
		t.Fatalf("container → gateway: %v %+v stderr=%s", err, res, res.Stderr)
	}
}
