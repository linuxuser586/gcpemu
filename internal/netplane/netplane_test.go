package netplane_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/netplane"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// TestServicesNetwork checks that a container on the services network
// reaches a host endpoint through the forwarder on the network gateway.
func TestServicesNetwork(t *testing.T) {
	emutest.RequireRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cl, err := runtime.Detect()
	if err != nil {
		t.Fatal(err)
	}
	m := &runtime.Manager{Client: cl, InstanceID: "nptest" + strings.ToLower(t.Name()[4:8]), Ephemeral: true, Log: slog.Default()}
	t.Cleanup(func() { _ = m.Cleanup(context.Background(), true) })

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "hello-from-host") })}
	go srv.Serve(l)
	defer srv.Close()

	p := netplane.New(m, func() map[string]string { return map[string]string{"gateway": l.Addr().String()} }, slog.Default())
	defer p.Close()
	svc, err := p.Services(ctx)
	if err != nil {
		t.Fatal(err)
	}
	addr, err := p.Addr(ctx, "gateway")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(addr, svc.Gateway+":") {
		t.Fatalf("forwarder %s not on gateway %s", addr, svc.Gateway)
	}
	const img = "rancher/k3s:v1.36.5-k3s1"
	if err := m.EnsureImage(ctx, img, false); err != nil {
		t.Fatal(err)
	}
	id, err := m.CreateContainer(ctx, runtime.ContainerSpec{
		Name: m.Name("probe"), Image: img, Entrypoint: []string{"sleep", "300"},
		Labels: m.Labels("test", "probe", ""), Networks: []runtime.Attachment{{Network: svc.Name}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CopyTo(ctx, id, "/tmp", []runtime.File{{Name: "x.txt", Data: []byte("copied")}}); err != nil {
		t.Fatal(err)
	}
	if err := m.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	res, err := m.Exec(ctx, id, []string{"wget", "-qO-", "http://" + addr + "/"}, nil)
	if err != nil || !strings.Contains(string(res.Stdout), "hello-from-host") {
		t.Fatalf("exec wget: %v %+v stderr=%s", err, res, res.Stderr)
	}
	res, err = m.Exec(ctx, id, []string{"cat"}, []byte("stdin-ok"))
	if err != nil || string(res.Stdout) != "stdin-ok" {
		t.Fatalf("exec stdin: %v %q", err, res.Stdout)
	}
	b, err := m.CopyFrom(ctx, id, "/tmp/x.txt")
	if err != nil || string(b) != "copied" {
		t.Fatalf("copy from: %v %q", err, b)
	}
	if res, _ := m.Exec(ctx, id, []string{"wget", "-qO-", "-T", "2", "http://1.1.1.1/"}, nil); res.ExitCode == 0 {
		t.Error("services network should have no egress")
	}
}
