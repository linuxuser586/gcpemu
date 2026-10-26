package build_test

import (
	"context"
	"net"
	"testing"
	"time"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
)

// A Persistent Instance restarted on another gateway port, as on the move
// from 4510 to 4562 (#645), gives each Build it kept a logUrl on the new
// port.
func TestLogURLFollowsTheGateway(t *testing.T) {
	persistent := emutest.WithDataDir(t.TempDir())
	e := start(t, persistent)
	op, _ := e.create(&cloudbuildpb.Build{Steps: []*cloudbuildpb.BuildStep{step("hello", "echo hello")}})
	b, err := op.Wait(e.ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	old := e.inst.Endpoint("gateway")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := e.inst.Internal().Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	e2 := start(t, persistent, emutest.WithConfig(func(c *config.Config) { c.Ports["gateway"] = otherPort(t, old) }))
	want := "http://" + e2.inst.Endpoint("gateway") + "/console/build/" + b.GetName()
	if got := e2.get(b.GetId()).GetLogUrl(); got != want {
		t.Errorf("logUrl after restart = %q, want %q", got, want)
	}
}

// otherPort is a free loopback port that is not the one in addr.
func otherPort(t *testing.T, addr string) int {
	t.Helper()
	l, err := net.Listen("tcp", addr) // holds the old port while another is picked
	if err == nil {
		defer l.Close()
	}
	p, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	return p.Addr().(*net.TCPAddr).Port
}
