package pubsub_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/pubsub/v2"
	vkit "cloud.google.com/go/pubsub/v2/apiv1"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"github.com/googleapis/gax-go/v2/apierror"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestExactlyOnceDelivery(t *testing.T) {
	_, c := newEmulatorClient(t)
	mustTopic(t, c, "eod-topic")
	sub := subName("eod-sub")
	mustSub(t, c, &pubsubpb.Subscription{Name: sub, Topic: topicName("eod-topic"), EnableExactlyOnceDelivery: true})
	publish(t, c, topicName("eod-topic"), &pubsub.Message{Data: []byte("once")})

	m := pullWait(t, c, sub, 1, 5*time.Second)
	if len(m) != 1 {
		t.Fatal("no message")
	}
	ack(t, c, sub, m...)
	// A second ack of the same (now invalid) ack ID fails with per-ID metadata.
	err := c.SubscriptionAdminClient.Acknowledge(context.Background(), &pubsubpb.AcknowledgeRequest{Subscription: sub, AckIds: []string{m[0].AckId}})
	var ae *apierror.APIError
	if !errors.As(err, &ae) || ae.GRPCStatus().Code() != codes.InvalidArgument || ae.Metadata()[m[0].AckId] != "PERMANENT_FAILURE_INVALID_ACK_ID" {
		t.Fatalf("re-ack error = %v", err)
	}

	// The client library reports ack results.
	publish(t, c, topicName("eod-topic"), &pubsub.Message{Data: []byte("twice")})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var result pubsub.AcknowledgeStatus = -1
	err = c.Subscriber(sub).Receive(ctx, func(ctx context.Context, m *pubsub.Message) {
		r := m.AckWithResult()
		st, err := r.Get(ctx)
		if err != nil {
			t.Errorf("ack result: %v", err)
		}
		result = st
		cancel()
	})
	if err != nil {
		t.Fatal(err)
	}
	if result != pubsub.AcknowledgeStatusSuccess {
		t.Errorf("ack status = %v", result)
	}
}

func TestSeekAndSnapshots(t *testing.T) {
	_, c := newEmulatorClient(t)
	ctx := ctxT(t, 20*time.Second)
	mustTopic(t, c, "seek-topic")
	sub := subName("seek-sub")
	mustSub(t, c, &pubsubpb.Subscription{Name: sub, Topic: topicName("seek-topic"), RetainAckedMessages: true})
	before := time.Now().Add(-time.Second)
	publish(t, c, topicName("seek-topic"), &pubsub.Message{Data: []byte("1")}, &pubsub.Message{Data: []byte("2")})
	got := pullWait(t, c, sub, 10, 5*time.Second)
	if len(got) != 2 {
		t.Fatalf("pulled %d", len(got))
	}
	ack(t, c, sub, got...)

	// Seek to time replays retained acked messages.
	if _, err := c.SubscriptionAdminClient.Seek(ctx, &pubsubpb.SeekRequest{Subscription: sub, Target: &pubsubpb.SeekRequest_Time{Time: timestamppb.New(before)}}); err != nil {
		t.Fatal(err)
	}
	replay := pullWait(t, c, sub, 10, 5*time.Second)
	if len(replay) != 2 {
		t.Fatalf("replayed %d", len(replay))
	}

	// Snapshot captures unacked messages and those published afterwards.
	snap := "projects/" + project + "/snapshots/seek-snap"
	sn, err := c.SubscriptionAdminClient.CreateSnapshot(ctx, &pubsubpb.CreateSnapshotRequest{Name: snap, Subscription: sub})
	if err != nil {
		t.Fatal(err)
	}
	if sn.Topic != topicName("seek-topic") || sn.ExpireTime == nil {
		t.Errorf("snapshot = %v", sn)
	}
	ack(t, c, sub, replay...)
	publish(t, c, topicName("seek-topic"), &pubsub.Message{Data: []byte("3")})
	ack(t, c, sub, pullWait(t, c, sub, 10, 5*time.Second)...)
	if left := pull(t, c, sub, 10); len(left) != 0 {
		t.Fatalf("left = %d", len(left))
	}
	if _, err := c.SubscriptionAdminClient.Seek(ctx, &pubsubpb.SeekRequest{Subscription: sub, Target: &pubsubpb.SeekRequest_Snapshot{Snapshot: snap}}); err != nil {
		t.Fatal(err)
	}
	all := pullWait(t, c, sub, 10, 5*time.Second)
	if len(all) != 3 {
		t.Fatalf("after snapshot seek: %d messages", len(all))
	}
	var names []string
	it := c.TopicAdminClient.ListTopicSnapshots(ctx, &pubsubpb.ListTopicSnapshotsRequest{Topic: topicName("seek-topic")})
	for n, err := it.Next(); err == nil; n, err = it.Next() {
		names = append(names, n)
	}
	if len(names) != 1 || names[0] != snap {
		t.Errorf("topic snapshots = %v", names)
	}
	if err := c.SubscriptionAdminClient.DeleteSnapshot(ctx, &pubsubpb.DeleteSnapshotRequest{Snapshot: snap}); err != nil {
		t.Fatal(err)
	}
}

func TestSeekWithTopicRetention(t *testing.T) {
	_, c := newEmulatorClient(t)
	ctx := ctxT(t, 20*time.Second)
	if _, err := c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topicName("ret-topic"), MessageRetentionDuration: durationpb.New(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	sub := subName("ret-sub")
	mustSub(t, c, &pubsubpb.Subscription{Name: sub, Topic: topicName("ret-topic")})
	before := time.Now().Add(-time.Second)
	publish(t, c, topicName("ret-topic"), &pubsub.Message{Data: []byte("kept")})
	ack(t, c, sub, pullWait(t, c, sub, 10, 5*time.Second)...)
	if _, err := c.SubscriptionAdminClient.Seek(ctx, &pubsubpb.SeekRequest{Subscription: sub, Target: &pubsubpb.SeekRequest_Time{Time: timestamppb.New(before)}}); err != nil {
		t.Fatal(err)
	}
	if got := pullWait(t, c, sub, 10, 5*time.Second); len(got) != 1 || string(got[0].Message.Data) != "kept" {
		t.Fatalf("replay from topic retention: %v", got)
	}
}

const avroSchema = `{"type":"record","name":"Order","fields":[
  {"name":"id","type":"string"},
  {"name":"qty","type":"int"},
  {"name":"note","type":["null","string"],"default":null}]}`

const protoSchema = `syntax = "proto3";
message Order {
  string id = 1;
  int32 qty = 2;
}`

func TestSchemas(t *testing.T) {
	inst, c := newEmulatorClient(t)
	ctx := ctxT(t, 20*time.Second)
	sc, err := vkit.NewSchemaClient(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	parent := "projects/" + project
	if _, err := sc.CreateSchema(ctx, &pubsubpb.CreateSchemaRequest{Parent: parent, SchemaId: "bad-schema",
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_AVRO, Definition: `{"type":"nope"}`}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("invalid avro: %v", err)
	}
	av, err := sc.CreateSchema(ctx, &pubsubpb.CreateSchemaRequest{Parent: parent, SchemaId: "order-avro",
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_AVRO, Definition: avroSchema}})
	if err != nil {
		t.Fatal(err)
	}
	if av.RevisionId == "" || av.RevisionCreateTime == nil {
		t.Errorf("schema = %v", av)
	}
	pb, err := sc.CreateSchema(ctx, &pubsubpb.CreateSchemaRequest{Parent: parent, SchemaId: "order-proto",
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_PROTOCOL_BUFFER, Definition: protoSchema}})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		topic, schema string
		good, bad     string
	}{
		{"avro-topic", av.Name, `{"id":"a","qty":2,"note":{"string":"x"}}`, `{"id":"a","qty":"two"}`},
		{"proto-topic", pb.Name, `{"id":"a","qty":2}`, `{"id":"a","qty":"x"}`},
	} {
		if _, err := c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topicName(tc.topic),
			SchemaSettings: &pubsubpb.SchemaSettings{Schema: tc.schema, Encoding: pubsubpb.Encoding_JSON}}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topicName(tc.topic), Messages: []*pubsubpb.PubsubMessage{{Data: []byte(tc.good)}}}); err != nil {
			t.Errorf("%s valid publish: %v", tc.topic, err)
		}
		if _, err := c.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topicName(tc.topic), Messages: []*pubsubpb.PubsubMessage{{Data: []byte(tc.bad)}}}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s invalid publish: %v", tc.topic, err)
		}
	}

	// Revisions.
	rev2, err := sc.CommitSchema(ctx, &pubsubpb.CommitSchemaRequest{Name: av.Name, Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_AVRO,
		Definition: `{"type":"record","name":"Order","fields":[{"name":"id","type":"string"}]}`}})
	if err != nil {
		t.Fatal(err)
	}
	var revs []*pubsubpb.Schema
	it := sc.ListSchemaRevisions(ctx, &pubsubpb.ListSchemaRevisionsRequest{Name: av.Name})
	for s, err := it.Next(); err == nil; s, err = it.Next() {
		revs = append(revs, s)
	}
	if len(revs) != 2 || revs[0].RevisionId != rev2.RevisionId {
		t.Errorf("revisions = %v", revs)
	}
	got, err := sc.GetSchema(ctx, &pubsubpb.GetSchemaRequest{Name: av.Name + "@" + av.RevisionId})
	if err != nil || got.Definition != avroSchema {
		t.Errorf("get revision: %v %v", got, err)
	}
	if _, err := sc.ValidateMessage(ctx, &pubsubpb.ValidateMessageRequest{Parent: parent, SchemaSpec: &pubsubpb.ValidateMessageRequest_Name{Name: pb.Name},
		Message: []byte(`{"id":"z"}`), Encoding: pubsubpb.Encoding_JSON}); err != nil {
		t.Errorf("validate message: %v", err)
	}
	if _, err := sc.ValidateSchema(ctx, &pubsubpb.ValidateSchemaRequest{Parent: parent, Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_PROTOCOL_BUFFER, Definition: "message {"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("validate bad schema: %v", err)
	}
	if err := sc.DeleteSchema(ctx, &pubsubpb.DeleteSchemaRequest{Name: pb.Name}); err != nil {
		t.Fatal(err)
	}
	tp, _ := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: topicName("proto-topic")})
	if tp.GetSchemaSettings().GetSchema() != "_deleted-schema_" {
		t.Errorf("topic schema after delete = %v", tp.GetSchemaSettings())
	}
	_ = inst
}

func TestSeed(t *testing.T) {
	inst, c := newEmulatorClient(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "order.avsc"), []byte(avroSchema), 0o600); err != nil {
		t.Fatal(err)
	}
	seed := `
pubsub:
  project: ` + project + `
  schemas:
    - name: seeded-schema
      type: AVRO
      definitionFile: order.avsc
  topics:
    - name: seeded-topic
      labels: {env: dev}
      schemaSettings: {schema: seeded-schema, encoding: JSON}
    - name: seeded-dlq
  subscriptions:
    - name: seeded-sub
      topic: seeded-topic
      ackDeadlineSeconds: 30
      filter: attributes.kind = "a"
      deadLetterPolicy: {deadLetterTopic: seeded-dlq, maxDeliveryAttempts: 7}
    - name: seeded-push
      topic: seeded-topic
      pushConfig: {pushEndpoint: "http://127.0.0.1:1/push", oidcToken: {serviceAccountEmail: "sa@test-proj.iam.gserviceaccount.com"}}
`
	path := filepath.Join(dir, "seed.yaml")
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := ctxT(t, 10*time.Second)
	for range 2 { // idempotent
		if err := inst.ApplySeed(ctx, path); err != nil {
			t.Fatal(err)
		}
	}
	s, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: subName("seeded-sub")})
	if err != nil {
		t.Fatal(err)
	}
	if s.AckDeadlineSeconds != 30 || s.Filter != `attributes.kind = "a"` || s.DeadLetterPolicy.GetDeadLetterTopic() != topicName("seeded-dlq") || s.DeadLetterPolicy.MaxDeliveryAttempts != 7 {
		t.Errorf("seeded sub = %v", s)
	}
	p, _ := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: subName("seeded-push")})
	if p.GetPushConfig().GetOidcToken().GetServiceAccountEmail() == "" {
		t.Errorf("push sub = %v", p)
	}
	tp, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: topicName("seeded-topic")})
	if err != nil || tp.Labels["env"] != "dev" || tp.SchemaSettings.GetSchema() != "projects/"+project+"/schemas/seeded-schema" {
		t.Errorf("seeded topic = %v %v", tp, err)
	}
}

func TestIAMPolicyGRPC(t *testing.T) {
	inst, c := newEmulatorClient(t)
	gw := newGatewayClient(t, inst)
	ctx := ctxT(t, 10*time.Second)
	mustTopic(t, c, "iam-topic")
	for name, cl := range map[string]*pubsub.Client{"emulator": c, "gateway": gw} {
		res := topicName("iam-topic")
		cur, err := cl.TopicAdminClient.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: res})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		cur.Bindings = append(cur.Bindings, &iampb.Binding{Role: "roles/pubsub.publisher", Members: []string{"user:" + name + "@example.com"}})
		if _, err := cl.TopicAdminClient.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: res, Policy: cur}); err != nil {
			t.Fatalf("%s: set: %v", name, err)
		}
		got, err := cl.TopicAdminClient.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: res})
		if err != nil || len(got.Bindings) == 0 {
			t.Fatalf("%s: get after set: %v %v", name, got, err)
		}
		tp, err := cl.TopicAdminClient.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: res, Permissions: []string{"pubsub.topics.publish"}})
		if err != nil || len(tp.Permissions) != 1 {
			t.Fatalf("%s: test permissions: %v %v", name, tp, err)
		}
		if _, err := cl.TopicAdminClient.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: topicName("no-such-topic")}); status.Code(err) != codes.NotFound {
			t.Errorf("%s: missing resource: %v", name, err)
		}
	}
}
