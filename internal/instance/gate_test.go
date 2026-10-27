package instance

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// gateSvc serves /dns/block, which blocks until its request ends (or, with
// ignoreCtx, until release), and holds its own Reset open until resume.
type gateSvc struct {
	fakeSvc
	ignoreCtx bool
	blocked   chan struct{} // a /dns/block request is in its handler
	release   chan struct{} // ends /dns/block when ignoreCtx
	returned  atomic.Bool   // the /dns/block handler has returned
	resetting chan bool     // Reset began; sends whether the handler had returned
	resume    chan struct{} // lets Reset finish
}

func (s *gateSvc) Register(r emu.Router) error {
	r.Mount("dns", nil, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/block" {
			close(s.blocked)
			if s.ignoreCtx {
				<-s.release
			} else {
				<-req.Context().Done()
				time.Sleep(20 * time.Millisecond) // Reset must wait for this
			}
			w.WriteHeader(http.StatusTeapot)
			s.returned.Store(true)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	return nil
}

func (s *gateSvc) Reset(context.Context) error {
	s.resetting <- s.returned.Load()
	<-s.resume
	return nil
}

func startGateInstance(t *testing.T, svc *gateSvc) (*Instance, string) {
	t.Helper()
	svc.blocked, svc.release = make(chan struct{}), make(chan struct{})
	svc.resetting, svc.resume = make(chan bool, 1), make(chan struct{})
	cfg := config.Defaults()
	cfg.Ephemeral = true
	cfg.DataDir = t.TempDir()
	cfg.Services = []string{"dns"}
	cfg.Ports["gateway"] = 0
	in, err := New(&cfg, map[string]Factory{"dns": func(*emu.Env) emu.Service { return svc }}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := in.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = in.Shutdown(context.Background()) })
	if err := in.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	return in, "http://" + in.Env.Endpoints.Get("gateway")
}

// readyReason is /_emu/v1/ready's readiness and its reason for hold.
func readyReason(t *testing.T, base, hold string) (bool, string) {
	t.Helper()
	var r struct {
		Ready    bool `json:"ready"`
		Services map[string]struct {
			Reason string `json:"reason"`
		} `json:"services"`
	}
	getJSON(t, base+"/_emu/v1/ready", nil, &r)
	return r.Ready, r.Services[hold].Reason
}

// testGatedStateChange checks that while change (a Reset or a State
// snapshot restore) runs, the Instance cuts off the request in progress
// before resetting services, refuses new ones but the Admin API's, and is
// not ready with hold's reason; and that it serves again afterwards (#302).
func testGatedStateChange(t *testing.T, base string, svc *gateSvc, change func(context.Context) error, hold, reason string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	inProgress := make(chan *http.Response, 1)
	go func() {
		resp, _ := call(t, http.DefaultClient, http.MethodGet, base+"/dns/block", nil)
		inProgress <- resp
	}()
	<-svc.blocked
	changed := make(chan error, 1)
	go func() { changed <- change(ctx) }()

	if returned := <-svc.resetting; !returned {
		t.Error("services were reset before the request in progress returned")
	}
	if resp := <-inProgress; resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("request in progress: %d, want 503", resp.StatusCode)
	}
	resp, body := call(t, http.DefaultClient, http.MethodGet, base+"/dns/v1/x", nil)
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" || !strings.Contains(body, `"UNAVAILABLE"`) {
		t.Errorf("new request: %d Retry-After=%q %s, want 503 UNAVAILABLE with Retry-After", resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
	if resp, _ := call(t, http.DefaultClient, http.MethodGet, base+"/_emu/v1/health", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("health: %d, want 200", resp.StatusCode)
	}
	if ready, got := readyReason(t, base, hold); ready || got != reason {
		t.Errorf("ready = %v, %s reason %q; want not ready, %q", ready, hold, got, reason)
	}

	close(svc.resume)
	if err := <-changed; err != nil {
		t.Fatal(err)
	}
	if resp, body := call(t, http.DefaultClient, http.MethodGet, base+"/dns/v1/x", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("after: %d %s, want 200", resp.StatusCode, body)
	}
	if ready, got := readyReason(t, base, hold); !ready || got != "" {
		t.Errorf("after: ready = %v, %s reason %q; want ready", ready, hold, got)
	}
}

func TestResetGatesRequests(t *testing.T) {
	svc := &gateSvc{}
	in, base := startGateInstance(t, svc)
	testGatedStateChange(t, base, svc, in.Reset, "reset", "reset in progress")
}

func TestRestoreSnapshotGatesRequests(t *testing.T) {
	svc := &gateSvc{}
	in, base := startGateInstance(t, svc)
	if _, err := in.SaveSnapshot(t.Context(), "s1"); err != nil {
		t.Fatal(err)
	}
	restore := func(ctx context.Context) error { return in.RestoreSnapshot(ctx, "s1") }
	testGatedStateChange(t, base, svc, restore, "restore", "restoring State snapshot s1")
}

// A handler that outlasts Reset's ctx fails the Reset before any service
// is reset, and the Instance serves again afterwards.
func TestResetBoundedByInProgressRequest(t *testing.T) {
	svc := &gateSvc{ignoreCtx: true}
	in, base := startGateInstance(t, svc)
	go func() { _, _ = call(t, http.DefaultClient, http.MethodGet, base+"/dns/block", nil) }()
	<-svc.blocked
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := in.Reset(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Reset = %v, want %v", err, context.DeadlineExceeded)
	}
	close(svc.release)
	select {
	case <-svc.resetting:
		t.Error("services were reset although the request in progress outlasted Reset's ctx")
	default:
	}
	if resp, body := call(t, http.DefaultClient, http.MethodGet, base+"/dns/v1/x", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("after: %d %s, want 200", resp.StatusCode, body)
	}
	if ready, got := readyReason(t, base, "reset"); !ready || got != "" {
		t.Errorf("after: ready = %v, reason %q; want ready", ready, got)
	}
}
