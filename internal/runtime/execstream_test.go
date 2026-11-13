package runtime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// execServer answers an exec whose attached stream is handled by stream,
// and reports exit code 3.
func execServer(stream func(net.Conn)) http.HandlerFunc {
	return execServerStatus(`{"ExitCode":3}`, stream)
}

// execServerStatus is execServer reporting the exec's inspect as status.
func execServerStatus(status string, stream func(net.Conn)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
			_, _ = io.WriteString(w, status)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// notifyWriter signals got on its first write.
type notifyWriter struct {
	buf bytes.Buffer
	got chan struct{}
}

func (w *notifyWriter) Write(p []byte) (int, error) {
	if w.buf.Len() == 0 {
		close(w.got)
	}
	return w.buf.Write(p)
}

// ExecStream hands stdout to its writer as the command writes it: the
// command here writes, then waits for the test before it exits.
func TestExecStreamDeliversOutputBeforeTheCommandExits(t *testing.T) {
	exit := make(chan struct{})
	cl := testClient(t, execServer(func(conn net.Conn) {
		// The stream is closed only once stdin is read to its end: a
		// close with stdin unread resets the connection (#1104).
		drained := make(chan struct{})
		go func() {
			_, _ = io.Copy(io.Discard, conn)
			close(drained)
		}()
		_, _ = conn.Write(frame(1, "first\n"))
		<-exit
		_, _ = conn.Write(frame(2, "bye\n"))
		<-drained
	}))
	stdout := &notifyWriter{got: make(chan struct{})}
	var stderr bytes.Buffer
	done := make(chan error, 1)
	var code int
	go func() {
		var err error
		code, err = cl.ExecStream(context.Background(), "c1", []string{"cmd"}, strings.NewReader("in"), stdout, &stderr)
		done <- err
	}()
	select {
	case <-stdout.got:
	case err := <-done:
		t.Fatalf("ExecStream returned (%v) before the command exited", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no stdout 5s after the command wrote it")
	}
	close(exit)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if stdout.buf.String() != "first\n" || stderr.String() != "bye\n" || code != 3 {
		t.Errorf("ExecStream = stdout %q stderr %q exit %d, want \"first\\n\", \"bye\\n\", 3", stdout.buf.String(), stderr.String(), code)
	}
}

// ExecStream sends stdin to the command as it is read, and ends it.
func TestExecStreamSendsStdin(t *testing.T) {
	cl := testClient(t, execServer(func(conn net.Conn) {
		in, _ := io.ReadAll(conn)
		_, _ = conn.Write(frame(1, string(in)))
	}))
	var stdout bytes.Buffer
	if _, err := cl.ExecStream(context.Background(), "c1", []string{"cat"}, strings.NewReader("hello"), &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "hello" {
		t.Errorf("stdout = %q, want hello", stdout.String())
	}
}

// failingReader returns some data, then an error.
type failingReader struct{ sent bool }

var errBrokenStdin = errors.New("broken stdin")

func (r *failingReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, "part"), nil
	}
	return 0, errBrokenStdin
}

// A stdin that breaks fails the exec, even though the command would wait
// for the rest of it.
func TestExecStreamFailsWhenStdinBreaks(t *testing.T) {
	cl := testClient(t, execServer(func(conn net.Conn) {
		_, _ = io.ReadAll(conn)
	}))
	done := make(chan error, 1)
	go func() {
		_, err := cl.ExecStream(context.Background(), "c1", []string{"cat"}, &failingReader{}, io.Discard, io.Discard)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, errBrokenStdin) {
			t.Errorf("ExecStream = %v, want the stdin error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ExecStream still running 5s after its stdin broke")
	}
}

// A stream that breaks part way through a frame header fails the exec:
// what arrived is not all the command wrote.
func TestExecStreamFailsWhenTheStreamBreaks(t *testing.T) {
	cl := testClient(t, execServer(func(conn net.Conn) {
		_, _ = conn.Write(frame(1, "first\n"))
		_, _ = conn.Write(frame(1, "second\n")[:4])
	}))
	if _, err := cl.ExecStream(context.Background(), "c1", []string{"cmd"}, nil, io.Discard, io.Discard); err == nil {
		t.Fatal("ExecStream of a broken stream succeeded")
	}
}

// A stream that ends while the command still runs fails the exec rather
// than reporting the exit code of a command that has not exited.
func TestExecStreamFailsWhenTheCommandOutlivesItsStream(t *testing.T) {
	cl := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/exec"):
			_, _ = io.WriteString(w, `{"Id":"e1"}`)
		case strings.HasSuffix(r.URL.Path, "/exec/e1/start"):
			_, _ = w.Write(frame(1, "first\n"))
		case strings.HasSuffix(r.URL.Path, "/exec/e1/json"):
			_, _ = io.WriteString(w, `{"Running":true,"ExitCode":0}`)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := cl.ExecStream(ctx, "c1", []string{"cmd"}, nil, io.Discard, io.Discard); err == nil || ctx.Err() != nil {
		t.Fatalf("ExecStream = %v (ctx %v), want an error before the deadline", err, ctx.Err())
	}
}

// blockingReader blocks every Read until unblock is closed.
type blockingReader struct{ unblock chan struct{} }

func (r *blockingReader) Read([]byte) (int, error) {
	<-r.unblock
	return 0, io.EOF
}

// A command that exits without reading its stdin ends the exec, even
// while a Read of stdin blocks.
func TestExecStreamReturnsWhileStdinBlocks(t *testing.T) {
	cl := testClient(t, execServer(func(conn net.Conn) {
		_, _ = conn.Write(frame(2, "fatal\n"))
	}))
	in := &blockingReader{unblock: make(chan struct{})}
	defer close(in.unblock)
	done := make(chan error, 1)
	go func() {
		_, err := cl.ExecStream(context.Background(), "c1", []string{"psql"}, in, io.Discard, io.Discard)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("ExecStream = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ExecStream still waiting on stdin 5s after the command exited")
	}
}

// unreadStdin gives a few bytes, more than resetAfter reads, then blocks
// until the test ends: a write of stdin would take the reset error, which
// a socket reports once, from the read of the output.
func unreadStdin(t *testing.T) io.Reader {
	in := &blockingReader{unblock: make(chan struct{})}
	t.Cleanup(func() { close(in.unblock) })
	return io.MultiReader(strings.NewReader("data"), in)
}

// resetAfter reads part of stdin, writes out, waits for the client to get
// it, and then resets the connection with the rest of stdin unread, as a
// peer that closes without draining stdin may.
func resetAfter(out []byte, got <-chan struct{}) func(net.Conn) {
	return func(conn net.Conn) {
		_, _ = io.ReadFull(conn, make([]byte, 2))
		_, _ = conn.Write(out)
		<-got
		// Linger 0 makes the close a reset whatever is left unread.
		_ = conn.(*net.TCPConn).SetLinger(0)
	}
}

// A connection reset after the command exited ends its output: the exit
// code the daemon recorded is the result, and the output that arrived is
// kept (#1106). psql with ON_ERROR_STOP exits like this, its dump unread.
func TestExecStreamReturnsTheExitCodeWhenTheStreamIsResetAfterExit(t *testing.T) {
	stdout := &notifyWriter{got: make(chan struct{})}
	cl := testClient(t, execServerStatus(`{"Running":false,"ExitCode":3}`, resetAfter(frame(1, "ERROR: bad\n"), stdout.got)))
	code, err := cl.ExecStream(context.Background(), "c1", []string{"psql"}, unreadStdin(t), stdout, io.Discard)
	if err != nil || code != 3 || stdout.buf.String() != "ERROR: bad\n" {
		t.Errorf("ExecStream = stdout %q exit %d err %v, want \"ERROR: bad\\n\", 3, nil", stdout.buf.String(), code, err)
	}
}

// A reset while the command still runs fails the exec: its output was cut
// short.
func TestExecStreamFailsWhenTheStreamIsResetWhileTheCommandRuns(t *testing.T) {
	stdout := &notifyWriter{got: make(chan struct{})}
	cl := testClient(t, execServerStatus(`{"Running":true,"ExitCode":0}`, resetAfter(frame(1, "first\n"), stdout.got)))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := cl.ExecStream(ctx, "c1", []string{"cmd"}, unreadStdin(t), stdout, io.Discard); err == nil || ctx.Err() != nil {
		t.Fatalf("ExecStream = %v (ctx %v), want an error before the deadline", err, ctx.Err())
	}
}

// Only a reset is taken as the end of output: a stream with stdin that
// breaks part way through a frame fails the exec though the command exited.
func TestExecStreamWithStdinFailsWhenTheStreamBreaks(t *testing.T) {
	cl := testClient(t, execServer(func(conn net.Conn) {
		_, _ = io.ReadAll(conn)
		_, _ = conn.Write(frame(1, "second\n")[:4])
	}))
	if _, err := cl.ExecStream(context.Background(), "c1", []string{"cmd"}, strings.NewReader("in"), io.Discard, io.Discard); err == nil {
		t.Fatal("ExecStream of a broken stream succeeded")
	}
}
