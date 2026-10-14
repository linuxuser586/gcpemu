package frontend

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strconv"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// vmContainers is an emu.Containers whose runtime is, or is not, a VM
// runtime.
type vmContainers struct {
	emu.Containers
	vm bool
}

func (c vmContainers) Runtime(context.Context) (*runtime.Manager, error) {
	return &runtime.Manager{VM: runtime.VMDecision{On: c.vm}}, nil
}

// A Containerized Instance on a VM runtime keeps the frontend's dynamic
// port out of --port-range, whose published ports LoadBalancer Services
// need (ADR 0012); otherwise the frontend takes one from the range.
func TestStartPortRange(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	free := l.Addr().(*net.TCPAddr).Port
	l.Close()
	for _, tc := range []struct {
		name            string
		inContainer, vm bool
		inRange         bool
	}{
		{"on the host", false, true, true},
		{"containerized", true, false, true},
		{"containerized on a VM runtime", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.PortRange = strconv.Itoa(free) + "-" + strconv.Itoa(free)
			env := &emu.Env{Config: &cfg, Endpoints: emu.NewEndpoints(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
				Containers: vmContainers{vm: tc.vm}}
			f := New(env, nil)
			f.inContainer = func() bool { return tc.inContainer }
			if err := f.Start(context.Background()); err != nil {
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
