package runtime

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestCleanupRemovesProbes: a Persistent Instance's cleanup removes the
// probe networks a failed pinned create left behind, which are never
// attached, and keeps its other networks and its volumes (#842).
func TestCleanupRemovesProbes(t *testing.T) {
	var mu sync.Mutex
	var deleted []string
	nets := map[string]string{"n1": "gcpemu-i1-svc", "n2": "gcpemu-i1-ext", "n3": "gcpemu-i1-svc-probe-0a1b2c3d"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		p := r.URL.Path
		switch {
		case r.Method == "GET" && strings.HasSuffix(p, "/containers/json"):
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "GET" && strings.HasSuffix(p, "/networks"):
			_, _ = io.WriteString(w, `[{"Id":"n1","Name":"gcpemu-i1-svc"},{"Id":"n2","Name":"gcpemu-i1-ext"},{"Id":"n3","Name":"gcpemu-i1-svc-probe-0a1b2c3d"}]`)
		case r.Method == "GET" && strings.Contains(p, "/networks/"):
			id := p[strings.LastIndex(p, "/")+1:]
			_, _ = io.WriteString(w, `{"Id":"`+id+`","Name":"`+nets[id]+`"}`)
		case r.Method == "GET" && strings.HasSuffix(p, "/volumes"):
			_, _ = io.WriteString(w, `{"Volumes":[{"Name":"gcpemu-i1-data"}]}`)
		case r.Method == "DELETE":
			deleted = append(deleted, p)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	cl, err := New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{Client: cl, InstanceID: "i1"}
	if err := m.Cleanup(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || !strings.HasSuffix(deleted[0], "/networks/n3") {
		t.Errorf("deleted %q; want the probe network n3 only", deleted)
	}
}
