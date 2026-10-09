package secrets

import (
	"context"
	"time"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/linuxuser586/gcpemu/internal/emu"
)

// Event notifications: every change to a secret with topics is published
// to each topic, as GCP does, with the secret (or the version, for
// version events) as JSON_API_V1 data.

func (s *Service) publisher() emu.Publisher {
	if svc, ok := s.env.Lookup("pubsub"); ok {
		if p, ok := svc.(emu.Publisher); ok {
			return p
		}
	}
	return nil
}

// notify publishes event to topics. Failures are logged, never returned.
func (s *Service) notify(ctx context.Context, topics []*secretmanagerpb.Topic, event string, sec *secretmanagerpb.Secret, v *secretmanagerpb.SecretVersion) {
	if len(topics) == 0 {
		return
	}
	pub := s.publisher()
	if pub == nil {
		s.env.Log.Warn("secrets: Pub/Sub is not running; event not published", "event", event, "secret", sec.GetName())
		return
	}
	var data proto.Message = sec
	attrs := map[string]string{
		"eventType":  event,
		"dataFormat": "JSON_API_V1",
		"secretId":   sec.GetName(),
		"timestamp":  s.env.Clock.Now().UTC().Format(time.RFC3339Nano),
	}
	if v != nil {
		data = v
		attrs["versionId"] = v.GetName()
	}
	b, err := protojson.Marshal(data)
	if err != nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	for _, t := range topics {
		if _, err := pub.PublishInternal(ctx, t.GetName(), b, attrs); err != nil {
			s.env.Log.Warn("secrets: publish failed", "event", event, "topic", t.GetName(), "err", err)
		}
	}
}
