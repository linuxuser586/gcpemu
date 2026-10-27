//go:build conformance

package conformance

import (
	"context"
	"errors"
	"testing"

	pubsub "cloud.google.com/go/pubsub/v2/apiv1"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func TestPubSubTopics(t *testing.T)        { pubsubTopics.test(t) }
func TestPubSubSubscriptions(t *testing.T) { pubsubSubscriptions.test(t) }
func TestPubSubPublishPull(t *testing.T)   { pubsubPublishPull.test(t) }

// topicAdmin is a Pub/Sub topic client for c, closed when the test ends.
func topicAdmin(t testing.TB, c *conn) *pubsub.TopicAdminClient {
	t.Helper()
	cl, err := pubsub.NewTopicAdminClient(c.ctx, c.GRPC()...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	return cl
}

// subscriptionAdmin is a Pub/Sub subscription client for c, closed when
// the test ends.
func subscriptionAdmin(t testing.TB, c *conn) *pubsub.SubscriptionAdminClient {
	t.Helper()
	cl, err := pubsub.NewSubscriptionAdminClient(c.ctx, c.GRPC()...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	return cl
}

// createTopic creates the topic base, deleted when the test ends.
func createTopic(t testing.TB, c *conn, topics *pubsub.TopicAdminClient, base string) *pubsubpb.Topic {
	t.Helper()
	name := "projects/" + c.Project() + "/topics/" + c.Name(base)
	topic, err := topics.CreateTopic(c.ctx, &pubsubpb.Topic{Name: name, Labels: c.Labels()})
	if err != nil {
		t.Fatalf("create topic: %v", err)
	}
	c.Cleanup(func(ctx context.Context) { _ = topics.DeleteTopic(ctx, &pubsubpb.DeleteTopicRequest{Topic: name}) })
	return topic
}

var pubsubTopics = scenario{
	product: "pubsub",
	opts:    []Option{RunOnly("topics")},
	body: func(t testing.TB, c *conn) {
		topics := topicAdmin(t, c)
		topic := createTopic(t, c, topics, "topic")
		if _, err := topics.GetTopic(c.ctx, &pubsubpb.GetTopicRequest{Topic: topic.Name}); err != nil {
			t.Fatalf("get: %v", err)
		}
		c.Retry(func() error {
			it := topics.ListTopics(c.ctx, &pubsubpb.ListTopicsRequest{Project: "projects/" + c.Project(), PageSize: 1000})
			return listed(it.Next, "the new topic", func(x *pubsubpb.Topic) bool { return x.Name == topic.Name })
		})
		labels := c.Labels()
		labels["color"] = "blue"
		if _, err := topics.UpdateTopic(c.ctx, &pubsubpb.UpdateTopicRequest{
			Topic:      &pubsubpb.Topic{Name: topic.Name, Labels: labels},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}},
		}); err != nil {
			t.Fatalf("update: %v", err)
		}
		if _, err := topics.GetTopic(c.ctx, &pubsubpb.GetTopicRequest{Topic: topic.Name + "-missing"}); err == nil {
			t.Error("get of a missing topic succeeded")
		}
		if _, err := topics.CreateTopic(c.ctx, &pubsubpb.Topic{Name: topic.Name}); err == nil {
			t.Error("creating a topic whose name is taken succeeded")
		}
		if err := topics.DeleteTopic(c.ctx, &pubsubpb.DeleteTopicRequest{Topic: topic.Name}); err != nil {
			t.Fatalf("delete: %v", err)
		}
	},
}

var pubsubSubscriptions = scenario{
	product: "pubsub",
	opts:    []Option{RunOnly("subscriptions")},
	body: func(t testing.TB, c *conn) {
		topic := createTopic(t, c, topicAdmin(t, c), "topic")
		subs := subscriptionAdmin(t, c)
		name := "projects/" + c.Project() + "/subscriptions/" + c.Name("sub")
		sub, err := subs.CreateSubscription(c.ctx, &pubsubpb.Subscription{Name: name, Topic: topic.Name, AckDeadlineSeconds: 20, Labels: c.Labels()})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		c.Cleanup(func(ctx context.Context) {
			_ = subs.DeleteSubscription(ctx, &pubsubpb.DeleteSubscriptionRequest{Subscription: name})
		})
		if _, err := subs.GetSubscription(c.ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name}); err != nil {
			t.Fatalf("get: %v", err)
		}
		c.Retry(func() error {
			it := subs.ListSubscriptions(c.ctx, &pubsubpb.ListSubscriptionsRequest{Project: "projects/" + c.Project(), PageSize: 1000})
			return listed(it.Next, "the new subscription", func(x *pubsubpb.Subscription) bool { return x.Name == sub.Name })
		})
		if _, err := subs.UpdateSubscription(c.ctx, &pubsubpb.UpdateSubscriptionRequest{
			Subscription: &pubsubpb.Subscription{Name: name, AckDeadlineSeconds: 30},
			UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{"ack_deadline_seconds"}},
		}); err != nil {
			t.Fatalf("update: %v", err)
		}
		if _, err := subs.GetSubscription(c.ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name + "-missing"}); err == nil {
			t.Error("get of a missing subscription succeeded")
		}
		if _, err := subs.CreateSubscription(c.ctx, &pubsubpb.Subscription{Name: name, Topic: topic.Name}); err == nil {
			t.Error("creating a subscription whose name is taken succeeded")
		}
		if err := subs.DeleteSubscription(c.ctx, &pubsubpb.DeleteSubscriptionRequest{Subscription: name}); err != nil {
			t.Fatalf("delete: %v", err)
		}
	},
}

var pubsubPublishPull = scenario{
	product: "pubsub",
	body: func(t testing.TB, c *conn) {
		topics := topicAdmin(t, c)
		topic := createTopic(t, c, topics, "topic")
		subs := subscriptionAdmin(t, c)
		name := "projects/" + c.Project() + "/subscriptions/" + c.Name("sub")
		if _, err := subs.CreateSubscription(c.ctx, &pubsubpb.Subscription{Name: name, Topic: topic.Name, AckDeadlineSeconds: 30, Labels: c.Labels()}); err != nil {
			t.Fatalf("create subscription: %v", err)
		}
		c.Cleanup(func(ctx context.Context) {
			_ = subs.DeleteSubscription(ctx, &pubsubpb.DeleteSubscriptionRequest{Subscription: name})
		})
		if _, err := topics.Publish(c.ctx, &pubsubpb.PublishRequest{Topic: topic.Name, Messages: []*pubsubpb.PubsubMessage{
			{Data: []byte("hello conformance"), Attributes: map[string]string{"kind": "greeting"}},
		}}); err != nil {
			t.Fatalf("publish: %v", err)
		}
		var ackID string
		// GCP may answer a pull before the message is deliverable.
		c.Retry(func() error {
			resp, err := subs.Pull(c.ctx, &pubsubpb.PullRequest{Subscription: name, MaxMessages: 1})
			if err != nil {
				return err
			}
			if len(resp.ReceivedMessages) != 1 {
				return errors.New("pull returned no message")
			}
			ackID = resp.ReceivedMessages[0].AckId
			return nil
		})
		if err := subs.Acknowledge(c.ctx, &pubsubpb.AcknowledgeRequest{Subscription: name, AckIds: []string{ackID}}); err != nil {
			t.Fatalf("acknowledge: %v", err)
		}
	},
}
