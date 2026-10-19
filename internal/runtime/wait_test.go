package runtime

import (
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"strings"
	"testing"
)

// frame is one frame of the Engine API's multiplexed output stream.
func frame(stream byte, s string) []byte {
	hdr := make([]byte, 8)
	hdr[0] = stream
	binary.BigEndian.PutUint32(hdr[4:], uint32(len(s)))
	return append(hdr, s...)
}

// WaitContainer waits for the container to stop and returns its exit code.
func TestWaitContainer(t *testing.T) {
	var got string
	cl := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Method + " " + r.URL.Path[strings.Index(r.URL.Path, "/containers/"):] + "?" + r.URL.RawQuery
		_, _ = io.WriteString(w, `{"StatusCode":3}`)
	})
	code, err := cl.WaitContainer(context.Background(), "c1")
	if err != nil || code != 3 {
		t.Fatalf("WaitContainer = %d, %v; want 3", code, err)
	}
	if want := "POST /containers/c1/wait?condition=not-running"; got != want {
		t.Errorf("request = %q, want %q", got, want)
	}
}

// FollowLogs streams stdout and stderr, demultiplexed, until the
// container's output ends.
func TestFollowLogs(t *testing.T) {
	var query string
	cl := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		w.Write(frame(1, "out 1\n"))
		w.(http.Flusher).Flush()
		w.Write(frame(2, "err 1\n"))
		w.Write(frame(1, "out 2\n"))
	})
	rc, err := cl.FollowLogs(context.Background(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if want := "out 1\nerr 1\nout 2\n"; string(b) != want {
		t.Errorf("logs = %q, want %q", b, want)
	}
	if !strings.Contains(query, "follow=true") || !strings.Contains(query, "stderr=true") || !strings.Contains(query, "stdout=true") {
		t.Errorf("query = %q, want follow, stdout and stderr", query)
	}
}
