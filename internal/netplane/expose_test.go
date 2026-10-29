package netplane_test

import (
	"bytes"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/netplane"
)

// syncBuffer is a log destination safe for concurrent writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) warnings() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, l := range strings.Split(b.b.String(), "\n") {
		if strings.Contains(l, "level=WARN") {
			out = append(out, l)
		}
	}
	return out
}

// connect opens a connection to addr and waits for the exposed port to
// close it or carry a byte back.
func connect(t *testing.T, addr string) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(15 * time.Second))
	_, _ = c.Read(make([]byte, 1))
}

// A named exposed port whose target is down logs one warning naming it
// for the outage, not one per connection, and warns again in the next
// outage (#840).
func TestExposeWarnsOncePerOutage(t *testing.T) {
	logs := &syncBuffer{}
	p := netplane.New(nil, nil, "127.0.0.1", slog.New(slog.NewTextHandler(logs, nil)))
	// A target address nothing listens on until the test does.
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	target := held.Addr().String()
	held.Close()

	e, err := p.Expose("GKE cluster p/us-central1-a/c", "127.0.0.1:0", target, true)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	for range 3 {
		connect(t, e.Addr())
	}
	w := logs.warnings()
	if len(w) != 1 {
		t.Fatalf("warnings = %q; want 1", w)
	}
	for _, want := range []string{"GKE cluster p/us-central1-a/c", target} {
		if !strings.Contains(w[0], want) {
			t.Errorf("warning %q; want it to contain %q", w[0], want)
		}
	}

	// The target is back: the outage ends.
	up, err := net.Listen("tcp", target)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := up.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("k"))
			c.Close()
		}
	}()
	connect(t, e.Addr())
	up.Close()
	connect(t, e.Addr())
	connect(t, e.Addr())
	if w := logs.warnings(); len(w) != 2 {
		t.Errorf("warnings after a second outage = %q; want 2", w)
	}
}
