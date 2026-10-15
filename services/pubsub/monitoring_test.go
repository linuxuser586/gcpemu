package pubsub_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"testing"
	"time"

	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/linuxuser586/gcpemu/emutest"
)

const (
	undelivered = "pubsub.googleapis.com/subscription/num_undelivered_messages"
	oldestAge   = "pubsub.googleapis.com/subscription/oldest_unacked_message_age"
	deadLetters = "pubsub.googleapis.com/subscription/dead_letter_message_count"
)

// TestMonitoringREST reads a subscription's backlog as Cloud Monitoring
// time series through the gateway, as projects.timeSeries.list does with
// CLOUDSDK_API_ENDPOINT_OVERRIDES_MONITORING.
func TestMonitoringREST(t *testing.T) {
	inst := emutest.Start(t, []string{"pubsub"})
	v1 := inst.GatewayURL() + "/pubsub/v1/projects/" + project
	restCall(t, http.MethodPut, v1+"/topics/mon", nil, 200)
	restCall(t, http.MethodPut, v1+"/subscriptions/mon-sub", map[string]any{"topic": topicName("mon")}, 200)
	restCall(t, http.MethodPut, v1+"/subscriptions/other-sub", map[string]any{"topic": topicName("mon")}, 200)
	data := base64.StdEncoding.EncodeToString([]byte("x"))
	restCall(t, http.MethodPost, v1+"/topics/mon:publish", map[string]any{"messages": []map[string]any{{"data": data}, {"data": data}}}, 200)

	end := time.Now().Add(time.Hour).UTC()
	list := func(filter string, wantStatus int) map[string]any {
		t.Helper()
		q := url.Values{
			"filter":             {filter},
			"interval.startTime": {end.Add(-2 * time.Hour).Format(time.RFC3339Nano)},
			"interval.endTime":   {end.Format(time.RFC3339Nano)},
		}
		return restCall(t, http.MethodGet, inst.GatewayURL()+"/monitoring/v3/projects/"+project+"/timeSeries?"+q.Encode(), nil, wantStatus)
	}
	series := func(out map[string]any) []map[string]any {
		var ss []map[string]any
		all, _ := out["timeSeries"].([]any)
		for _, s := range all {
			ss = append(ss, s.(map[string]any))
		}
		return ss
	}
	latest := func(s map[string]any) string {
		t.Helper()
		ps := s["points"].([]any)
		if len(ps) == 0 {
			t.Fatalf("no points in %v", s)
		}
		return ps[0].(map[string]any)["value"].(map[string]any)["int64Value"].(string)
	}

	out := list(`metric.type="`+undelivered+`" AND resource.labels.subscription_id="mon-sub"`, 200)
	ss := series(out)
	if len(ss) != 1 {
		t.Fatalf("series = %v", out)
	}
	s := ss[0]
	res := s["resource"].(map[string]any)
	labels := res["labels"].(map[string]any)
	if res["type"] != "pubsub_subscription" || labels["subscription_id"] != "mon-sub" || labels["project_id"] != project {
		t.Errorf("resource = %v", res)
	}
	if s["metric"].(map[string]any)["type"] != undelivered || s["metricKind"] != "GAUGE" || s["valueType"] != "INT64" {
		t.Errorf("series = %v", s)
	}
	if got := latest(s); got != "2" {
		t.Errorf("num_undelivered_messages = %s, want 2", got)
	}

	// Every subscription of the Project when no subscription_id is given.
	if ss := series(list(`metric.type = "`+undelivered+`"`, 200)); len(ss) != 2 {
		t.Errorf("%d series, want 2", len(ss))
	}
	age := series(list(`metric.type="`+oldestAge+`" AND resource.type="pubsub_subscription" AND resource.label.subscription_id="mon-sub"`, 200))
	if len(age) != 1 || age[0]["metricKind"] != "GAUGE" || age[0]["unit"] != "s" {
		t.Fatalf("oldest_unacked_message_age = %v", age)
	}
	latest(age[0])
	// A delta needs the baseline sample the Service takes of a new
	// subscription on its next tick.
	var dl []map[string]any
	for deadline := time.Now().Add(5 * time.Second); len(dl) == 0 && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		dl = series(list(`metric.type="`+deadLetters+`" AND resource.labels.subscription_id="mon-sub"`, 200))
	}
	if len(dl) != 1 || dl[0]["metricKind"] != "DELTA" || latest(dl[0]) != "0" {
		t.Fatalf("dead_letter_message_count = %v", dl)
	}

	if out := list(`metric.type="`+undelivered+`" AND resource.labels.subscription_id="nope"`, 200); out["timeSeries"] != nil {
		t.Errorf("unknown subscription: %v", out)
	}
	list(`resource.labels.subscription_id="mon-sub"`, 400)
	list(`metric.type="`+undelivered+`" OR resource.labels.subscription_id="mon-sub"`, 400)
	list(`metric.type="compute.googleapis.com/instance/cpu/utilization"`, 404)
}

// TestMonitoringGRPC reads the same metrics with the Cloud Monitoring Go
// client, which speaks gRPC to the gateway.
func TestMonitoringGRPC(t *testing.T) {
	inst := emutest.Start(t, []string{"pubsub"})
	v1 := inst.GatewayURL() + "/pubsub/v1/projects/" + project
	restCall(t, http.MethodPut, v1+"/topics/g-topic", nil, 200)
	restCall(t, http.MethodPut, v1+"/subscriptions/g-sub", map[string]any{"topic": topicName("g-topic")}, 200)
	data := base64.StdEncoding.EncodeToString([]byte("x"))
	restCall(t, http.MethodPost, v1+"/topics/g-topic:publish", map[string]any{"messages": []map[string]any{{"data": data}}}, 200)

	ctx := context.Background()
	c, err := monitoring.NewMetricClient(ctx,
		option.WithEndpoint(inst.Endpoint("gateway")),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	now := time.Now()
	it := c.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{
		Name:   "projects/" + project,
		Filter: `metric.type="` + undelivered + `" AND resource.labels.subscription_id="g-sub"`,
		Interval: &monitoringpb.TimeInterval{
			StartTime: timestamppb.New(now.Add(-time.Minute)),
			EndTime:   timestamppb.New(now.Add(time.Minute)),
		},
	})
	ts, err := it.Next()
	if err != nil {
		t.Fatal(err)
	}
	if got := ts.GetPoints()[0].GetValue().GetInt64Value(); got != 1 {
		t.Errorf("num_undelivered_messages = %d, want 1", got)
	}
	if _, err := it.Next(); err != iterator.Done {
		t.Errorf("second series: %v", err)
	}
}
