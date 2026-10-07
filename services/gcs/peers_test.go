package gcs_test

import (
	"context"
	"testing"

	"strings"

	"cloud.google.com/go/iam"
	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/storage"

	"github.com/linuxuser586/gcpemu/emutest"
)

// TestRealPeers exercises bucket IAM and notification topic validation
// against the real iam and pubsub services.
func TestRealPeers(t *testing.T) {
	inst := emutest.Start(t, []string{"gcs", "iam", "pubsub"})
	c := emulatorClient(t, inst)
	ctx := context.Background()
	b := c.Bucket("peers")
	if err := b.Create(ctx, testProject, nil); err != nil {
		t.Fatal(err)
	}
	h := b.IAM()
	p, err := h.Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p.Add("user:bob@example.com", iam.RoleName("roles/storage.objectAdmin"))
	if err := h.SetPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	p2, err := h.Policy(ctx)
	if err != nil || !p2.HasRole("user:bob@example.com", "roles/storage.objectAdmin") {
		t.Fatalf("policy = %v %v", p2, err)
	}
	if _, err := h.TestPermissions(ctx, []string{"storage.objects.get"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.AddNotification(ctx, &storage.Notification{TopicProjectID: testProject, TopicID: "nope", PayloadFormat: storage.JSONPayload}); httpCode(err) != 400 {
		t.Errorf("missing topic err = %v", err)
	}

	// End to end: notification → real Pub/Sub topic → pull (FR-INT-010).
	t.Setenv("PUBSUB_EMULATOR_HOST", inst.Endpoint("pubsub"))
	pc, err := pubsub.NewClient(ctx, testProject)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	topic, sub := "projects/"+testProject+"/topics/gcs-events", "projects/"+testProject+"/subscriptions/gcs-events"
	if _, err := pc.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatal(err)
	}
	if _, err := pc.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.AddNotification(ctx, &storage.Notification{TopicProjectID: testProject, TopicID: "gcs-events", PayloadFormat: storage.JSONPayload, EventTypes: []string{storage.ObjectFinalizeEvent}}); err != nil {
		t.Fatal(err)
	}
	write(t, ctx, b.Object("hello.txt"), []byte("hi"), 0)
	resp, err := pc.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{Subscription: sub, MaxMessages: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ReceivedMessages) != 1 {
		t.Fatalf("pulled %d messages", len(resp.ReceivedMessages))
	}
	m := resp.ReceivedMessages[0].Message
	if m.Attributes["eventType"] != "OBJECT_FINALIZE" || m.Attributes["objectId"] != "hello.txt" || !strings.Contains(string(m.Data), `"storage#object"`) {
		t.Errorf("message = %v %s", m.Attributes, m.Data)
	}
}
