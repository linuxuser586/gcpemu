package pubsub

import (
	"context"
	"strconv"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// publisherServer implements google.pubsub.v1.Publisher (FR-PS-001, FR-PS-002).
type publisherServer struct {
	pubsubpb.UnimplementedPublisherServer
	s *Service
}

// topicUpdatable lists the Topic fields UpdateTopic may change.
var topicUpdatable = map[string]bool{
	"labels": true, "message_storage_policy": true, "kms_key_name": true,
	"schema_settings": true, "message_retention_duration": true,
	"ingestion_data_source_settings": true, "message_transforms": true,
}

// validateTopic checks a topic config and resolves its schema.
func (s *Service) validateTopic(cfg *pubsubpb.Topic) error {
	if err := validateLabels(cfg.GetLabels()); err != nil {
		return err
	}
	if err := validateRetention("message_retention_duration", cfg.GetMessageRetentionDuration()); err != nil {
		return err
	}
	if ss := cfg.GetSchemaSettings(); ss != nil && ss.GetSchema() != deletedSchema {
		if ss.GetSchema() == "" {
			return apierr.InvalidArgument("schema_settings.schema must be set.")
		}
		if _, _, err := parseName(ss.GetSchema(), kindSchemas); err != nil {
			return err
		}
		if ss.GetEncoding() == pubsubpb.Encoding_ENCODING_UNSPECIFIED {
			ss.Encoding = pubsubpb.Encoding_JSON
		}
		s.mu.Lock()
		_, ok := s.schemas[ss.GetSchema()]
		s.mu.Unlock()
		if !ok {
			return apierr.NotFound("Schema %s not found.", ss.GetSchema()).WithReason("pubsub.googleapis.com", "RESOURCE_NOT_FOUND")
		}
	}
	return nil
}

func (p *publisherServer) CreateTopic(ctx context.Context, req *pubsubpb.Topic) (*pubsubpb.Topic, error) {
	return p.s.createTopic(ctx, req, true)
}

// createTopic creates a topic; check=false skips IAM (seeding).
func (s *Service) createTopic(ctx context.Context, req *pubsubpb.Topic, check bool) (*pubsubpb.Topic, error) {
	project, _, err := parseName(req.GetName(), kindTopics)
	if err != nil {
		return nil, err
	}
	if err := s.env.EnsureProject(project); err != nil {
		return nil, err
	}
	if check {
		if err := s.env.Auth.Check(ctx, "pubsub.topics.create", projectResource(project)); err != nil {
			return nil, err
		}
		if ss := req.GetSchemaSettings(); ss != nil && ss.GetSchema() != "" {
			if err := s.env.Auth.Check(ctx, "pubsub.schemas.attach", fullName(ss.GetSchema())); err != nil {
				return nil, err
			}
		}
	}
	cfg := proto.Clone(req).(*pubsubpb.Topic)
	cfg.State = pubsubpb.Topic_STATE_UNSPECIFIED
	if err := s.validateTopic(cfg); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if _, ok := s.topics[cfg.Name]; ok {
		s.mu.Unlock()
		return nil, alreadyExists(cfg.Name)
	}
	t := &topic{name: cfg.Name, cfg: cfg, subs: map[string]*sub{}, schema: s.topicSchema(cfg)}
	s.topics[cfg.Name] = t
	var w writes
	w.putProto(nsTopics, cfg.Name, cfg)
	out := proto.Clone(cfg).(*pubsubpb.Topic)
	s.mu.Unlock()
	if err := s.commit(&w); err != nil {
		return nil, err
	}
	return out, nil
}

// lookupTopic returns a clone of a topic's config.
func (s *Service) lookupTopic(name string) (*pubsubpb.Topic, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.topics[name]
	if !ok {
		return nil, notFound(name)
	}
	return proto.Clone(t.cfg).(*pubsubpb.Topic), nil
}

func (p *publisherServer) GetTopic(ctx context.Context, req *pubsubpb.GetTopicRequest) (*pubsubpb.Topic, error) {
	if _, _, err := parseName(req.GetTopic(), kindTopics); err != nil {
		return nil, err
	}
	if err := p.s.env.Auth.Check(ctx, "pubsub.topics.get", fullName(req.GetTopic())); err != nil {
		return nil, err
	}
	return p.s.lookupTopic(req.GetTopic())
}

func (p *publisherServer) UpdateTopic(ctx context.Context, req *pubsubpb.UpdateTopicRequest) (*pubsubpb.Topic, error) {
	s := p.s
	if req.GetTopic() == nil {
		return nil, apierr.InvalidArgument("The topic field in the UpdateTopicRequest must be set.")
	}
	name := req.GetTopic().GetName()
	if _, _, err := parseName(name, kindTopics); err != nil {
		return nil, err
	}
	if err := s.env.Auth.Check(ctx, "pubsub.topics.update", fullName(name)); err != nil {
		return nil, err
	}
	cur, err := s.lookupTopic(name)
	if err != nil {
		return nil, err
	}
	if err := applyMask(cur, req.GetTopic(), req.GetUpdateMask().GetPaths(), topicUpdatable, "Topic"); err != nil {
		return nil, err
	}
	if err := s.validateTopic(cur); err != nil {
		return nil, err
	}
	s.mu.Lock()
	t, ok := s.topics[name]
	if !ok {
		s.mu.Unlock()
		return nil, notFound(name)
	}
	t.cfg = cur
	t.schema = s.topicSchema(cur)
	var w writes
	w.putProto(nsTopics, name, cur)
	// Keep subscriptions' output-only topic retention in sync.
	for _, sb := range t.subs {
		sb.cfg.TopicMessageRetentionDuration = cur.GetMessageRetentionDuration()
	}
	out := proto.Clone(cur).(*pubsubpb.Topic)
	s.mu.Unlock()
	if err := s.commit(&w); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *publisherServer) ListTopics(ctx context.Context, req *pubsubpb.ListTopicsRequest) (*pubsubpb.ListTopicsResponse, error) {
	s := p.s
	project, err := parseProject(req.GetProject())
	if err != nil {
		return nil, err
	}
	if err := s.env.EnsureProject(project); err != nil {
		return nil, err
	}
	if err := s.env.Auth.Check(ctx, "pubsub.topics.list", projectResource(project)); err != nil {
		return nil, err
	}
	s.mu.Lock()
	var names []string
	for name := range s.topics {
		if projectOf(name) == project {
			names = append(names, name)
		}
	}
	s.mu.Unlock()
	page, next, err := paginate(names, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	resp := &pubsubpb.ListTopicsResponse{NextPageToken: next}
	s.mu.Lock()
	for _, n := range page {
		if t, ok := s.topics[n]; ok {
			resp.Topics = append(resp.Topics, proto.Clone(t.cfg).(*pubsubpb.Topic))
		}
	}
	s.mu.Unlock()
	return resp, nil
}

func (p *publisherServer) ListTopicSubscriptions(ctx context.Context, req *pubsubpb.ListTopicSubscriptionsRequest) (*pubsubpb.ListTopicSubscriptionsResponse, error) {
	s := p.s
	if _, _, err := parseName(req.GetTopic(), kindTopics); err != nil {
		return nil, err
	}
	if err := s.env.Auth.Check(ctx, "pubsub.topics.get", fullName(req.GetTopic())); err != nil {
		return nil, err
	}
	s.mu.Lock()
	t, ok := s.topics[req.GetTopic()]
	var names []string
	if ok {
		for n := range t.subs {
			names = append(names, n)
		}
	}
	s.mu.Unlock()
	if !ok {
		return nil, notFound(req.GetTopic())
	}
	page, next, err := paginate(names, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &pubsubpb.ListTopicSubscriptionsResponse{Subscriptions: page, NextPageToken: next}, nil
}

func (p *publisherServer) ListTopicSnapshots(ctx context.Context, req *pubsubpb.ListTopicSnapshotsRequest) (*pubsubpb.ListTopicSnapshotsResponse, error) {
	s := p.s
	if _, _, err := parseName(req.GetTopic(), kindTopics); err != nil {
		return nil, err
	}
	if err := s.env.Auth.Check(ctx, "pubsub.topics.get", fullName(req.GetTopic())); err != nil {
		return nil, err
	}
	s.mu.Lock()
	_, ok := s.topics[req.GetTopic()]
	var names []string
	for n, sn := range s.snaps {
		if sn.cfg.GetTopic() == req.GetTopic() {
			names = append(names, n)
		}
	}
	s.mu.Unlock()
	if !ok {
		return nil, notFound(req.GetTopic())
	}
	page, next, err := paginate(names, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &pubsubpb.ListTopicSnapshotsResponse{Snapshots: page, NextPageToken: next}, nil
}

func (p *publisherServer) DeleteTopic(ctx context.Context, req *pubsubpb.DeleteTopicRequest) (*emptypb.Empty, error) {
	s := p.s
	if _, _, err := parseName(req.GetTopic(), kindTopics); err != nil {
		return nil, err
	}
	if err := s.env.Auth.Check(ctx, "pubsub.topics.delete", fullName(req.GetTopic())); err != nil {
		return nil, err
	}
	var w writes
	s.mu.Lock()
	t, ok := s.topics[req.GetTopic()]
	if !ok {
		s.mu.Unlock()
		return nil, notFound(req.GetTopic())
	}
	delete(s.topics, t.name)
	w.del(nsTopics, t.name)
	// Existing subscriptions keep their backlog; their topic becomes
	// _deleted-topic_.
	for _, sb := range t.subs {
		sb.cfg.Topic = deletedTopic
		sb.cfg.TopicMessageRetentionDuration = nil
		w.putProto(nsSubs, sb.name, sb.cfg)
	}
	for _, m := range t.log {
		s.unref(m, &w)
	}
	t.log = nil
	s.mu.Unlock()
	return &emptypb.Empty{}, s.commit(&w)
}

func (p *publisherServer) DetachSubscription(ctx context.Context, req *pubsubpb.DetachSubscriptionRequest) (*pubsubpb.DetachSubscriptionResponse, error) {
	s := p.s
	if _, _, err := parseName(req.GetSubscription(), kindSubscriptions); err != nil {
		return nil, err
	}
	s.mu.Lock()
	sb, ok := s.subs[req.GetSubscription()]
	var topicName string
	if ok {
		topicName = sb.cfg.GetTopic()
	}
	s.mu.Unlock()
	if !ok {
		return nil, notFound(req.GetSubscription())
	}
	res := fullName(req.GetSubscription())
	if topicName != deletedTopic {
		res = fullName(topicName)
	}
	if err := s.env.Auth.Check(ctx, "pubsub.topics.detachSubscription", res); err != nil {
		return nil, err
	}
	var w writes
	s.mu.Lock()
	sb, ok = s.subs[req.GetSubscription()]
	if ok && !sb.detached() {
		sb.cfg.Detached = true
		if t := s.topics[sb.cfg.GetTopic()]; t != nil {
			delete(t.subs, sb.name)
		}
		s.dropBacklog(sb, &w)
		s.stopPush(sb)
		w.putProto(nsSubs, sb.name, sb.cfg)
		sb.signal()
	}
	s.mu.Unlock()
	if !ok {
		return nil, notFound(req.GetSubscription())
	}
	return &pubsubpb.DetachSubscriptionResponse{}, s.commit(&w)
}

// dropBacklog removes every entry of a subscription.
func (s *Service) dropBacklog(sb *sub, w *writes) {
	for seq, e := range sb.entries {
		s.endLease(sb, e)
		e.queued = false
		e.qgen++
		w.del(nsBacklog, backlogKey(sb.name, seq))
		s.unref(e.m, w)
	}
	sb.entries = map[uint64]*entry{}
	sb.ready, sb.delayed, sb.leases = nil, nil, nil
	sb.keys = map[string]*keyQueue{}
}

func (p *publisherServer) Publish(ctx context.Context, req *pubsubpb.PublishRequest) (*pubsubpb.PublishResponse, error) {
	s := p.s
	if _, _, err := parseName(req.GetTopic(), kindTopics); err != nil {
		return nil, err
	}
	if err := s.env.Auth.Check(ctx, "pubsub.topics.publish", fullName(req.GetTopic())); err != nil {
		return nil, err
	}
	if err := validatePublish(req); err != nil {
		return nil, err
	}
	ids, err := s.publish(req.GetTopic(), req.GetMessages())
	if err != nil {
		return nil, err
	}
	return &pubsubpb.PublishResponse{MessageIds: ids}, nil
}

// validatePublish enforces GCP's publish limits (FR-PS-002).
func validatePublish(req *pubsubpb.PublishRequest) error {
	n := len(req.GetMessages())
	if n == 0 {
		return apierr.InvalidArgument("The publish request must contain at least one message.")
	}
	if n > maxPublishMessages {
		return tooLarge("message_count", n, maxPublishMessages)
	}
	if size := proto.Size(req); size > maxPublishBytes {
		return tooLarge("request_size", size, maxPublishBytes)
	}
	for _, m := range req.GetMessages() {
		if len(m.GetData()) == 0 && len(m.GetAttributes()) == 0 {
			return apierr.InvalidArgument("One or more messages in the publish request is empty. Each message must contain either non-empty data, or at least one attribute.")
		}
		if len(m.GetAttributes()) > maxAttributes {
			return tooLarge("num_attributes", len(m.GetAttributes()), maxAttributes)
		}
		for k, v := range m.GetAttributes() {
			if k == "" {
				return apierr.InvalidArgument("Message attribute keys must be non-empty.")
			}
			if len(k) > maxAttrKeyBytes {
				return tooLarge("attribute_key_size", len(k), maxAttrKeyBytes)
			}
			if len(v) > maxAttrValueBytes {
				return tooLarge("attribute_value_size", len(v), maxAttrValueBytes)
			}
			if len(k) >= 4 && (k[:4] == "goog") {
				return apierr.InvalidArgument("The attribute key '%s' is reserved; keys may not begin with 'goog'.", k)
			}
		}
		if len(m.GetOrderingKey()) > maxOrderingKeyBytes {
			return tooLarge("ordering_key_size", len(m.GetOrderingKey()), maxOrderingKeyBytes)
		}
	}
	return nil
}

// target is a backlog holder receiving one published message.
type target struct {
	sb   *sub
	sn   *snapshot
	seqs []int // indexes into the publish batch
}

// publish commits messages to the store and then enqueues them on every
// attached subscription and snapshot whose filter matches (FR-PS-002,
// FR-PS-005, NFR-REL-001). It returns the message IDs.
func (s *Service) publish(topicName string, in []*pubsubpb.PubsubMessage) ([]string, error) {
	now := s.env.Clock.Now()
	ts := timestamppb.New(now)
	s.mu.Lock()
	t, ok := s.topics[topicName]
	if !ok {
		s.mu.Unlock()
		return nil, notFound(topicName)
	}
	schema := t.schema
	s.mu.Unlock()
	if schema != nil {
		for _, m := range in {
			if err := schema.validateMessage(m.GetData()); err != nil {
				return nil, apierr.InvalidArgument("Invalid data in message: %v.", err).
					WithReason("pubsub.googleapis.com", "SCHEMA_VALIDATION_FAILED")
			}
		}
	}

	s.mu.Lock()
	t, ok = s.topics[topicName]
	if !ok {
		s.mu.Unlock()
		return nil, notFound(topicName)
	}
	msgs := make([]*message, len(in))
	ids := make([]string, len(in))
	for i, pm := range in {
		s.seq++
		id := strconv.FormatUint(s.seq, 10)
		pb := &pubsubpb.PubsubMessage{
			Data: pm.GetData(), Attributes: pm.GetAttributes(), MessageId: id,
			PublishTime: ts, OrderingKey: pm.GetOrderingKey(),
		}
		msgs[i] = &message{seq: s.seq, topic: topicName, pb: pb, size: int64(proto.Size(pb)), publish: now}
		ids[i] = id
	}
	high := s.seq
	var targets []target
	needed := make([]bool, len(msgs))
	retain := t.retention() > 0
	for _, sb := range t.subs {
		tg := target{sb: sb}
		for i, m := range msgs {
			if filterMatches(sb.filter, m.pb.Attributes) {
				tg.seqs = append(tg.seqs, i)
				needed[i] = true
			}
		}
		if len(tg.seqs) > 0 {
			targets = append(targets, tg)
		}
	}
	for _, sn := range s.snaps {
		if sn.cfg.GetTopic() != topicName {
			continue
		}
		tg := target{sn: sn}
		for i, m := range msgs {
			if filterMatches(sn.filter, m.pb.Attributes) {
				tg.seqs = append(tg.seqs, i)
				needed[i] = true
			}
		}
		if len(tg.seqs) > 0 {
			targets = append(targets, tg)
		}
	}
	s.mu.Unlock()

	var w writes
	w.ops = make([]writeOp, 0, len(msgs)*2+1)
	for i, m := range msgs {
		if !needed[i] && !retain {
			continue
		}
		b, err := encodeMessage(m)
		if err != nil {
			return nil, err
		}
		w.put(nsMessages, seqKey(m.seq), b)
	}
	for _, tg := range targets {
		holder := ""
		if tg.sb != nil {
			holder = tg.sb.name
		} else {
			holder = tg.sn.name
		}
		for _, i := range tg.seqs {
			w.put(nsBacklog, backlogKey(holder, msgs[i].seq), stateUnacked)
		}
	}
	w.put(nsMeta, "seq", encodeSeq(high))
	if err := s.commit(&w); err != nil {
		return nil, apierr.Internal("publish: %v", err)
	}

	// Make the committed messages visible.
	var cleanup writes
	s.mu.Lock()
	now2 := time.Now()
	for i, m := range msgs {
		if needed[i] || retain {
			s.msgs[m.seq] = m
		}
	}
	if t2 := s.topics[topicName]; t2 == t && retain {
		for _, m := range msgs {
			m.refs++
			t.log = append(t.log, m)
		}
	}
	for _, tg := range targets {
		switch {
		case tg.sb != nil:
			sb := tg.sb
			if sb.deleted || sb.detached() || s.subs[sb.name] != sb {
				for _, i := range tg.seqs {
					cleanup.del(nsBacklog, backlogKey(sb.name, msgs[i].seq))
				}
				continue
			}
			for _, i := range tg.seqs {
				m := msgs[i]
				e := newEntry(m)
				sb.entries[m.seq] = e
				m.refs++
				s.enqueue(sb, e, now2)
			}
		case tg.sn != nil:
			if s.snaps[tg.sn.name] != tg.sn {
				for _, i := range tg.seqs {
					cleanup.del(nsBacklog, backlogKey(tg.sn.name, msgs[i].seq))
				}
				continue
			}
			for _, i := range tg.seqs {
				tg.sn.msgs[msgs[i].seq] = msgs[i]
				msgs[i].refs++
			}
		}
	}
	for i, m := range msgs {
		if (needed[i] || retain) && m.refs == 0 {
			delete(s.msgs, m.seq)
			cleanup.del(nsMessages, seqKey(m.seq))
		}
	}
	s.mu.Unlock()
	if err := s.commit(&cleanup); err != nil {
		s.env.Log.Warn("pubsub: publish cleanup", "err", err)
	}
	return ids, nil
}

// PublishInternal implements emu.Publisher for other services (FR-GCS-007).
func (s *Service) PublishInternal(ctx context.Context, topicName string, data []byte, attrs map[string]string) (string, error) {
	req := &pubsubpb.PublishRequest{Topic: topicName, Messages: []*pubsubpb.PubsubMessage{{Data: data, Attributes: attrs}}}
	if len(data) == 0 && len(attrs) == 0 {
		return "", apierr.InvalidArgument("One or more messages in the publish request is empty. Each message must contain either non-empty data, or at least one attribute.")
	}
	ids, err := s.publish(topicName, req.Messages)
	if err != nil {
		return "", err
	}
	return ids[0], nil
}

// TopicExists implements emu.Publisher.
func (s *Service) TopicExists(ctx context.Context, topicName string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.topics[topicName]
	return ok
}
