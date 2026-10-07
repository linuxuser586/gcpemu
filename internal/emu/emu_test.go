package emu

import (
	"net"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/config"
)

// TestListenTCPRange: port 0 is taken from --port-range; an exhausted
// range is an error (FR-CORE-042).
func TestListenTCPRange(t *testing.T) {
	cfg := config.Defaults()
	cfg.PortRange = "42400-42402"
	e := &Env{Config: &cfg}
	var ls []net.Listener
	defer func() {
		for _, l := range ls {
			l.Close()
		}
	}()
	seen := map[int]bool{}
	for range 3 {
		l, err := e.ListenTCP("127.0.0.1", 0)
		if err != nil {
			t.Fatal(err)
		}
		ls = append(ls, l)
		p := l.Addr().(*net.TCPAddr).Port
		if p < 42400 || p > 42402 || seen[p] {
			t.Fatalf("port %d (seen %v)", p, seen)
		}
		seen[p] = true
	}
	if _, err := e.ListenTCP("127.0.0.1", 0); err == nil {
		t.Fatal("exhausted range still allocated a port")
	}
	cfg.PortRange = ""
	l, err := e.ListenTCP("127.0.0.1", 0)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
}
