package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestDisrupted is #318: a container the client started that the runtime
// has since stopped or started again (a daemon restart, `docker stop`,
// `docker restart`) is disrupted; one the client stopped, restarted or
// never started is not.
func TestDisrupted(t *testing.T) {
	type ct struct{ status, started string }
	var mu sync.Mutex
	cts := map[string]*ct{}
	at := 0
	start := func(c *ct) {
		at++
		c.status, c.started = "running", time.Date(2026, 10, 15, 4, 0, at, 0, time.UTC).Format(time.RFC3339Nano)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"+apiVersion+"/containers/"), "/")
		c := cts[parts[0]]
		if c == nil {
			c = &ct{status: "created", started: "0001-01-01T00:00:00Z"}
			cts[parts[0]] = c
		}
		switch parts[len(parts)-1] {
		case "start", "restart":
			start(c)
		case "stop":
			c.status = "exited"
		case "json":
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": parts[0], "Name": "/" + parts[0],
				"State": map[string]any{"Status": c.status, "Running": c.status == "running", "StartedAt": c.started}})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	cl, err := New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, id := range []string{"exited", "restarted", "stopped", "bounced", "running"} {
		if err := cl.StartContainer(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := cl.StopContainer(ctx, "stopped", time.Second); err != nil {
		t.Fatal(err)
	}
	if err := cl.RestartContainer(ctx, "bounced", time.Second); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	cts["exited"].status = "exited"
	start(cts["restarted"])
	cts["untracked"] = &ct{status: "exited"}
	mu.Unlock()

	var all []Container
	for _, id := range []string{"bounced", "exited", "restarted", "running", "stopped", "untracked"} {
		c, err := cl.InspectContainer(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, c)
	}
	if got, want := cl.Disrupted(all), []string{"exited (exited)", "restarted (restarted)"}; !slices.Equal(got, want) {
		t.Errorf("Disrupted = %v, want %v", got, want)
	}
}
