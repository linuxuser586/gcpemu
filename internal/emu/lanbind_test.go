package emu

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/lanbind"
)

// LANBind goes by the Containerized Instance decision: Bridge only when
// the runtime finds the emulator's own container, Host when the decision
// is off, and Unconfirmed when it is on without the runtime's answer
// (#564).
func TestLANBind(t *testing.T) {
	noRT := NoRuntime(errors.New("no runtime found"))
	for _, tc := range []struct {
		name    string
		setting string
		markers bool
		self    string
		found   bool
		err     error
		want    lanbind.Where
	}{
		{"on the host", "auto", false, "", false, noRT, lanbind.Host},
		{"runtime: bridge container", "auto", true, "abc", true, nil, lanbind.Bridge},
		{"runtime: bridge container, set false", "false", true, "abc", true, nil, lanbind.Bridge},
		{"runtime: host network", "auto", true, "", true, nil, lanbind.Host},
		{"runtime: host network, set true", "true", true, "", true, nil, lanbind.Host},
		{"runtime: can't find its container", "auto", true, "", false, nil, lanbind.Unconfirmed},
		{"runtime: can't find its container, set false", "false", true, "", false, nil, lanbind.Host},
		{"no runtime: set false", "false", true, "", false, noRT, lanbind.Host},
		{"no runtime: auto", "auto", true, "", false, noRT, lanbind.Unconfirmed},
		{"no runtime: set true", "true", true, "", false, noRT, lanbind.Unconfirmed},
		{"runtime not reachable yet: set false", "false", true, "", false, errors.New("timeout"), lanbind.Host},
		{"runtime not reachable yet: auto", "auto", true, "", false, errors.New("timeout"), lanbind.Unconfirmed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.Containerized = tc.setting
			e := &Env{Config: &cfg, Log: slog.New(slog.DiscardHandler), ContainerizedProbe: &ContainerizedProbe{
				Markers: func() bool { return tc.markers },
				Self:    func(context.Context) (string, bool, error) { return tc.self, tc.found, tc.err },
			}}
			if got := e.LANBind(context.Background()); got != tc.want {
				t.Errorf("LANBind = %v, want %v", got, tc.want)
			}
		})
	}
}
