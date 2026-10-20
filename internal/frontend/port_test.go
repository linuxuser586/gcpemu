package frontend

import (
	"context"
	"errors"
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
// Instance on the host, or in a container on the host's network, takes
// one from the range (#502).
func TestStartPortRange(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	free := l.Addr().(*net.TCPAddr).Port
	l.Close()
	noRT := emu.NoRuntime(errors.New("no runtime found"))
	for _, tc := range []struct {
		name    string
		markers bool
		setting string
		self    string
		err     error
		inRange bool
	}{
		{"on the host", false, "auto", "", noRT, true},
		{"runtime: bridge container", true, "auto", "abc", nil, false},
		{"runtime: host network", true, "auto", "", nil, true},
		{"no runtime: guessed containerized", true, "auto", "", noRT, false},
		{"no runtime: set false", true, "false", "", noRT, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.PortRange = strconv.Itoa(free) + "-" + strconv.Itoa(free)
			cfg.Containerized = tc.setting
			env := &emu.Env{Config: &cfg, Endpoints: emu.NewEndpoints(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
				ContainerizedProbe: &emu.ContainerizedProbe{
					Markers: func() bool { return tc.markers },
					Self:    func(context.Context) (string, error) { return tc.self, tc.err },
				}}
			f := New(env, nil)
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
