package cli

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/frontend"
	"github.com/linuxuser586/gcpemu/internal/instance"
)

// idleService is a Service that does nothing.
type idleService struct{}

func (idleService) Name() string                { return "dns" }
func (idleService) Register(emu.Router) error   { return nil }
func (idleService) Start(context.Context) error { return nil }
func (idleService) Stop(context.Context) error  { return nil }
func (idleService) Ready() error                { return nil }

// TestALiveInstanceIsConfirmedWithAStalledRuntime is #1025: a runtime that
// connects and never answers does not make a Live Instance Unconfirmed.
func TestALiveInstanceIsConfirmedWithAStalledRuntime(t *testing.T) {
	stalled, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stalled.Close() })
	go func() {
		for {
			c, err := stalled.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	t.Setenv("DOCKER_HOST", "tcp://"+stalled.Addr().String())
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("GCPEMU_ADMIN_TOKEN", "")
	// Below the probe's 3 seconds, as hung endpoints exceed the real limit.
	old := liveCheckTimeout
	liveCheckTimeout = 2 * time.Second
	t.Cleanup(func() { liveCheckTimeout = old })

	cfg := config.Defaults()
	cfg.Ephemeral = true
	cfg.DataDir = t.TempDir()
	cfg.Services = []string{"dns"}
	cfg.Ports["gateway"] = 0
	cfg.Ports[frontend.Endpoint] = 0
	in, err := instance.New(&cfg, map[string]instance.Factory{"dns": func(*emu.Env) emu.Service { return idleService{} }}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	t.Cleanup(func() { _ = in.Shutdown(ctx) })
	if err := in.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if live := checkLive(&cfg); live.state != stateLive {
		t.Fatalf("state = %v (%v), want a Live Instance", live.state, live.err)
	}
}
