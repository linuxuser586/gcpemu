package build

import (
	"context"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/linuxuser586/gcpemu/internal/emu"
)

// Build notifications: every status change of a Build is published to
// its Project's cloud-builds topic, when that topic exists, as GCP does,
// with the Build as JSON data in the shape builds.get returns. Callers
// publish right after each change, under the ordering the change itself
// has, so one Build's messages arrive in status order.

func (s *Service) publisher() emu.Publisher {
	if svc, ok := s.env.Lookup("pubsub"); ok {
		if p, ok := svc.(emu.Publisher); ok {
			return p
		}
	}
	return nil
}

// notify publishes b's status to cloud-builds. Failures are logged, never
// returned: they do not affect the Build.
func (s *Service) notify(b *cloudbuildpb.Build) {
	pub := s.publisher()
	if pub == nil {
		return
	}
	ctx := context.Background()
	topic := "projects/" + b.GetProjectId() + "/topics/cloud-builds"
	if !pub.TopicExists(ctx, topic) {
		return
	}
	data, err := protojson.Marshal(b)
	if err != nil {
		s.env.Log.Warn("build: notification not published", "build", b.GetName(), "err", err)
		return
	}
	attrs := map[string]string{"buildId": b.GetId(), "status": b.GetStatus().String()}
	if _, err := pub.PublishInternal(ctx, topic, data, attrs); err != nil {
		s.env.Log.Warn("build: publish failed", "build", b.GetName(), "status", attrs["status"], "topic", topic, "err", err)
	}
}
