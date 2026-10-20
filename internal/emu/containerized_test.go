package emu

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/config"
)

// Containerized asks the runtime only when the Instance may be in a
// container, goes by its answer when there is one, and otherwise by
// --containerized and the marker files; the guess and a conflict are
// each logged once (#502).
func TestContainerized(t *testing.T) {
	noRT := NoRuntime(errors.New("no runtime found"))
	for _, tc := range []struct {
		name    string
		setting string
		markers bool
		self    string
		found   bool
		err     error
		want    bool
		asked   bool
		log     string
	}{
		{"on the host", "auto", false, "", false, nil, false, false, ""},
		{"runtime: bridge container", "auto", true, "abc", true, nil, true, true, ""},
		{"runtime: host network", "auto", true, "", true, nil, false, true, ""},
		{"runtime: bridge container, set false", "false", true, "abc", true, nil, true, true, "disagrees with the container runtime"},
		{"runtime: can't find its container", "auto", true, "", false, nil, true, true, "GCPEMU_SELF_CONTAINER"},
		{"runtime: can't find its container, set true", "true", false, "", false, nil, true, true, ""},
		{"no runtime: set false", "false", true, "", false, noRT, false, true, ""},
		{"no runtime: auto", "auto", true, "", false, noRT, true, true, "no container runtime socket"},
		{"runtime not reachable yet: auto", "auto", true, "", false, errors.New("timeout"), true, true, ""},
		{"no markers, set true, no runtime", "true", false, "", false, noRT, true, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.Containerized = tc.setting
			var buf bytes.Buffer
			asked := false
			e := &Env{Config: &cfg, Log: slog.New(slog.NewTextHandler(&buf, nil)), ContainerizedProbe: &ContainerizedProbe{
				Markers: func() bool { return tc.markers },
				Self: func(context.Context) (string, bool, error) {
					asked = true
					return tc.self, tc.found, tc.err
				},
			}}
			for range 2 {
				if got := e.Containerized(context.Background()); got != tc.want {
					t.Errorf("Containerized = %v, want %v", got, tc.want)
				}
			}
			if asked != tc.asked {
				t.Errorf("runtime asked: %v, want %v", asked, tc.asked)
			}
			logged := buf.String()
			if tc.log == "" && strings.Contains(logged, "WARN") {
				t.Errorf("warned: %s", logged)
			}
			if tc.log != "" && (strings.Count(logged, "WARN") != 1 || !strings.Contains(logged, tc.log)) {
				t.Errorf("log %q, want one warning naming %q", logged, tc.log)
			}
		})
	}
}

// A runtime that doesn't answer within ctx is no answer.
func TestContainerizedRuntimeSlow(t *testing.T) {
	cfg := config.Defaults()
	cfg.Containerized = "false"
	block := make(chan struct{})
	defer close(block)
	e := &Env{Config: &cfg, Log: slog.New(slog.DiscardHandler), ContainerizedProbe: &ContainerizedProbe{
		Markers: func() bool { return true },
		Self: func(context.Context) (string, bool, error) {
			<-block
			return "abc", true, nil
		},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if e.Containerized(ctx) {
		t.Error("Containerized = true with no answer and --containerized=false")
	}
}
