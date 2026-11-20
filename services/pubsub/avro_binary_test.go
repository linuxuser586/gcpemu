package pubsub_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
	"time"

	vkit "cloud.google.com/go/pubsub/v2/apiv1"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// avroLong is Avro's binary encoding of a long: a zig-zag varint, which is
// what encoding/binary's signed varint is.
func avroLong(n int64) []byte { return binary.AppendVarint(nil, n) }

// TestAvroBinaryLimits checks Avro binary messages against the decode limits
// (#1199): block counts claiming huge maps or arrays are rejected promptly
// with InvalidArgument, and a bytes value over the library's 1 MiB default
// (the limit is now the publish limit) passes, on both publish and
// ValidateMessage.
func TestAvroBinaryLimits(t *testing.T) {
	_, c := newEmulatorClient(t)
	ctx := ctxT(t, 60*time.Second)
	sc, err := vkit.NewSchemaClient(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	parent := "projects/" + project

	// Each item of a null map or array encodes in 0 bytes, so the message
	// stays tiny while its block count claims 2^40 entries.
	const huge = int64(1) << 40
	bigBytes := bytes.Repeat([]byte{'x'}, 2<<20)
	for _, tc := range []struct {
		name, def string
		msg       []byte
		valid     bool
	}{
		{"huge-map", `{"type":"map","values":"null"}`, avroLong(huge), false},
		{"huge-array", `{"type":"array","items":"null"}`, append(avroLong(huge), 0), false},
		{"big-bytes", `"bytes"`, append(avroLong(int64(len(bigBytes))), bigBytes...), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := sc.CreateSchema(ctx, &pubsubpb.CreateSchemaRequest{Parent: parent, SchemaId: "lim-" + tc.name,
				Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_AVRO, Definition: tc.def}})
			if err != nil {
				t.Fatal(err)
			}
			topic := topicName("lim-" + tc.name)
			if _, err := c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic,
				SchemaSettings: &pubsubpb.SchemaSettings{Schema: s.Name, Encoding: pubsubpb.Encoding_BINARY}}); err != nil {
				t.Fatal(err)
			}
			want := codes.OK
			if !tc.valid {
				want = codes.InvalidArgument
			}

			pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			_, err = c.TopicAdminClient.Publish(pctx, &pubsubpb.PublishRequest{Topic: topic,
				Messages: []*pubsubpb.PubsubMessage{{Data: tc.msg}}})
			if got := status.Code(err); got != want {
				t.Errorf("publish: code %v, want %v (%v)", got, want, err)
			}

			vctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			_, err = sc.ValidateMessage(vctx, &pubsubpb.ValidateMessageRequest{Parent: parent,
				SchemaSpec: &pubsubpb.ValidateMessageRequest_Name{Name: s.Name},
				Message:    tc.msg, Encoding: pubsubpb.Encoding_BINARY})
			if got := status.Code(err); got != want {
				t.Errorf("validate message: code %v, want %v (%v)", got, want, err)
			}
		})
	}
}
