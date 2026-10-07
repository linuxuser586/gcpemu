package pubsub_test

import (
	"context"
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
	n := b.N

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

	b.ResetTimer()
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
	b.StopTimer()
	if got := received.Load(); got < int64(n) {
		b.Fatalf("received %d of %d", got, n)
	}
	b.ReportMetric(float64(n)/el.Seconds(), "msg/s")
}
