package ar

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/lanbind"
)

// TestLANNotLocal is #497: an --ar-bind address that isn't the Instance's
// is skipped with a warning that, in a container, names the cause and the
// fix, and on the host is unchanged.
func TestLANNotLocal(t *testing.T) {
	const ip = "192.0.2.1" // TEST-NET-1: never an address of this host
	if l, err := net.Listen("tcp", ip+":0"); err == nil {
		l.Close()
		t.Skip(ip + " is an address of this host")
	}
	for _, tc := range []struct {
		where       lanbind.Where
		want, avoid string
	}{
		{lanbind.Bridge, "not an address of this container", "may not"},
		{lanbind.Unconfirmed, "may not be an address of this container", ""},
		{lanbind.Host, "cannot serve the registry", "container"},
	} {
		var out strings.Builder
		cfg := config.Defaults()
		cfg.ARBind = []string{ip}
		s := &Service{env: &emu.Env{
			Config:       &cfg,
			Log:          slog.New(slog.NewTextHandler(&out, nil)),
			LANBindWhere: func(context.Context) lanbind.Where { return tc.where },
		}}
		if srvs := s.startLAN(context.Background(), 0, http.NotFoundHandler()); len(srvs) != 0 {
			t.Errorf("%v: served %d listeners", tc.where, len(srvs))
		}
		log := out.String()
		if !strings.Contains(log, "level=WARN") || !strings.Contains(log, tc.want) {
			t.Errorf("%v: log lacks %q:\n%s", tc.where, tc.want, log)
		}
		if tc.avoid != "" && strings.Contains(log, tc.avoid) {
			t.Errorf("%v: log has %q:\n%s", tc.where, tc.avoid, log)
		}
		if tc.where != lanbind.Host && !strings.Contains(log, "--network host") {
			t.Errorf("%v: log lacks the fix:\n%s", tc.where, log)
		}
		if tc.where != lanbind.Host && strings.Contains(log, "port taken") {
			t.Errorf("%v: log blames the port:\n%s", tc.where, log)
		}
	}
}
