package instance

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// clockSvc counts the ClockAdvanced calls it gets.
type clockSvc struct {
	fakeSvc
	advanced atomic.Int32
}

func (c *clockSvc) ClockAdvanced(context.Context) error { c.advanced.Add(1); return nil }

// clockState is the body of GET /_emu/v1/time.
type clockState struct {
	Now    string `json:"now"`
	Offset string `json:"offset"`
}

func startClockInstance(t *testing.T) (*Instance, *clockSvc, func(method, path string) (int, clockState)) {
	t.Helper()
	cfg := config.Defaults()
	cfg.Ephemeral = true
	cfg.DataDir = t.TempDir()
	cfg.Services = []string{"dns"}
	cfg.Ports["gateway"] = 0
	svc := &clockSvc{}
	in, err := New(&cfg, map[string]Factory{"dns": func(*emu.Env) emu.Service { return svc }}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := in.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = in.Shutdown(ctx) })
	base := "http://" + in.Env.Endpoints.Get("gateway")
	admin := func(method, path string) (int, clockState) {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var s clockState
		_ = json.NewDecoder(resp.Body).Decode(&s)
		return resp.StatusCode, s
	}
	return in, svc, admin
}

// nowNear fails unless s is an RFC 3339 instant within a second of want.
func nowNear(t *testing.T, s string, want time.Time) {
	t.Helper()
	got, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("now = %q: %v", s, err)
	}
	if d := got.Sub(want); d < -time.Second || d > time.Second {
		t.Errorf("now = %v, want within 1s of %v", got, want)
	}
}

// TestClockShowAndReset is #1172: GET /_emu/v1/time reads the clock without
// moving it or running ClockAdvanced, and POST /_emu/v1/time/reset sets the
// offset back to zero without running ClockAdvanced either.
func TestClockShowAndReset(t *testing.T) {
	_, svc, admin := startClockInstance(t)
	if code, _ := admin(http.MethodPost, "/_emu/v1/time/advance?by=2h"); code != http.StatusOK {
		t.Fatalf("advance = %d", code)
	}
	advanced := svc.advanced.Load()
	for i := range 2 {
		code, s := admin(http.MethodGet, "/_emu/v1/time")
		if code != http.StatusOK || s.Offset != "2h0m0s" {
			t.Fatalf("show %d = %d %+v, want offset 2h0m0s", i, code, s)
		}
		nowNear(t, s.Now, time.Now().Add(2*time.Hour))
	}
	if got := svc.advanced.Load(); got != advanced {
		t.Errorf("show ran ClockAdvanced %d times", got-advanced)
	}

	code, s := admin(http.MethodPost, "/_emu/v1/time/reset")
	if code != http.StatusOK || s.Offset != "0s" {
		t.Fatalf("reset = %d %+v, want offset 0s", code, s)
	}
	nowNear(t, s.Now, time.Now())
	if got := svc.advanced.Load(); got != advanced {
		t.Errorf("time reset ran ClockAdvanced %d times", got-advanced)
	}
	code, s = admin(http.MethodGet, "/_emu/v1/time")
	if code != http.StatusOK || s.Offset != "0s" {
		t.Fatalf("show after reset = %d %+v, want offset 0s", code, s)
	}
	nowNear(t, s.Now, time.Now())
}

// TestResetClearsClockOffset is #1172: `gcpemu reset` makes the Instance
// new, and that includes its clock.
func TestResetClearsClockOffset(t *testing.T) {
	_, _, admin := startClockInstance(t)
	if code, _ := admin(http.MethodPost, "/_emu/v1/time/advance?by=2h"); code != http.StatusOK {
		t.Fatalf("advance = %d", code)
	}
	if code, _ := admin(http.MethodPost, "/_emu/v1/reset"); code != http.StatusOK {
		t.Fatalf("reset = %d", code)
	}
	code, s := admin(http.MethodGet, "/_emu/v1/time")
	if code != http.StatusOK || s.Offset != "0s" {
		t.Fatalf("show after reset = %d %+v, want offset 0s", code, s)
	}
	nowNear(t, s.Now, time.Now())
}
