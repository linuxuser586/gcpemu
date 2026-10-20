package frontend

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"syscall"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// The LAN front-end relay serves a privileged --gfe-bind port on Linux
// unless the Instance is a Containerized Instance: a container on the
// host's network is not one, so the relay, on the host's network too,
// serves for it (#564).
func TestRelayable(t *testing.T) {
	noRT := emu.NoRuntime(errors.New("no runtime found"))
	privileged := &net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", syscall.EACCES)}
	for _, tc := range []struct {
		name    string
		setting string
		markers bool
		self    string
		found   bool
		err     error
		want    bool
	}{
		{"on the host", "auto", false, "", false, noRT, true},
		{"runtime: host network", "auto", true, "", true, nil, true},
		{"runtime: bridge container", "auto", true, "abc", true, nil, false},
		{"runtime: can't find its container, set false", "false", true, "", false, nil, true},
		{"runtime: can't find its container", "auto", true, "", false, nil, false},
		{"no runtime: set false", "false", true, "", false, noRT, true},
		{"no runtime: auto", "auto", true, "", false, noRT, false},
		{"no runtime: set true", "true", true, "", false, noRT, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.Containerized = tc.setting
			env := &emu.Env{Config: &cfg, Endpoints: emu.NewEndpoints(), Log: slog.New(slog.DiscardHandler),
				ContainerizedProbe: &emu.ContainerizedProbe{
					Markers: func() bool { return tc.markers },
					Self:    func(context.Context) (string, bool, error) { return tc.self, tc.found, tc.err },
				}}
			f := New(env, nil)
			defer f.Close()
			f.goos = "linux"
			if got := f.relayable(privileged, f.relayContainerized); got != tc.want {
				t.Errorf("relayable = %v, want %v", got, tc.want)
			}
		})
	}
}
