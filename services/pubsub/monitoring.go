package pubsub

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/transcode"
)

// Cloud Monitoring's subscription metrics (#347). GCP reports a
// subscription's backlog only as Monitoring metrics, so Pub/Sub serves
// google.monitoring.v3.MetricService.ListTimeSeries for them on the gateway
// (gRPC, and REST under /monitoring/):
//
//	pubsub.googleapis.com/subscription/num_undelivered_messages    GAUGE
//	pubsub.googleapis.com/subscription/oldest_unacked_message_age  GAUGE, s
//	pubsub.googleapis.com/subscription/dead_letter_message_count   DELTA
//
// The points replay backlog samples taken every sampleEvery of the
// Emulator clock and kept for sampleHistory, and come out newest first. An
// interval that ends within the last sampleEvery and after the last sample
// also gets a point for the backlog now, stamped with the interval's end if
// that is earlier, so that a client polling for a backlog to drain need
// not wait for the next sample. As in GCP, a series with no points in the
// interval is left out. Filters are an AND of equalities; aggregation is
// not supported, and every series comes back in one page.

const (
	sampleEvery   = 10 * time.Second
	sampleHistory = time.Hour

	metricPrefix = "pubsub.googleapis.com/subscription/"
)

// backlogSample is one subscription's backlog at a point in time.
type backlogSample struct {
	at          time.Time
	undelivered int64
	// oldestAge is the age in seconds of the oldest unacked message.
	oldestAge int64
	// deadLettered counts the messages dead-lettered since the Instance
	// started; dead_letter_message_count reports its increase.
	deadLettered int64
}

// subMetrics describes the metrics served, by type without metricPrefix.
var subMetrics = map[string]struct {
	kind  metric.MetricDescriptor_MetricKind
	unit  string
	value func(b backlogSample) int64
}{
	"num_undelivered_messages":   {metric.MetricDescriptor_GAUGE, "1", func(b backlogSample) int64 { return b.undelivered }},
	"oldest_unacked_message_age": {metric.MetricDescriptor_GAUGE, "s", func(b backlogSample) int64 { return b.oldestAge }},
	"dead_letter_message_count":  {metric.MetricDescriptor_DELTA, "1", func(b backlogSample) int64 { return b.deadLettered }},
}

// backlogOf samples sb's backlog at now; callers hold s.mu.
func backlogOf(name string, sb *sub, now time.Time) backlogSample {
	st := statsOf(name, sb)
	b := backlogSample{at: now, undelivered: int64(st.Backlog), deadLettered: st.DeadLettered}
	if !st.OldestUnackedPublishTime.IsZero() {
		b.oldestAge = max(0, int64(now.Sub(st.OldestUnackedPublishTime)/time.Second))
	}
	return b
}

// sample records the backlog of every subscription whose last sample is
// sampleEvery old, or that has none, and drops samples older than
// sampleHistory.
func (s *Service) sample(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, sb := range s.subs {
		if n := len(sb.samples); n > 0 && now.Sub(sb.samples[n-1].at) < sampleEvery {
			continue
		}
		i := 0
		for i < len(sb.samples) && now.Sub(sb.samples[i].at) > sampleHistory {
			i++
		}
		sb.samples = append(sb.samples[i:], backlogOf(name, sb, now))
	}
}

// monitoringHandler serves the REST surface of MetricService.
func (s *Service) monitoringHandler() http.Handler {
	t := transcode.New()
	monitoringpb.RegisterMetricServiceServer(t, &metricServer{s: s})
	return t
}

type metricServer struct {
	monitoringpb.UnimplementedMetricServiceServer
	s *Service
}

func (m *metricServer) ListTimeSeries(ctx context.Context, req *monitoringpb.ListTimeSeriesRequest) (*monitoringpb.ListTimeSeriesResponse, error) {
	s := m.s
	project, ok := strings.CutPrefix(req.GetName(), "projects/")
	if !ok || project == "" || strings.Contains(project, "/") {
		return nil, apierr.Unimplemented("gcpemu serves time series of projects only, not %q.", req.GetName())
	}
	if err := s.env.Auth.Check(ctx, "monitoring.timeSeries.list", projectResource(project)); err != nil {
		return nil, err
	}
	for _, a := range []*monitoringpb.Aggregation{req.GetAggregation(), req.GetSecondaryAggregation()} {
		if a.GetPerSeriesAligner() != monitoringpb.Aggregation_ALIGN_NONE || a.GetCrossSeriesReducer() != monitoringpb.Aggregation_REDUCE_NONE {
			return nil, apierr.Unimplemented("gcpemu does not align or reduce time series; omit aggregation.")
		}
	}
	f, err := parseMetricFilter(req.GetFilter())
	if err != nil {
		return nil, err
	}
	typ := f["metric.type"]
	if typ == "" {
		return nil, apierr.InvalidArgument("The filter must name exactly one metric.type.")
	}
	id, ok := strings.CutPrefix(typ, metricPrefix)
	def, served := subMetrics[id]
	if !ok || !served {
		var ids []string
		for id := range subMetrics {
			ids = append(ids, metricPrefix+id)
		}
		sort.Strings(ids)
		return nil, apierr.NotFound("Cannot find metric(s) that match type = %q. gcpemu serves %s.", typ, strings.Join(ids, ", "))
	}
	end := req.GetInterval().GetEndTime()
	if end == nil {
		return nil, apierr.InvalidArgument("Missing required field: interval.end_time.")
	}
	iv := interval{end: end.AsTime()}
	iv.start = iv.end
	if st := req.GetInterval().GetStartTime(); st != nil {
		iv.start = st.AsTime()
	}
	if iv.start.After(iv.end) {
		return nil, apierr.InvalidArgument("The start time must be before the end time (%s) for the interval.", iv.end.Format(time.RFC3339Nano))
	}
	resp := &monitoringpb.ListTimeSeriesResponse{}
	if rt, ok := f["resource.type"]; ok && rt != "pubsub_subscription" {
		return resp, nil
	}
	for _, k := range []string{"resource.labels.project_id", "project"} {
		if p, ok := f[k]; ok && p != project {
			return resp, nil
		}
	}
	subID, bySub := f["resource.labels.subscription_id"]

	now := s.env.Clock.Now()
	type series struct {
		id      string
		samples []backlogSample
	}
	var all []series
	s.mu.Lock()
	for name, sb := range s.subs {
		if projectOf(name) != project {
			continue
		}
		sid := lastSegment(name)
		if bySub && sid != subID {
			continue
		}
		samples := append([]backlogSample(nil), sb.samples...)
		if stamp := iv.liveStamp(now); now.Sub(stamp) < sampleEvery &&
			(len(samples) == 0 || stamp.After(samples[len(samples)-1].at)) && iv.contains(stamp) {
			live := backlogOf(name, sb, now)
			live.at = stamp
			samples = append(samples, live)
		}
		all = append(all, series{sid, samples})
	}
	s.mu.Unlock()
	sort.Slice(all, func(i, j int) bool { return all[i].id < all[j].id })

	for _, se := range all {
		ts := &monitoringpb.TimeSeries{
			Metric:     &metric.Metric{Type: typ},
			Resource:   &monitoredres.MonitoredResource{Type: "pubsub_subscription", Labels: map[string]string{"project_id": project, "subscription_id": se.id}},
			MetricKind: def.kind,
			ValueType:  metric.MetricDescriptor_INT64,
			Unit:       def.unit,
		}
		for i := len(se.samples) - 1; i >= 0; i-- {
			b := se.samples[i]
			if !iv.contains(b.at) {
				continue
			}
			p := &monitoringpb.Point{Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(b.at), EndTime: timestamppb.New(b.at)}}
			v := def.value(b)
			if def.kind == metric.MetricDescriptor_DELTA {
				if i == 0 {
					continue
				}
				prev := se.samples[i-1]
				p.Interval.StartTime = timestamppb.New(prev.at)
				v -= def.value(prev)
			}
			p.Value = &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: v}}
			ts.Points = append(ts.Points, p)
		}
		if len(ts.Points) == 0 {
			continue
		}
		if req.GetView() == monitoringpb.ListTimeSeriesRequest_HEADERS {
			ts.Points = nil
		}
		resp.TimeSeries = append(resp.TimeSeries, ts)
	}
	return resp, nil
}

// interval is a read interval: (start, end], or the instant end when
// start equals end.
type interval struct{ start, end time.Time }

func (iv interval) contains(t time.Time) bool {
	if iv.start.Equal(iv.end) {
		return t.Equal(iv.end)
	}
	return t.After(iv.start) && !t.After(iv.end)
}

// liveStamp is the time a point for the backlog now is stamped with.
func (iv interval) liveStamp(now time.Time) time.Time {
	if iv.end.Before(now) {
		return iv.end
	}
	return now
}

// parseMetricFilter parses a Monitoring filter that is an AND of
// key = "value" terms into its terms. resource.label.K is read as
// resource.labels.K.
func parseMetricFilter(filter string) (map[string]string, error) {
	bad := func(format string, args ...any) error {
		return apierr.InvalidArgument("Invalid filter %q: "+format+" gcpemu supports an AND of metric.type, resource.type, resource.labels.project_id, resource.labels.subscription_id and project = \"value\" terms.", append([]any{filter}, args...)...)
	}
	out := map[string]string{}
	rest := strings.TrimSpace(filter)
	for {
		i := strings.IndexByte(rest, '=')
		if i < 0 {
			return nil, bad("expected key = \"value\" at %q.", rest)
		}
		key := strings.TrimSpace(rest[:i])
		if k, ok := strings.CutPrefix(key, "resource.label."); ok {
			key = "resource.labels." + k
		}
		switch key {
		case "metric.type", "resource.type", "resource.labels.project_id", "resource.labels.subscription_id", "project":
		default:
			return nil, bad("unsupported term %q.", key)
		}
		if _, dup := out[key]; dup {
			return nil, bad("%s appears twice.", key)
		}
		rest = strings.TrimSpace(rest[i+1:])
		if !strings.HasPrefix(rest, `"`) {
			return nil, bad("%s must be compared with a quoted string.", key)
		}
		var val strings.Builder
		j := 1
		for ; j < len(rest) && rest[j] != '"'; j++ {
			if rest[j] == '\\' && j+1 < len(rest) {
				j++
			}
			val.WriteByte(rest[j])
		}
		if j >= len(rest) {
			return nil, bad("unterminated string.")
		}
		out[key] = val.String()
		rest = strings.TrimSpace(rest[j+1:])
		if rest == "" {
			return out, nil
		}
		next, ok := strings.CutPrefix(rest, "AND")
		if !ok || (next != "" && next[0] != ' ' && next[0] != '\t' && next[0] != '\n') {
			return nil, bad("expected AND at %q.", rest)
		}
		rest = strings.TrimSpace(next)
	}
}
