package emutestx

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/instance"
)

// HeldRequest sends a request for path to the Instance's endpoint and
// holds it in progress: its headers promise a body that never comes. It
// returns once service's handler has answered and logged it, when the HTTP
// server is waiting for that body, so the request stays in progress until
// the connection it returns closes. path must name a handler that does not
// read its body.
func HeldRequest(t testing.TB, in *instance.Instance, endpoint, service, path string) net.Conn {
	t.Helper()
	before := len(in.Log.Entries(service))
	c, err := net.Dial("tcp", in.Env.Endpoints.Get(endpoint))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if _, err := io.WriteString(c, "GET "+path+" HTTP/1.1\r\nHost: localhost\r\nMetadata-Flavor: Google\r\nContent-Length: 10\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(in.Log.Entries(service)) == before {
		if time.Now().After(deadline) {
			t.Fatalf("%s never answered %s", service, path)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return c
}

// Ended is a context that has already ended, as Teardown's can be.
func Ended() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), -time.Second)
	cancel()
	return ctx
}

// AssertClosed fails t unless the server has closed c: reading it ends
// without waiting for a deadline.
func AssertClosed(t testing.TB, c net.Conn) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.Copy(io.Discard, c); errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("connection still open after Stop returned")
	}
}
