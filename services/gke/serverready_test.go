package gke

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// fakeServerRuntime is a runtime API whose control-plane container id
// reports status and, once exited, a k3s fatal log line.
func fakeServerRuntime(t *testing.T, id, status string) *runtime.Manager {
	t.Helper()
	exited := status == "exited"
	docker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/"+id+"/json"):
			_ = json.NewEncoder(w).Encode(map[string]any{"Name": "/" + id,
				"State": map[string]any{"Status": status, "Running": !exited, "ExitCode": map[bool]int{true: 255}[exited]}})
		case strings.HasSuffix(r.URL.Path, "/containers/"+id+"/logs"):
			msg := "F1016 hooks.go:203] PostStartHook \"start-service-ip-repair-controllers\" failed\n"
			hdr := make([]byte, 8)
			hdr[0] = 2
			binary.BigEndian.PutUint32(hdr[4:], uint32(len(msg)))
			_, _ = w.Write(append(hdr, msg...))
		default:
			t.Errorf("unexpected runtime request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(docker.Close)
	cl, err := runtime.New("tcp://" + strings.TrimPrefix(docker.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	return &runtime.Manager{Client: cl, InstanceID: "i1"}
}

// TestAwaitServerFailsFast (#395): when the control-plane container exits
// before the API server is ready, waiting fails at once with the exit
// code and the k3s log tail, not "context deadline exceeded".
func TestAwaitServerFailsFast(t *testing.T) {
	rt := fakeServerRuntime(t, "cp1", "exited")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	err := awaitServer(ctx, rt, "cp1", func() bool { return false })
	var exited *serverExitedError
	if !errors.As(err, &exited) {
		t.Fatalf("awaitServer = %v; want a serverExitedError", err)
	}
	for _, want := range []string{"control plane exited with code 255", "start-service-ip-repair-controllers"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("awaitServer = %q; want it to contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "deadline") {
		t.Errorf("awaitServer = %q; want no deadline error", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("awaitServer took %v; want it to fail fast", d)
	}
}

// TestAwaitServerRunning: a running server that never becomes ready
// times out with the log tail, and is not reported as exited.
func TestAwaitServerRunning(t *testing.T) {
	rt := fakeServerRuntime(t, "cp1", "running")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := awaitServer(ctx, rt, "cp1", func() bool { return false })
	var exited *serverExitedError
	if err == nil || errors.As(err, &exited) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("awaitServer = %v; want a deadline error that is not serverExitedError", err)
	}
	if !strings.Contains(err.Error(), "control plane did not become ready") {
		t.Errorf("awaitServer = %q; want the not-ready message", err)
	}

	// Ready at once.
	if err := awaitServer(context.Background(), rt, "cp1", func() bool { return true }); err != nil {
		t.Errorf("awaitServer(ready) = %v", err)
	}
}

// TestRestartExitedServer (#395): a control plane that exits before it is
// ready is started once more; a second exit, or any other error, fails.
func TestRestartExitedServer(t *testing.T) {
	crash := &serverExitedError{code: 255, logs: "panic"}
	other := errors.New("pulling image")
	for _, tt := range []struct {
		name  string
		errs  []error // returned by successive starts
		want  error
		calls int
	}{
		{"ready", []error{nil}, nil, 1},
		{"crash then ready", []error{crash, nil}, nil, 2},
		{"crash twice", []error{crash, crash}, crash, 2},
		{"other error", []error{other}, other, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls, warned := 0, 0
			err := restartExited(func(again bool) error {
				if again != (calls > 0) {
					t.Errorf("start %d: again = %v", calls+1, again)
				}
				calls++
				return tt.errs[calls-1]
			}, func(error) { warned++ })
			if !errors.Is(err, tt.want) {
				t.Errorf("restartExited = %v; want %v", err, tt.want)
			}
			if calls != tt.calls {
				t.Errorf("started %d times; want %d", calls, tt.calls)
			}
			if warned != tt.calls-1 {
				t.Errorf("warned %d times; want %d", warned, tt.calls-1)
			}
		})
	}
}
