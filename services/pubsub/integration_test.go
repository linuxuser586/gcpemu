package pubsub_test

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/linuxuser586/gcpemu/emutest"
)

const project = "test-proj"

func topicName(id string) string { return "projects/" + project + "/topics/" + id }
func subName(id string) string   { return "projects/" + project + "/subscriptions/" + id }

// newEmulatorClient starts an instance and returns a client connected via
// PUBSUB_EMULATOR_HOST (FR-PS-001).
func newEmulatorClient(t *testing.T) (*emutest.Instance, *pubsub.Client) {
	t.Helper()
	inst := emutest.Start(t, []string{"pubsub"})
	t.Setenv("PUBSUB_EMULATOR_HOST", inst.Endpoint("pubsub"))
	c, err := pubsub.NewClient(context.Background(), project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return inst, c
}

// newGatewayClient returns a client that reaches the gateway with an
// explicit endpoint (FR-CORE-040).
func newGatewayClient(t *testing.T, inst *emutest.Instance) *pubsub.Client {
	t.Helper()
	t.Setenv("PUBSUB_EMULATOR_HOST", "")
	c, err := pubsub.NewClient(context.Background(), project,
		option.WithEndpoint(inst.Endpoint("gateway")),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func ctxT(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func mustTopic(t *testing.T, c *pubsub.Client, id string) {
	t.Helper()
	if _, err := c.TopicAdminClient.CreateTopic(context.Background(), &pubsubpb.Topic{Name: topicName(id)}); err != nil {
		t.Fatal(err)
	}
}

func mustSub(t *testing.T, c *pubsub.Client, s *pubsubpb.Subscription) *pubsubpb.Subscription {
	t.Helper()
	out, err := c.SubscriptionAdminClient.CreateSubscription(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func publish(t *testing.T, c *pubsub.Client, topic string, msgs ...*pubsub.Message) []string {
	t.Helper()
	p := c.Publisher(topic)
	defer p.Stop()
	var results []*pubsub.PublishResult
	for _, m := range msgs {
		results = append(results, p.Publish(context.Background(), m))
	}
	var ids []string
	for _, r := range results {
		id, err := r.Get(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func pull(t *testing.T, c *pubsub.Client, sub string, max int32) []*pubsubpb.ReceivedMessage {
	t.Helper()
	resp, err := c.SubscriptionAdminClient.Pull(context.Background(), &pubsubpb.PullRequest{
		Subscription: sub, MaxMessages: max, ReturnImmediately: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp.ReceivedMessages
}

// pullWait pulls until at least one message arrives or the timeout passes.
func pullWait(t *testing.T, c *pubsub.Client, sub string, max int32, timeout time.Duration) []*pubsubpb.ReceivedMessage {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if got := pull(t, c, sub, max); len(got) > 0 || time.Now().After(deadline) {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func ack(t *testing.T, c *pubsub.Client, sub string, ms ...*pubsubpb.ReceivedMessage) {
	t.Helper()
	var ids []string
	for _, m := range ms {
		ids = append(ids, m.AckId)
	}
	if err := c.SubscriptionAdminClient.Acknowledge(context.Background(), &pubsubpb.AcknowledgeRequest{Subscription: sub, AckIds: ids}); err != nil {
		t.Fatal(err)
	}
}

func modack(t *testing.T, c *pubsub.Client, sub string, secs int32, ms ...*pubsubpb.ReceivedMessage) {
	t.Helper()
	var ids []string
	for _, m := range ms {
		ids = append(ids, m.AckId)
	}
	if err := c.SubscriptionAdminClient.ModifyAckDeadline(context.Background(), &pubsubpb.ModifyAckDeadlineRequest{Subscription: sub, AckIds: ids, AckDeadlineSeconds: secs}); err != nil {
		t.Fatal(err)
	}
}

// receiveN runs Receive until n messages were acked and returns them.
func receiveN(t *testing.T, c *pubsub.Client, sub string, n int, settings *pubsub.ReceiveSettings) []*pubsub.Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := c.Subscriber(sub)
	if settings != nil {
		s.ReceiveSettings = *settings
	}
	var mu sync.Mutex
	var got []*pubsub.Message
	err := s.Receive(ctx, func(_ context.Context, m *pubsub.Message) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, m)
		m.Ack()
		if len(got) == n {
			cancel()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < n {
		t.Fatalf("received %d messages, want %d", len(got), n)
	}
	return got
}

func testPublishReceive(t *testing.T, c *pubsub.Client) {
	mustTopic(t, c, "orders")
	mustSub(t, c, &pubsubpb.Subscription{Name: subName("orders-sub"), Topic: topicName("orders")})
	const n = 200
	var msgs []*pubsub.Message
	for i := range n {
		msgs = append(msgs, &pubsub.Message{Data: fmt.Appendf(nil, "msg-%d", i), Attributes: map[string]string{"i": fmt.Sprint(i)}})
	}
	ids := publish(t, c, topicName("orders"), msgs...)
	if len(ids) != n || ids[0] == "" {
		t.Fatalf("ids = %v", ids)
	}
	got := receiveN(t, c, subName("orders-sub"), n, nil)
	seen := map[string]bool{}
	for _, m := range got {
		if string(m.Data) != "msg-"+m.Attributes["i"] {
			t.Errorf("data %q attrs %v", m.Data, m.Attributes)
		}
		if m.PublishTime.IsZero() || m.ID == "" {
			t.Errorf("missing id/publish time: %+v", m)
		}
		seen[m.ID] = true
	}
	if len(seen) != n {
		t.Errorf("distinct messages = %d", len(seen))
	}
	// Everything was acked: nothing left.
	if left := pull(t, c, subName("orders-sub"), 10); len(left) != 0 {
		t.Errorf("leftover messages: %d", len(left))
	}
}

func TestPublishReceiveEmulatorHost(t *testing.T) {
	_, c := newEmulatorClient(t)
	testPublishReceive(t, c)
}

func TestPublishReceiveGateway(t *testing.T) {
	inst := emutest.Start(t, []string{"pubsub"})
	testPublishReceive(t, newGatewayClient(t, inst))
}

func TestTopicSubscriptionAdmin(t *testing.T) {
	_, c := newEmulatorClient(t)
	ctx := ctxT(t, 10*time.Second)
	ta, sa := c.TopicAdminClient, c.SubscriptionAdminClient

	if _, err := ta.CreateTopic(ctx, &pubsubpb.Topic{Name: "projects/p/topics/x"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("short topic id: %v", err)
	}
	if _, err := ta.CreateTopic(ctx, &pubsubpb.Topic{Name: topicName("goog-topic")}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("goog prefix: %v", err)
	}
	tp, err := ta.CreateTopic(ctx, &pubsubpb.Topic{Name: topicName("topic-1"), Labels: map[string]string{"env": "dev"},
		MessageRetentionDuration: durationpb.New(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if tp.Labels["env"] != "dev" {
		t.Errorf("labels = %v", tp.Labels)
	}
	if _, err := ta.CreateTopic(ctx, &pubsubpb.Topic{Name: topicName("topic-1")}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("duplicate: %v", err)
	}
	for i := range 5 {
		mustTopic(t, c, fmt.Sprintf("page-%d", i))
	}
	// Pagination.
	var names []string
	it := ta.ListTopics(ctx, &pubsubpb.ListTopicsRequest{Project: "projects/" + project, PageSize: 2})
	for {
		tp, err := it.Next()
		if err != nil {
			break
		}
		names = append(names, tp.Name)
	}
	if len(names) != 6 || !sort.StringsAreSorted(names) {
		t.Errorf("listed %v", names)
	}

	// Update with mask.
	up, err := ta.UpdateTopic(ctx, &pubsubpb.UpdateTopicRequest{
		Topic:      &pubsubpb.Topic{Name: topicName("topic-1"), Labels: map[string]string{"env": "prod"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if up.Labels["env"] != "prod" || up.MessageRetentionDuration.AsDuration() != time.Hour {
		t.Errorf("updated = %v", up)
	}
	if _, err := ta.UpdateTopic(ctx, &pubsubpb.UpdateTopicRequest{Topic: &pubsubpb.Topic{Name: topicName("topic-1")}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty mask: %v", err)
	}

	// Subscriptions: defaults, update, list by topic, detach, delete.
	s1 := mustSub(t, c, &pubsubpb.Subscription{Name: subName("sub-1"), Topic: topicName("topic-1")})
	if s1.AckDeadlineSeconds != 10 || s1.MessageRetentionDuration.AsDuration() != 7*24*time.Hour || s1.State != pubsubpb.Subscription_ACTIVE {
		t.Errorf("defaults: %v", s1)
	}
	if s1.TopicMessageRetentionDuration.AsDuration() != time.Hour {
		t.Errorf("topic retention: %v", s1.TopicMessageRetentionDuration)
	}
	if _, err := sa.CreateSubscription(ctx, &pubsubpb.Subscription{Name: subName("sub-2"), Topic: topicName("missing")}); status.Code(err) != codes.NotFound {
		t.Errorf("missing topic: %v", err)
	}
	if _, err := sa.CreateSubscription(ctx, &pubsubpb.Subscription{Name: subName("sub-2"), Topic: topicName("topic-1"), AckDeadlineSeconds: 5}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad deadline: %v", err)
	}
	if _, err := sa.CreateSubscription(ctx, &pubsubpb.Subscription{Name: subName("sub-2"), Topic: topicName("topic-1"), Filter: `attributes.x = `}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad filter: %v", err)
	}
	mustSub(t, c, &pubsubpb.Subscription{Name: subName("sub-2"), Topic: topicName("topic-1")})
	us, err := sa.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
		Subscription: &pubsubpb.Subscription{Name: subName("sub-1"), AckDeadlineSeconds: 30, Labels: map[string]string{"a": "b"}},
		UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{"ack_deadline_seconds", "labels"}},
	})
	if err != nil || us.AckDeadlineSeconds != 30 || us.Labels["a"] != "b" {
		t.Fatalf("update sub: %v %v", us, err)
	}
	if _, err := sa.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
		Subscription: &pubsubpb.Subscription{Name: subName("sub-1"), Filter: `attributes:x`},
		UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{"filter"}},
	}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("filter update: %v", err)
	}
	var subs []string
	sit := ta.ListTopicSubscriptions(ctx, &pubsubpb.ListTopicSubscriptionsRequest{Topic: topicName("topic-1")})
	for {
		n, err := sit.Next()
		if err != nil {
			break
		}
		subs = append(subs, n)
	}
	if len(subs) != 2 {
		t.Errorf("topic subscriptions: %v", subs)
	}

	if _, err := ta.DetachSubscription(ctx, &pubsubpb.DetachSubscriptionRequest{Subscription: subName("sub-2")}); err != nil {
		t.Fatal(err)
	}
	got, _ := sa.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: subName("sub-2")})
	if !got.GetDetached() {
		t.Errorf("not detached: %v", got)
	}
	if _, err := sa.Pull(ctx, &pubsubpb.PullRequest{Subscription: subName("sub-2"), MaxMessages: 1, ReturnImmediately: true}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("pull detached: %v", err)
	}

	// Deleting the topic leaves the subscription with _deleted-topic_.
	if err := ta.DeleteTopic(ctx, &pubsubpb.DeleteTopicRequest{Topic: topicName("topic-1")}); err != nil {
		t.Fatal(err)
	}
	got, _ = sa.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: subName("sub-1")})
	if got.GetTopic() != "_deleted-topic_" {
		t.Errorf("topic after delete = %q", got.GetTopic())
	}
	if _, err := ta.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: topicName("topic-1")}); status.Code(err) != codes.NotFound {
		t.Errorf("get deleted: %v", err)
	}
	if err := sa.DeleteSubscription(ctx, &pubsubpb.DeleteSubscriptionRequest{Subscription: subName("sub-1")}); err != nil {
		t.Fatal(err)
	}
	if _, err := sa.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: subName("sub-1")}); status.Code(err) != codes.NotFound {
		t.Errorf("get deleted sub: %v", err)
	}
}

func TestPublishValidation(t *testing.T) {
	_, c := newEmulatorClient(t)
	mustTopic(t, c, "topic-v")
	ctx := ctxT(t, 10*time.Second)
	cases := []struct {
		name string
		msgs []*pubsubpb.PubsubMessage
	}{
		{"empty", []*pubsubpb.PubsubMessage{{}}},
		{"goog attr", []*pubsubpb.PubsubMessage{{Data: []byte("x"), Attributes: map[string]string{"googx": "1"}}}},
		{"long value", []*pubsubpb.PubsubMessage{{Data: []byte("x"), Attributes: map[string]string{"k": string(make([]byte, 1025))}}}},
		{"too many", make([]*pubsubpb.PubsubMessage, 1001)},
	}
	for _, tc := range cases {
		for i := range tc.msgs {
			if tc.msgs[i] == nil {
				tc.msgs[i] = &pubsubpb.PubsubMessage{Data: []byte("x")}
			}
		}
		_, err := c.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topicName("topic-v"), Messages: tc.msgs})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	if _, err := c.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topicName("nope"), Messages: []*pubsubpb.PubsubMessage{{Data: []byte("x")}}}); status.Code(err) != codes.NotFound {
		t.Errorf("missing topic: %v", err)
	}
}

func TestNackAndDeadlineExpiry(t *testing.T) {
	_, c := newEmulatorClient(t)
	mustTopic(t, c, "topic-t")
	sub := subName("sub-s")
	mustSub(t, c, &pubsubpb.Subscription{Name: sub, Topic: topicName("topic-t")})
	publish(t, c, topicName("topic-t"), &pubsub.Message{Data: []byte("hello")})

	first := pullWait(t, c, sub, 10, 5*time.Second)
	if len(first) != 1 {
		t.Fatalf("pulled %d", len(first))
	}
	// Leased: not redelivered.
	if again := pull(t, c, sub, 10); len(again) != 0 {
		t.Fatalf("redelivered while leased")
	}
	// Nack (deadline 0) → immediate redelivery with a new ack ID.
	modack(t, c, sub, 0, first...)
	second := pullWait(t, c, sub, 10, 5*time.Second)
	if len(second) != 1 || second[0].AckId == first[0].AckId || string(second[0].Message.Data) != "hello" {
		t.Fatalf("after nack: %v", second)
	}
	// Short deadline → expiry → redelivery.
	modack(t, c, sub, 1, second...)
	start := time.Now()
	third := pullWait(t, c, sub, 10, 5*time.Second)
	if len(third) != 1 {
		t.Fatalf("not redelivered after expiry")
	}
	if el := time.Since(start); el < 800*time.Millisecond {
		t.Errorf("redelivered after %v, before the deadline", el)
	}
	ack(t, c, sub, third...)
	time.Sleep(1200 * time.Millisecond)
	if left := pull(t, c, sub, 10); len(left) != 0 {
		t.Fatalf("redelivered after ack")
	}
}

func TestRetryPolicy(t *testing.T) {
	_, c := newEmulatorClient(t)
	mustTopic(t, c, "topic-t")
	sub := subName("sub-s")
	mustSub(t, c, &pubsubpb.Subscription{Name: sub, Topic: topicName("topic-t"), RetryPolicy: &pubsubpb.RetryPolicy{
		MinimumBackoff: durationpb.New(time.Second), MaximumBackoff: durationpb.New(2 * time.Second),
	}})
	publish(t, c, topicName("topic-t"), &pubsub.Message{Data: []byte("x")})
	m := pullWait(t, c, sub, 1, 5*time.Second)
	modack(t, c, sub, 0, m...)
	start := time.Now()
	if got := pull(t, c, sub, 1); len(got) != 0 {
		t.Fatal("redelivered before backoff")
	}
	got := pullWait(t, c, sub, 1, 5*time.Second)
	if len(got) != 1 || time.Since(start) < 800*time.Millisecond {
		t.Fatalf("redelivery %v after %v", got, time.Since(start))
	}
}

func TestOrderedDelivery(t *testing.T) {
	_, c := newEmulatorClient(t)
	mustTopic(t, c, "ord")
	sub := subName("ord-sub")
	s := mustSub(t, c, &pubsubpb.Subscription{Name: sub, Topic: topicName("ord"), EnableMessageOrdering: true})
	if !s.EnableMessageOrdering {
		t.Fatal("ordering not enabled")
	}
	p := c.Publisher(topicName("ord"))
	p.EnableMessageOrdering = true
	const perKey = 50
	keys := []string{"a", "b", "c"}
	var results []*pubsub.PublishResult
	for i := range perKey {
		for _, k := range keys {
			results = append(results, p.Publish(context.Background(), &pubsub.Message{Data: fmt.Appendf(nil, "%d", i), OrderingKey: k}))
		}
	}
	for _, r := range results {
		if _, err := r.Get(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	p.Stop()
	got := receiveN(t, c, sub, perKey*len(keys), nil)
	next := map[string]int{}
	for _, m := range got {
		want := fmt.Sprint(next[m.OrderingKey])
		if string(m.Data) != want {
			t.Fatalf("key %s: got %s, want %s", m.OrderingKey, m.Data, want)
		}
		next[m.OrderingKey]++
	}
}

func TestOrderingNackRedeliversKey(t *testing.T) {
	_, c := newEmulatorClient(t)
	mustTopic(t, c, "ord")
	sub := subName("ord-sub")
	mustSub(t, c, &pubsubpb.Subscription{Name: sub, Topic: topicName("ord"), EnableMessageOrdering: true})
	ctx := ctxT(t, 10*time.Second)
	if _, err := c.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topicName("ord"), Messages: []*pubsubpb.PubsubMessage{
		{Data: []byte("1"), OrderingKey: "k"}, {Data: []byte("2"), OrderingKey: "k"}, {Data: []byte("3"), OrderingKey: "k"}, {Data: []byte("x"), OrderingKey: "other"},
	}}); err != nil {
		t.Fatal(err)
	}
	got := pullWait(t, c, sub, 10, 5*time.Second)
	if len(got) != 4 {
		t.Fatalf("pulled %d", len(got))
	}
	var k []*pubsubpb.ReceivedMessage
	for _, m := range got {
		if m.Message.OrderingKey == "k" {
			k = append(k, m)
		} else {
			ack(t, c, sub, m)
		}
	}
	// Ack 1, nack 2: 2 and 3 are redelivered in order.
	ack(t, c, sub, k[0])
	modack(t, c, sub, 0, k[1])
	again := pullWait(t, c, sub, 10, 5*time.Second)
	if len(again) != 2 || string(again[0].Message.Data) != "2" || string(again[1].Message.Data) != "3" {
		t.Fatalf("redelivered %v", again)
	}
	// The key stays blocked while messages are outstanding.
	if _, err := c.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topicName("ord"), Messages: []*pubsubpb.PubsubMessage{{Data: []byte("4"), OrderingKey: "k"}}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if blocked := pull(t, c, sub, 10); len(blocked) != 0 {
		t.Fatalf("key not blocked: %v", blocked)
	}
	ack(t, c, sub, again...)
	if last := pullWait(t, c, sub, 10, 5*time.Second); len(last) != 1 || string(last[0].Message.Data) != "4" {
		t.Fatalf("after ack: %v", last)
	}
}

func TestDeadLetter(t *testing.T) {
	_, c := newEmulatorClient(t)
	mustTopic(t, c, "src")
	mustTopic(t, c, "dlq")
	mustSub(t, c, &pubsubpb.Subscription{Name: subName("dlq-sub"), Topic: topicName("dlq")})
	sub := subName("src-sub")
	mustSub(t, c, &pubsubpb.Subscription{Name: sub, Topic: topicName("src"), DeadLetterPolicy: &pubsubpb.DeadLetterPolicy{
		DeadLetterTopic: topicName("dlq"), MaxDeliveryAttempts: 5,
	}})
	publish(t, c, topicName("src"), &pubsub.Message{Data: []byte("poison"), Attributes: map[string]string{"a": "1"}})
	for attempt := int32(1); attempt <= 5; attempt++ {
		got := pullWait(t, c, sub, 1, 5*time.Second)
		if len(got) != 1 {
			t.Fatalf("attempt %d: pulled %d", attempt, len(got))
		}
		if got[0].DeliveryAttempt != attempt {
			t.Errorf("deliveryAttempt = %d, want %d", got[0].DeliveryAttempt, attempt)
		}
		modack(t, c, sub, 0, got...)
	}
	dead := pullWait(t, c, subName("dlq-sub"), 1, 5*time.Second)
	if len(dead) != 1 {
		t.Fatal("message not dead-lettered")
	}
	m := dead[0].Message
	if string(m.Data) != "poison" || m.Attributes["a"] != "1" || m.Attributes["CloudPubSubDeadLetterSourceDeliveryCount"] != "5" ||
		m.Attributes["CloudPubSubDeadLetterSourceSubscription"] != "src-sub" {
		t.Errorf("dead letter = %v", m)
	}
	if left := pull(t, c, sub, 1); len(left) != 0 {
		t.Errorf("source still has the message")
	}
}

func TestFilteredSubscription(t *testing.T) {
	_, c := newEmulatorClient(t)
	mustTopic(t, c, "topic-t")
	sub := subName("orders-only")
	mustSub(t, c, &pubsubpb.Subscription{Name: sub, Topic: topicName("topic-t"),
		Filter: `attributes.type = "order" AND NOT hasPrefix(attributes.region, "eu")`})
	publish(t, c, topicName("topic-t"),
		&pubsub.Message{Data: []byte("1"), Attributes: map[string]string{"type": "order", "region": "us"}},
		&pubsub.Message{Data: []byte("2"), Attributes: map[string]string{"type": "refund"}},
		&pubsub.Message{Data: []byte("3"), Attributes: map[string]string{"type": "order", "region": "eu-west"}},
		&pubsub.Message{Data: []byte("4"), Attributes: map[string]string{"type": "order"}},
	)
	got := receiveN(t, c, sub, 2, nil)
	var data []string
	for _, m := range got {
		data = append(data, string(m.Data))
	}
	sort.Strings(data)
	if fmt.Sprint(data) != "[1 4]" {
		t.Errorf("filtered = %v", data)
	}
}

func TestStreamingPullFlowControl(t *testing.T) {
	_, c := newEmulatorClient(t)
	mustTopic(t, c, "topic-t")
	sub := subName("sub-s")
	mustSub(t, c, &pubsubpb.Subscription{Name: sub, Topic: topicName("topic-t")})
	var msgs []*pubsub.Message
	for i := range 5 {
		msgs = append(msgs, &pubsub.Message{Data: fmt.Appendf(nil, "%d", i)})
	}
	publish(t, c, topicName("topic-t"), msgs...)
	ctx := ctxT(t, 10*time.Second)
	stream, err := c.SubscriptionAdminClient.StreamingPull(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&pubsubpb.StreamingPullRequest{Subscription: sub, StreamAckDeadlineSeconds: 10, MaxOutstandingMessages: 2}); err != nil {
		t.Fatal(err)
	}
	resp, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if n := len(resp.ReceivedMessages); n != 2 {
		t.Fatalf("first response has %d messages, want 2", n)
	}
	// Ack on the stream frees flow-control credit.
	if err := stream.Send(&pubsubpb.StreamingPullRequest{AckIds: []string{resp.ReceivedMessages[0].AckId}}); err != nil {
		t.Fatal(err)
	}
	resp, err = stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if n := len(resp.ReceivedMessages); n != 1 {
		t.Fatalf("second response has %d messages, want 1", n)
	}
	// Extend and nack on the stream.
	if err := stream.Send(&pubsubpb.StreamingPullRequest{ModifyDeadlineAckIds: []string{resp.ReceivedMessages[0].AckId}, ModifyDeadlineSeconds: []int32{0}}); err != nil {
		t.Fatal(err)
	}
	resp, err = stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ReceivedMessages) == 0 {
		t.Fatal("no redelivery after stream nack")
	}
	_ = stream.CloseSend()
}
