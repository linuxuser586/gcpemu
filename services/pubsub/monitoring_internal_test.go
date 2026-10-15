package pubsub

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"
)

// TestMonitoringHistory checks that the time series replay the backlog
// samples within the interval, newest first, and that
// dead_letter_message_count reports what each sampling period added.
func TestMonitoringHistory(t *testing.T) {
	svc, h, clk, closeFn := openServiceClock(t, filepath.Join(t.TempDir(), "state.db"))
	defer closeFn()
	mh := svc.monitoringHandler()
	const v1 = "/v1/projects/mon-proj"
	do(t, h, http.MethodPut, v1+"/topics/mon-topic", nil)
	do(t, h, http.MethodPut, v1+"/subscriptions/mon-sub", map[string]any{"topic": "projects/mon-proj/topics/mon-topic"})
	data := base64.StdEncoding.EncodeToString([]byte("x"))
	do(t, h, http.MethodPost, v1+"/topics/mon-topic:publish", map[string]any{"messages": []map[string]any{{"data": data}, {"data": data}, {"data": data}}})

	t0 := clk.Now()
	svc.sample(t0)
	clk.Advance(time.Minute)
	pulled := do(t, h, http.MethodPost, v1+"/subscriptions/mon-sub:pull", map[string]any{"maxMessages": 2, "returnImmediately": true})
	var ids []any
	for _, m := range pulled["receivedMessages"].([]any) {
		ids = append(ids, m.(map[string]any)["ackId"])
	}
	do(t, h, http.MethodPost, v1+"/subscriptions/mon-sub:acknowledge", map[string]any{"ackIds": ids})
	svc.mu.Lock()
	svc.subs["projects/mon-proj/subscriptions/mon-sub"].deadLettered = 2
	svc.mu.Unlock()
	t1 := clk.Now()
	svc.sample(t1)
	clk.Advance(time.Minute)
	t2 := clk.Now()
	svc.sample(t2)

	list := func(metric string, start, end time.Time) []any {
		t.Helper()
		q := url.Values{
			"filter":             {`metric.type="pubsub.googleapis.com/subscription/` + metric + `" AND resource.labels.subscription_id="mon-sub"`},
			"interval.startTime": {start.Format(time.RFC3339Nano)},
			"interval.endTime":   {end.Format(time.RFC3339Nano)},
		}
		out := do(t, mh, http.MethodGet, "/v3/projects/mon-proj/timeSeries?"+q.Encode(), nil)
		ss := out["timeSeries"].([]any)
		if len(ss) != 1 {
			t.Fatalf("series = %v", out)
		}
		return ss[0].(map[string]any)["points"].([]any)
	}
	type pt struct {
		start, end time.Time
		v          string
	}
	points := func(ps []any) []pt {
		var out []pt
		for _, p := range ps {
			p := p.(map[string]any)
			iv := p["interval"].(map[string]any)
			e, _ := time.Parse(time.RFC3339Nano, iv["endTime"].(string))
			s, _ := time.Parse(time.RFC3339Nano, iv["startTime"].(string))
			out = append(out, pt{s, e, p["value"].(map[string]any)["int64Value"].(string)})
		}
		return out
	}

	got := points(list("num_undelivered_messages", t0.Add(-time.Second), t2))
	if len(got) != 3 || got[0].v != "1" || got[1].v != "1" || got[2].v != "3" ||
		!got[0].end.Equal(t2) || !got[1].end.Equal(t1) || !got[2].end.Equal(t0) || !got[0].start.Equal(t2) {
		t.Fatalf("num_undelivered_messages = %+v", got)
	}
	// The start time is excluded.
	if got := points(list("num_undelivered_messages", t1, t2)); len(got) != 1 || !got[0].end.Equal(t2) {
		t.Errorf("(t1, t2] = %+v", got)
	}

	age := points(list("oldest_unacked_message_age", t0.Add(-time.Second), t2))
	if len(age) != 3 || age[0].v != "120" || age[1].v != "60" || age[2].v != "0" {
		t.Errorf("oldest_unacked_message_age = %+v", age)
	}

	// The first sample is the baseline: a delta needs the one before it.
	dl := points(list("dead_letter_message_count", t0.Add(-time.Second), t2))
	if len(dl) != 2 || dl[0].v != "0" || dl[1].v != "2" || !dl[1].start.Equal(t0) || !dl[1].end.Equal(t1) {
		t.Errorf("dead_letter_message_count = %+v", dl)
	}

	// An interval reaching past the last sample ends with the backlog now.
	clk.Advance(time.Minute)
	live := points(list("num_undelivered_messages", t2, clk.Now().Add(time.Hour)))
	if len(live) != 1 || live[0].v != "1" || !live[0].end.After(t2) {
		t.Errorf("live = %+v", live)
	}

	// A series with no points in the interval is left out, and a past
	// interval gets no point for the backlog now.
	q := url.Values{
		"filter":             {`metric.type="pubsub.googleapis.com/subscription/num_undelivered_messages"`},
		"interval.startTime": {t0.Add(-time.Hour).Format(time.RFC3339Nano)},
		"interval.endTime":   {t0.Add(-time.Minute).Format(time.RFC3339Nano)},
	}
	do(t, h, http.MethodPut, v1+"/subscriptions/new-sub", map[string]any{"topic": "projects/mon-proj/topics/mon-topic"})
	if out := do(t, mh, http.MethodGet, "/v3/projects/mon-proj/timeSeries?"+q.Encode(), nil); out["timeSeries"] != nil {
		t.Errorf("past interval: %v", out)
	}

	if got := svc.EnvVars("127.0.0.1:1", nil)["CLOUDSDK_API_ENDPOINT_OVERRIDES_MONITORING"]; got != "http://127.0.0.1:1/monitoring/" {
		t.Errorf("CLOUDSDK_API_ENDPOINT_OVERRIDES_MONITORING = %q", got)
	}
}
