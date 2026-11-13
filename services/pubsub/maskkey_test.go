package pubsub_test

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/linuxuser586/gcpemu/emutest"
)

// #1089: an update_mask path naming one map key (AIP-161, ADR 0075), such
// as labels.env, changes that key only.

// keyTopic creates topic "keys" with labels env=dev and team=core.
func keyTopic(t *testing.T) *pubsub.Client {
	t.Helper()
	_, c := newEmulatorClient(t)
	if _, err := c.TopicAdminClient.CreateTopic(context.Background(), &pubsubpb.Topic{
		Name: topicName("keys"), Labels: map[string]string{"env": "dev", "team": "core"},
	}); err != nil {
		t.Fatal(err)
	}
	return c
}

// updateTopic sends UpdateTopic for "keys" with the given labels and mask.
func updateTopic(c *pubsub.Client, labels map[string]string, paths ...string) (*pubsubpb.Topic, error) {
	return c.TopicAdminClient.UpdateTopic(context.Background(), &pubsubpb.UpdateTopicRequest{
		Topic:      &pubsubpb.Topic{Name: topicName("keys"), Labels: labels},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: paths},
	})
}

// topicLabels returns topic "keys"'s labels as GetTopic sees them.
func topicLabels(t *testing.T, c *pubsub.Client) map[string]string {
	t.Helper()
	tp, err := c.TopicAdminClient.GetTopic(context.Background(), &pubsubpb.GetTopicRequest{Topic: topicName("keys")})
	if err != nil {
		t.Fatal(err)
	}
	return tp.GetLabels()
}

// A path naming one label sets it and keeps the others.
func TestMaskKeyTopicLabel(t *testing.T) {
	c := keyTopic(t)
	if _, err := updateTopic(c, map[string]string{"env": "prod", "other": "x"}, "labels.env"); err != nil {
		t.Fatal(err)
	}
	if l := topicLabels(t, c); len(l) != 2 || l["env"] != "prod" || l["team"] != "core" {
		t.Fatalf("labels = %v, want env=prod team=core", l)
	}
}

// A path naming one subscription label sets it and keeps the others.
func TestMaskKeySubscriptionLabel(t *testing.T) {
	c := keyTopic(t)
	ctx := context.Background()
	mustSub(t, c, &pubsubpb.Subscription{Name: subName("keys"), Topic: topicName("keys"), Labels: map[string]string{"env": "dev", "team": "core"}})
	if _, err := c.SubscriptionAdminClient.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
		Subscription: &pubsubpb.Subscription{Name: subName("keys"), Labels: map[string]string{"env": "prod"}},
		UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{"labels.env"}},
	}); err != nil {
		t.Fatal(err)
	}
	s, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: subName("keys")})
	if err != nil {
		t.Fatal(err)
	}
	if l := s.GetLabels(); len(l) != 2 || l["env"] != "prod" || l["team"] != "core" {
		t.Fatalf("labels = %v, want env=prod team=core", l)
	}
}

// A backtick-quoted key is used as written, and a key the request lacks
// is removed.
func TestMaskKeyQuotedRemoves(t *testing.T) {
	c := keyTopic(t)
	if _, err := updateTopic(c, map[string]string{"my-key": "v"}, "labels.`my-key`"); err != nil {
		t.Fatal(err)
	}
	if l := topicLabels(t, c); len(l) != 3 || l["my-key"] != "v" {
		t.Fatalf("labels = %v, want my-key=v added", l)
	}
	if _, err := updateTopic(c, nil, "labels.`my-key`", "labels.team"); err != nil {
		t.Fatal(err)
	}
	if l := topicLabels(t, c); len(l) != 1 || l["env"] != "dev" {
		t.Fatalf("labels = %v, want env=dev only", l)
	}
}

// A whole-field path wins over key paths of the same field, in either
// order.
func TestMaskKeyWholeFieldWins(t *testing.T) {
	c := keyTopic(t)
	for _, mask := range [][]string{{"labels", "labels.env"}, {"labels.env", "labels"}} {
		if _, err := updateTopic(c, map[string]string{"x": "1"}, mask...); err != nil {
			t.Fatal(err)
		}
		if l := topicLabels(t, c); len(l) != 1 || l["x"] != "1" {
			t.Fatalf("mask %v: labels = %v, want x=1 only", mask, l)
		}
	}
}

// A key path with more segments after the key, or on a non-map field, is
// INVALID_ARGUMENT and changes nothing.
func TestMaskKeyRejected(t *testing.T) {
	c := keyTopic(t)
	for _, p := range []string{"labels.env.x", "labels.`a", "labels.", "kms_key_name.x"} {
		if _, err := updateTopic(c, map[string]string{"env": "prod"}, "labels.team", p); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%q: err = %v, want InvalidArgument", p, err)
		}
	}
	if l := topicLabels(t, c); len(l) != 2 || l["env"] != "dev" || l["team"] != "core" {
		t.Fatalf("labels = %v, want unchanged", l)
	}
}

// Over REST, a key keeps its case and characters whether updateMask is in
// the body or the query; only field segments are lowerCamel.
func TestMaskKeyREST(t *testing.T) {
	inst := emutest.Start(t, []string{"pubsub"})
	v1 := inst.EnvVars()["CLOUDSDK_API_ENDPOINT_OVERRIDES_PUBSUB"] + "v1/projects/" + project + "/topics/keys"
	restCall(t, http.MethodPut, v1, map[string]any{"labels": map[string]string{"env": "dev"}}, 200)
	restCall(t, http.MethodPatch, v1, map[string]any{
		"topic":      map[string]any{"labels": map[string]string{"camel_key": "a", "my-key": "b"}},
		"updateMask": "labels.camel_key,labels.`my-key`",
	}, 200)
	up := restCall(t, http.MethodPatch, v1+"?updateMask=labels.env,messageRetentionDuration", map[string]any{
		"topic": map[string]any{"labels": map[string]string{"env": "prod"}, "messageRetentionDuration": "3600s"},
	}, 200)
	want := map[string]any{"env": "prod", "camel_key": "a", "my-key": "b"}
	if l := up["labels"].(map[string]any); len(l) != len(want) || l["env"] != want["env"] || l["camel_key"] != "a" || l["my-key"] != "b" || up["messageRetentionDuration"] != "3600s" {
		t.Fatalf("patched = %v, want labels %v", up, want)
	}
}

// Concurrent updates naming different keys all land: each merges into the
// stored map it writes, not into a stale copy.
func TestMaskKeyConcurrent(t *testing.T) {
	c := keyTopic(t)
	mustSub(t, c, &pubsubpb.Subscription{Name: subName("keys"), Topic: topicName("keys")})
	const n = 20
	var wg sync.WaitGroup
	for i := range n {
		k := fmt.Sprintf("k%d", i)
		wg.Go(func() {
			if _, err := updateTopic(c, map[string]string{k: "v"}, "labels."+k); err != nil {
				t.Error(err)
			}
		})
		wg.Go(func() {
			if _, err := c.SubscriptionAdminClient.UpdateSubscription(context.Background(), &pubsubpb.UpdateSubscriptionRequest{
				Subscription: &pubsubpb.Subscription{Name: subName("keys"), Labels: map[string]string{k: "v"}},
				UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{"labels." + k}},
			}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if l := topicLabels(t, c); len(l) != n+2 {
		t.Errorf("topic has %d labels, want %d: %v", len(l), n+2, l)
	}
	s, err := c.SubscriptionAdminClient.GetSubscription(context.Background(), &pubsubpb.GetSubscriptionRequest{Subscription: subName("keys")})
	if err != nil {
		t.Fatal(err)
	}
	if l := s.GetLabels(); len(l) != n {
		t.Errorf("subscription has %d labels, want %d: %v", len(l), n, l)
	}
}
