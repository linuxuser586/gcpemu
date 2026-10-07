package pubsub_test

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"

	"github.com/linuxuser586/gcpemu/emutest"
)

// BenchmarkPublishPull measures publish-to-pull throughput of 1 KB messages
// through the official client over PUBSUB_EMULATOR_HOST (NFR-PERF-006,
// target ≥ 10,000 msg/s).
func BenchmarkPublishPull(b *testing.B) {
	rate := publishPull(b, b.N, b.ResetTimer)
	b.StopTimer()
	b.ReportMetric(rate, "msg/s")
}

// TestPublishPullTarget asserts NFR-PERF-006 when GCPEMU_PERF_TESTS=1.
func TestPublishPullTarget(t *testing.T) {
	if os.Getenv("GCPEMU_PERF_TESTS") != "1" {
		t.Skip("set GCPEMU_PERF_TESTS=1 to check NFR-PERF-006")
	}
	rate := publishPull(t, 50000, func() {})
	t.Logf("publish-to-pull %.0f msg/s", rate)
	if rate < 10000 {
		t.Errorf("NFR-PERF-006: %.0f msg/s, want >= 10,000", rate)
	}
}

// publishPull publishes n 1 KB messages and returns the rate at which they
// are received; reset is called when timing starts.
func publishPull(b testing.TB, n int, reset func()) float64 {
	inst := emutest.Start(b, []string{"pubsub"})
	b.Setenv("PUBSUB_EMULATOR_HOST", inst.Endpoint("pubsub"))
	ctx := context.Background()
	c, err := pubsub.NewClient(ctx, project)
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	if _, err := c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topicName("bench")}); err != nil {
		b.Fatal(err)
	}
	if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: subName("bench"), Topic: topicName("bench")}); err != nil {
		b.Fatal(err)
	}
	payload := make([]byte, 1024)

	rctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var received atomic.Int64
	done := make(chan error, 1)
	s := c.Subscriber(subName("bench"))
	s.ReceiveSettings.MaxOutstandingMessages = 10000
	s.ReceiveSettings.NumGoroutines = 4
	go func() {
		done <- s.Receive(rctx, func(_ context.Context, m *pubsub.Message) {
			m.Ack()
			if received.Add(1) == int64(n) {
				cancel()
			}
		})
	}()

	reset()
	start := time.Now()
	p := c.Publisher(topicName("bench"))
	results := make([]*pubsub.PublishResult, n)
	for i := range n {
		results[i] = p.Publish(ctx, &pubsub.Message{Data: payload})
	}
	for _, r := range results {
		if _, err := r.Get(ctx); err != nil {
			b.Fatal(err)
		}
	}
	p.Stop()
	if err := <-done; err != nil {
		b.Fatal(err)
	}
	el := time.Since(start)
	if got := received.Load(); got < int64(n) {
		b.Fatalf("received %d of %d", got, n)
	}
	return float64(n) / el.Seconds()
}
