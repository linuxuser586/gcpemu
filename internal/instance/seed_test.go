package instance

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestSeedUploadAndExport covers the admin API's seed file upload and
// export (FR-UI-017, FR-CORE-013) for the sections the core owns.
func TestSeedUploadAndExport(t *testing.T) {
	in, _ := startSnapInstance(t)
	base := "http://" + in.Env.Endpoints.Get("gateway") + "/_emu/v1/seed"
	do := func(method, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, base, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/yaml")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	seed := `projects: [seed-a, seed-b]
faults:
  - id: slow
    service: dns
    latency: 1.5s
    disabled: true
`
	if code, body := do("POST", seed); code != 200 {
		t.Fatalf("upload = %d %s", code, body)
	}
	code, exported := do("GET", "")
	if code != 200 {
		t.Fatalf("export = %d %s", code, exported)
	}
	want := `projects:
  - seed-a
  - seed-b
faults:
  - disabled: true
    id: slow
    latency: 1.5s
    service: dns
`
	if exported != want {
		t.Errorf("export =\n%s\nwant\n%s", exported, want)
	}
	// The export applies back unchanged (idempotent re-seed).
	if code, body := do("POST", exported); code != 200 {
		t.Fatalf("re-upload = %d %s", code, body)
	}
	if n := len(in.Faults().List()); n != 1 {
		t.Errorf("rules after re-seeding the export = %d", n)
	}
	if code, body := do("POST", "pubsub: {topics: []}\n"); code != 400 || !strings.Contains(body, "service not running") {
		t.Errorf("seed for a Service not running = %d %s", code, body)
	}
	if code, body := do("POST", "projects: [unclosed"); code != 400 {
		t.Errorf("malformed seed = %d %s", code, body)
	}
}
