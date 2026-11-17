package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// clockAPIGateway returns the address of the Instance "mine"'s API gateway,
// which answers every clock request with the clock body, and the requests
// it got other than the one asking which Instance it is.
func clockAPIGateway(t *testing.T) (string, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_emu/v1/info" {
			_, _ = fmt.Fprint(w, `{"id":"mine"}`)
			return
		}
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.Path)
		mu.Unlock()
		_, _ = fmt.Fprint(w, `{"now":"2026-11-17T12:00:00Z","offset":"2h0m0s"}`)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

// TestTimeShowAndReset is #1172: `time show` reads the clock with a GET
// and `time reset` sets it back with a POST, and each prints the clock.
func TestTimeShowAndReset(t *testing.T) {
	for _, tt := range []struct{ cmd, want string }{
		{"show", "GET /_emu/v1/time"},
		{"reset", "POST /_emu/v1/time/reset"},
	} {
		t.Run(tt.cmd, func(t *testing.T) {
			gw, requests := clockAPIGateway(t)
			out, err := gcpemu(t, "--data-dir", dataDir(t, gw), "time", tt.cmd)
			if err != nil {
				t.Fatal(err)
			}
			if got := requests(); len(got) != 1 || got[0] != tt.want {
				t.Errorf("requests = %q, want [%q]", got, tt.want)
			}
			if !strings.Contains(out, "2026-11-17T12:00:00Z") || !strings.Contains(out, "2h0m0s") {
				t.Errorf("output = %q, want the clock's now and offset", out)
			}
		})
	}
}

// TestTimeResetHelpSaysWhatStaysDone is #1172: the help says a reset only
// moves the clock, and points at snapshots to undo what an advance did.
func TestTimeResetHelpSaysWhatStaysDone(t *testing.T) {
	out, err := gcpemu(t, "time", "reset", "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"stays done", "snapshot restore"} {
		if !strings.Contains(out, want) {
			t.Errorf("help lacks %q:\n%s", want, out)
		}
	}
}
