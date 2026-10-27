package instance

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// condSvc evaluates a "fake/thing" Wait condition from a fixed table of
// states by NAME; any other NAME is not found. LOCATION "bad" is a target
// it can never evaluate.
type condSvc struct {
	fakeSvc
	states map[string]emu.WaitStatus
}

func (s *condSvc) WaitConditions() map[string]emu.WaitCondition {
	return map[string]emu.WaitCondition{
		"fake/thing": func(_ context.Context, t emu.WaitTarget) (emu.WaitStatus, error) {
			if t.Location == "bad" {
				return emu.WaitStatus{}, errors.New("location bad is not a fake location")
			}
			if st, ok := s.states[t.Name]; ok {
				return st, nil
			}
			return emu.WaitStatus{State: emu.WaitPending, Reason: "not found"}, nil
		},
	}
}

// TestConditions is FR-CI-007: GET /_emu/v1/conditions reports each Wait
// condition's state, with a reason for one that is not met, and refuses a
// malformed one. The Admin API is not subject to IAM, so it answers under
// --iam-mode=enforce with no grants.
func TestConditions(t *testing.T) {
	cfg := testConfig()
	cfg.Ephemeral = true
	cfg.DataDir = t.TempDir()
	cfg.Services = []string{"dns"}
	cfg.Ports["gateway"] = 0
	cfg.IAMMode = config.IAMEnforce
	svc := &condSvc{states: map[string]emu.WaitStatus{
		"up":   {State: emu.WaitMet},
		"busy": {State: emu.WaitPending, Reason: "status PROVISIONING"},
		"dead": {State: emu.WaitFailed, Reason: "status ERROR"},
	}}
	in, err := New(&cfg, map[string]Factory{"dns": func(*emu.Env) emu.Service { return svc }}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := in.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = in.Shutdown(ctx) })
	base := "http://" + in.Env.Endpoints.Get("gateway") + "/_emu/v1/conditions"

	type cond struct{ For, State, Reason string }
	query := func(fors ...string) (int, bool, []cond, string) {
		t.Helper()
		q := url.Values{"for": fors}
		resp, body := call(t, http.DefaultClient, "GET", base+"?"+q.Encode(), nil)
		var out struct {
			Met        bool
			Conditions []cond
			Error      string
		}
		_ = json.Unmarshal([]byte(body), &out)
		if out.Error != "" {
			body = out.Error
		}
		return resp.StatusCode, out.Met, out.Conditions, body
	}

	code, met, conds, body := query("fake/thing=p/l/up")
	if code != 200 || !met || len(conds) != 1 || conds[0] != (cond{For: "fake/thing=p/l/up", State: "met"}) {
		t.Errorf("one met: %d %s", code, body)
	}
	code, met, conds, body = query("fake/thing=p/l/up", "fake/thing=p/l/busy", "fake/thing=p/l/gone", "fake/thing=p/l/dead")
	want := []cond{
		{"fake/thing=p/l/up", "met", ""},
		{"fake/thing=p/l/busy", "pending", "status PROVISIONING"},
		{"fake/thing=p/l/gone", "pending", "not found"},
		{"fake/thing=p/l/dead", "failed", "status ERROR"},
	}
	if code != 200 || met || len(conds) != len(want) {
		t.Fatalf("several: %d %s", code, body)
	}
	for i := range want {
		if conds[i] != want[i] {
			t.Errorf("condition %d = %+v, want %+v", i, conds[i], want[i])
		}
	}

	for _, tc := range []struct {
		fors []string
		want string
	}{
		{nil, "for=KIND=TARGET"},
		{[]string{"nonsense"}, `"nonsense"`},
		{[]string{"fake/thing=p/l"}, "PROJECT/LOCATION/NAME"},
		{[]string{"fake/thing=p/l/n/x"}, "PROJECT/LOCATION/NAME"},
		{[]string{"fake/thing=p//n"}, "PROJECT/LOCATION/NAME"},
		{[]string{"other/kind=p/l/n"}, "fake/thing"},
		{[]string{"fake/thing=p/bad/n"}, "location bad"},
	} {
		code, _, _, body := query(tc.fors...)
		if code != 400 || !strings.Contains(body, tc.want) {
			t.Errorf("%q: %d %s, want 400 naming %s", tc.fors, code, body, tc.want)
		}
	}
}
