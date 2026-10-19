package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
)

// testPipe returns a fresh pipe name and its npipe:// endpoint.
func testPipe() (name, endpoint string) {
	name = fmt.Sprintf(`\\.\pipe\gcpemu-test-%d`, time.Now().UnixNano())
	return name, "npipe://" + strings.ReplaceAll(name, `\`, "/")
}

// pipeDaemon serves h on a fresh named pipe and returns its npipe:// endpoint.
func pipeDaemon(t *testing.T, h http.Handler) string {
	t.Helper()
	name, ep := testPipe()
	l, err := winio.ListenPipe(name, &winio.PipeConfig{MessageMode: true})
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ep
}

// TestNamedPipeInfo: a client for an npipe:// endpoint talks to the daemon
// listening on that pipe (#374).
func TestNamedPipeInfo(t *testing.T) {
	ep := pipeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/version") {
			_, _ = io.WriteString(w, `{"Version":"28.0.1","Arch":"amd64"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	cl, err := New(ep)
	if err != nil {
		t.Fatal(err)
	}
	in, err := cl.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if in.Version != "28.0.1" {
		t.Errorf("Version = %q, want 28.0.1", in.Version)
	}
}

// execDaemon answers an exec whose attached stream is handled by stream.
func execDaemon(stream func(net.Conn)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/exec"):
			_, _ = io.WriteString(w, `{"Id":"e1"}`)
		case strings.HasSuffix(r.URL.Path, "/exec/e1/start"):
			conn, buf, err := http.NewResponseController(w).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = buf.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			_ = buf.Flush()
			stream(conn)
		case strings.HasSuffix(r.URL.Path, "/exec/e1/json"):
			_, _ = io.WriteString(w, `{"ExitCode":3}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// TestNamedPipeExecDeadline: a deadline aborts an attached stream the daemon
// never ends.
func TestNamedPipeExecDeadline(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	ep := pipeDaemon(t, execDaemon(func(net.Conn) { <-release }))
	cl, err := New(ep)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := cl.Exec(ctx, "c1", []string{"cat"}, []byte("hello"))
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Exec = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Exec still running 5s after its deadline")
	}
}

// TestNamedPipeDialDeadline: a deadline aborts a dial to a pipe whose
// instances are all busy.
func TestNamedPipeDialDeadline(t *testing.T) {
	name, ep := testPipe()
	l, err := winio.ListenPipe(name, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	// Nothing accepts, so every instance of the pipe is busy.
	cl, err := New(ep)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = cl.Info(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Info = %v, want context.DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("Info took %v after a 200ms deadline", d)
	}
}

// TestNamedPipeExecStdin: exec with stdin goes over a hijacked pipe
// connection; the daemon sees stdin end and the output is demultiplexed.
func TestNamedPipeExecStdin(t *testing.T) {
	ep := pipeDaemon(t, execDaemon(func(conn net.Conn) {
		in, _ := io.ReadAll(conn)
		out := append([]byte{1, 0, 0, 0, 0, 0, 0, byte(len(in))}, in...)
		_, _ = conn.Write(out)
	}))
	cl, err := New(ep)
	if err != nil {
		t.Fatal(err)
	}
	res, err := cl.Exec(context.Background(), "c1", []string{"cat"}, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Stdout) != "hello" || res.ExitCode != 3 {
		t.Errorf("Exec = %q exit %d, want \"hello\" exit 3", res.Stdout, res.ExitCode)
	}
}

// TestNamedPipeDaemon: the client reaches a real Docker Engine on its named
// pipe, for plain requests and hijacked ones. It runs against the hosted
// windows-* runners' engine (Windows containers, which is enough for the
// transport) and Docker Desktop, and skips where neither is present.
func TestNamedPipeDaemon(t *testing.T) {
	found := false
	for _, pipe := range []string{"docker_engine", "dockerDesktopLinuxEngine"} {
		if _, err := os.Stat(`\\.\pipe\` + pipe); err != nil {
			continue
		}
		found = true
		t.Run(pipe, func(t *testing.T) {
			cl, err := New("npipe:////./pipe/" + pipe)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			in, err := cl.Info(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if in.Version == "" {
				t.Errorf("Info = %+v, want a version", in)
			}
			// The engine answers an upgrade for an exec it doesn't know.
			_, _, _, err = cl.hijack(ctx, "/exec/gcpemu-no-such-exec/start", []byte(`{"Detach":false,"Tty":false}`))
			if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
				t.Errorf("hijack = %v, want HTTP 404", err)
			}
		})
	}
	if !found {
		t.Skip("no Docker Engine named pipe")
	}
}
