package edgeagent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/runtime"
)

func TestControlLineRoundTrip(t *testing.T) {
	var b strings.Builder
	writeControl(&b, "127.0.0.1:40123")
	logs := "starting\n" + b.String() + "gcpemu agent lb-lan: listening on 10.0.0.5:80\n"
	if got, ok := readControl(logs); !ok || got != "127.0.0.1:40123" {
		t.Errorf("readControl = %q, %v; want 127.0.0.1:40123, true", got, ok)
	}
}

func TestControlLineMissing(t *testing.T) {
	for _, logs := range []string{"", "starting\nlisten tcp 127.0.0.1:0: bind: address in use\n"} {
		if got, ok := readControl(logs); ok {
			t.Errorf("readControl(%q) = %q, true; want not found", logs, got)
		}
	}
}

func TestLANAgentReportsControl(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- serveLAN(ctx, pw, "secret"); pw.Close() }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serveLAN: %v", err)
		}
	})
	line, err := bufio.NewReader(pr).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	go io.Copy(io.Discard, pr)
	addr, ok := readControl(line)
	if !ok {
		t.Fatalf("no control line in %q", line)
	}
	resp, err := http.Post("http://"+addr+"/config", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST /config without token = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
}

// relayID is the container id the relay under test holds.
const relayID = "abc123"

// closeRelay closes a gfe-lan relay holding container relayID with runtime
// rt, checks its state is reset and returns the log.
func closeRelay(t *testing.T, rt func(context.Context) (*runtime.Manager, error)) string {
	t.Helper()
	var buf bytes.Buffer
	r := &LANRelay{Runtime: rt, Name: "gfe-lan", Log: slog.New(slog.NewTextHandler(&buf, nil))}
	r.created, r.id, r.pushed = true, relayID, "{}"
	r.Close(context.Background())
	if r.created || r.id != "" || r.pushed != "" {
		t.Errorf("after Close: created=%v id=%q pushed=%q; want reset", r.created, r.id, r.pushed)
	}
	return buf.String()
}

// fakeRuntime returns a runtime whose daemon answers DELETE
// /containers/{relayID} with status and anything else with 404.
func fakeRuntime(t *testing.T, status int) func(context.Context) (*runtime.Manager, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodDelete && strings.HasSuffix(req.URL.Path, "/containers/"+relayID) {
			w.WriteHeader(status)
			return
		}
		http.NotFound(w, req)
	}))
	t.Cleanup(srv.Close)
	cl, err := runtime.New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	return func(context.Context) (*runtime.Manager, error) { return &runtime.Manager{Client: cl}, nil }
}

func TestLANRelayCloseLogsRemoved(t *testing.T) {
	logs := closeRelay(t, fakeRuntime(t, http.StatusNoContent))
	if !strings.Contains(logs, "gfe-lan relay container removed") {
		t.Errorf("log = %q; want relay container removed", logs)
	}
	if strings.Contains(logs, "Cleanup") {
		t.Errorf("log = %q; want no left-for-Cleanup line", logs)
	}
}

func TestLANRelayCloseRemoveFailsLeavesForCleanup(t *testing.T) {
	logs := closeRelay(t, fakeRuntime(t, http.StatusInternalServerError))
	assertLeftForCleanup(t, logs)
}

func TestLANRelayCloseRuntimeFailsLeavesForCleanup(t *testing.T) {
	logs := closeRelay(t, func(context.Context) (*runtime.Manager, error) {
		return nil, errors.New("no runtime here")
	})
	assertLeftForCleanup(t, logs)
	if !strings.Contains(logs, "no runtime here") {
		t.Errorf("log = %q; want the runtime's error", logs)
	}
}

func assertLeftForCleanup(t *testing.T, logs string) {
	t.Helper()
	if strings.Contains(logs, "relay container removed") {
		t.Errorf("log = %q; want no relay container removed", logs)
	}
	if !strings.Contains(logs, "level=INFO") || !strings.Contains(logs, "gfe-lan relay container left for Cleanup") || !strings.Contains(logs, "err=") {
		t.Errorf("log = %q; want Info left for Cleanup with err", logs)
	}
}
