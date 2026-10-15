package frontend

import (
	"io"
	"log/slog"
	"net"
	"strconv"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// A Containerized Instance keeps the frontend's dynamic port out of
// --port-range, whose published ports forwarding rules (on any runtime)
// and LoadBalancer Services (on a VM runtime) need (ADR 0012); an
// Instance on the host takes one from the range.
func TestStartPortRange(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	free := l.Addr().(*net.TCPAddr).Port
	l.Close()
	for _, tc := range []struct {
		name        string
		inContainer bool
		inRange     bool
	}{
		{"on the host", false, true},
		{"containerized", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.PortRange = strconv.Itoa(free) + "-" + strconv.Itoa(free)
			env := &emu.Env{Config: &cfg, Endpoints: emu.NewEndpoints(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			f := New(env, nil)
			f.inContainer = func() bool { return tc.inContainer }
			if err := f.Start(); err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			_, p, _ := net.SplitHostPort(f.Addr())
			if got := p == strconv.Itoa(free); got != tc.inRange {
				t.Errorf("frontend at %s; in --port-range %d-%d: %v, want %v", f.Addr(), free, free, got, tc.inRange)
			}
		})
	}
}
