package lb

import (
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"

	computev1 "google.golang.org/api/compute/v1"

	emuconfig "github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// TestExcludedPortNeverEdge: on Windows a refused bind means the port is
// in an excluded range, so the forwarding rule fails naming
// excludedportrange, even with Options.Edge and a usable runtime, and the
// Edge relay never gets it (ADR 0015, #376).
func TestExcludedPortNeverEdge(t *testing.T) {
	for _, port := range []int{80, 50000} {
		s := &Service{env: &emu.Env{Config: &emuconfig.Config{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
		s.opts.Store(&Options{Edge: true})
		d := &dataplane{s: s, listeners: map[string]*listener{}, loopX: map[string]int{}, single: true, goos: "windows"}
		d.edge = &edge{d: d, rtTried: true, rtOK: true, alias: map[string]string{}, byAlias: map[string]string{}, ports: map[string]int{}}
		d.bind = func(addr string) (net.Listener, error) {
			return nil, &net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", syscall.EACCES)}
		}
		fe := &frontend{key: "203.0.113.5:" + itoa(port), fr: &computev1.ForwardingRule{Name: "a"},
			frPath: "projects/p/global/forwardingRules/a", ip: "203.0.113.5", port: port}
		l, err := d.listen(fe)
		if err == nil {
			t.Fatalf("port %d: listen = %s (%s), want an error", port, l.addr, l.mode)
		}
		if !strings.Contains(err.Error(), "excludedportrange protocol=tcp") {
			t.Errorf("port %d: error %q does not name excludedportrange", port, err)
		}
		if len(d.edge.alias) != 0 || d.edge.id != "" {
			t.Errorf("port %d: the Edge relay got the rule: alias %v, container %q", port, d.edge.alias, d.edge.id)
		}
	}
}
