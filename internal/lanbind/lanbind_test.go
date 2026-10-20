package lanbind

import (
	"errors"
	"net"
	"strings"
	"testing"
)

// notLocal binds an address that is never one of this host's (TEST-NET-1,
// RFC 5737), for the platform's "not an address of mine" error.
func notLocal(t *testing.T) error {
	t.Helper()
	l, err := net.Listen("tcp", "192.0.2.1:0")
	if err == nil {
		l.Close()
		t.Skip("192.0.2.1 is an address of this host")
	}
	return err
}

func TestExplainNotLocal(t *testing.T) {
	err := notLocal(t)
	for _, c := range []struct {
		where Where
		want  string
	}{
		{Bridge, "not an address of this container"},
		{Unconfirmed, "may not be an address of this container"},
	} {
		got := Explain(c.where, err)
		if !errors.Is(got, err) {
			t.Errorf("%v: Explain(%v) does not wrap the bind error", c.where, got)
		}
		for _, w := range []string{c.want, "--network host", "native binary"} {
			if !strings.Contains(got.Error(), w) {
				t.Errorf("%v: %q lacks %q", c.where, got, w)
			}
		}
		if strings.Contains(got.Error(), "publish") {
			t.Errorf("%v: %q suggests publishing a port", c.where, got)
		}
	}
	if c := Explain(Bridge, err).Error(); strings.Contains(c, "may not") {
		t.Errorf("bridge: %q hedges", c)
	}
	if got := Explain(Host, err); got != err {
		t.Errorf("host: Explain = %v, want the bind error unchanged", got)
	}
}

func TestExplainOtherErrorsUnchanged(t *testing.T) {
	err := errors.New("address already in use")
	for _, w := range []Where{Host, Bridge, Unconfirmed} {
		if got := Explain(w, err); got != err {
			t.Errorf("%v: Explain = %v, want it unchanged", w, got)
		}
	}
}

func TestWarning(t *testing.T) {
	got := Warning([]string{"gke-bind", "ar-bind"})
	for _, w := range []string{"--gke-bind", "--ar-bind", "container networks", "not the LAN", "--network host", "native binary"} {
		if !strings.Contains(got, w) {
			t.Errorf("Warning = %q, lacks %q", got, w)
		}
	}
	if strings.Contains(got, "--lb-bind") {
		t.Errorf("Warning = %q names a flag that isn't set", got)
	}
}

// Locate goes by the Containerized Instance decision: only a runtime
// that found the emulator's own container confirms a bridge network (#564).
func TestLocate(t *testing.T) {
	for _, c := range []struct {
		name          string
		containerized bool
		confirmed     bool
		want          Where
	}{
		{"host, or a host-network container", false, false, Host},
		{"runtime finds it on the host's network", false, true, Host},
		{"runtime finds its own container", true, true, Bridge},
		{"decision on without the runtime's answer", true, false, Unconfirmed},
	} {
		if got := Locate(c.containerized, c.confirmed); got != c.want {
			t.Errorf("%s: Locate = %v, want %v", c.name, got, c.want)
		}
	}
}
