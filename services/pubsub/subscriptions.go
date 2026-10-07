package pubsub

import (
	"context"
	"net/url"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// subscriberServer implements google.pubsub.v1.Subscriber.
type subscriberServer struct {
	pubsubpb.UnimplementedSubscriberServer
	s *Service
}

// subUpdatable lists the Subscription fields UpdateSubscription may change.
var subUpdatable = map[string]bool{
	"push_config": true, "bigquery_config": true, "cloud_storage_config": true,
	"ack_deadline_seconds": true, "retain_acked_messages": true,
	"message_retention_duration": true, "labels": true, "expiration_policy": true,
	"dead_letter_policy": true, "retry_policy": true,
	"enable_exactly_once_delivery": true, "message_transforms": true,
}

// normalizeSubscription validates a subscription config and fills in
// GCP's defaults (FR-PS-001, FR-PS-004).
func normalizeSubscription(cfg *pubsubpb.Subscription) (filterExpr, error) {
	if err := validateLabels(cfg.GetLabels()); err != nil {
		return nil, err
	}
	switch d := cfg.GetAckDeadlineSeconds(); {
	case d == 0:
		cfg.AckDeadlineSeconds = defaultAckDeadline
	case d < minAckDeadline || d > maxAckDeadline:
		return nil, apierr.InvalidArgument("Invalid ack_deadline_seconds: %d. The minimum deadline you can specify is 10 seconds. The maximum deadline you can specify is 600 seconds.", d)
	}
	if cfg.MessageRetentionDuration == nil {
		cfg.MessageRetentionDuration = durationpb.New(defaultSubRetention)
	} else if err := validateRetention("message_retention_duration", cfg.MessageRetentionDuration); err != nil {
		return nil, err
	}
	if cfg.ExpirationPolicy == nil {
		cfg.ExpirationPolicy = &pubsubpb.ExpirationPolicy{Ttl: durationpb.New(defaultExpirationTTL)}
	} else if ttl := cfg.ExpirationPolicy.GetTtl(); ttl != nil && ttl.AsDuration() < minExpirationTTL {
		return nil, apierr.InvalidArgument("Invalid expiration_policy.ttl: the minimum allowed value is 1 day.")
	}
	f, err := compileFilter(cfg.GetFilter())
	if err != nil {
		return nil, apierr.InvalidArgument("Invalid filter expression: %v.", err)
	}
	if dl := cfg.GetDeadLetterPolicy(); dl != nil {
		if dl.GetDeadLetterTopic() == "" {
			cfg.DeadLetterPolicy = nil
		} else {
			if _, _, err := parseName(dl.GetDeadLetterTopic(), kindTopics); err != nil {
				return nil, err
			}
			if dl.MaxDeliveryAttempts == 0 {
				dl.MaxDeliveryAttempts = defaultMaxDeliveryAttempts
			}
			if dl.MaxDeliveryAttempts < minDeliveryAttempts || dl.MaxDeliveryAttempts > maxDeliveryAttempts {
				return nil, apierr.InvalidArgument("Invalid dead_letter_policy.max_delivery_attempts: %d. It must be between 5 and 100.", dl.MaxDeliveryAttempts)
			}
		}
	}
	if rp := cfg.GetRetryPolicy(); rp != nil {
		if rp.MinimumBackoff == nil {
			rp.MinimumBackoff = durationpb.New(defaultMinBackoff)
		}
		if rp.MaximumBackoff == nil {
			rp.MaximumBackoff = durationpb.New(defaultMaxBackoff)
		}
		lo, hi := rp.MinimumBackoff.AsDuration(), rp.MaximumBackoff.AsDuration()
		if lo < 0 || hi < 0 || lo > maxBackoff || hi > maxBackoff || lo > hi {
			return nil, apierr.InvalidArgument("Invalid retry_policy: backoffs must be between 0 and 600 seconds and minimum_backoff must not exceed maximum_backoff.")
		}
	}
	if pc := cfg.GetPushConfig(); pc == nil {
		cfg.PushConfig = &pubsubpb.PushConfig{}
	} else if ep := pc.GetPushEndpoint(); ep != "" {
		u, err := url.Parse(ep)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, apierr.InvalidArgument("Invalid push_config.push_endpoint: %q is not a valid URL.", ep)
		}
		if oidc := pc.GetOidcToken(); oidc != nil && oidc.GetServiceAccountEmail() == "" {
			return nil, apierr.InvalidArgument("push_config.oidc_token.service_account_email must be set.")
		}
		if cfg.GetEnableExactlyOnceDelivery() {
			return nil, apierr.InvalidArgument("Exactly once delivery is not supported for push subscriptions.")
		}
	}
	cfg.State = pubsubpb.Subscription_ACTIVE
	return f, nil
}

func (p *subscriberServer) CreateSubscription(ctx context.Context, req *pubsubpb.Subscription) (*pubsubpb.Subscription, error) {
	return p.s.createSubscription(ctx, req, true)
}

// createSubscription creates a subscription; check=false skips IAM (seeding).
func (s *Service) createSubscription(ctx context.Context, req *pubsubpb.Subscription, check bool) (*pubsubpb.Subscription, error) {
	project, _, err := parseName(req.GetName(), kindSubscriptions)
	if err != nil {
		return nil, err
	}
	if req.GetTopic() == "" {
		return nil, apierr.InvalidArgument("The topic field in the Subscription must be set.")
	}
	if _, _, err := parseName(req.GetTopic(), kindTopics); err != nil {
		return nil, err
	}
	if err := s.env.EnsureProject(project); err != nil {
		return nil, err
	}
	if check {
		if err := s.env.Auth.Check(ctx, "pubsub.subscriptions.create", projectResource(project)); err != nil {
			return nil, err
		}
		if err := s.env.Auth.Check(ctx, "pubsub.topics.attachSubscription", fullName(req.GetTopic())); err != nil {
			return nil, err
		}
	}
	cfg := proto.Clone(req).(*pubsubpb.Subscription)
	cfg.Detached = false
	f, err := normalizeSubscription(cfg)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	t, ok := s.topics[cfg.Topic]
	if !ok {
		s.mu.Unlock()
		return nil, notFound(cfg.Topic)
	}
	if _, ok := s.subs[cfg.Name]; ok {
		s.mu.Unlock()
		return nil, alreadyExists(cfg.Name)
	}
	cfg.TopicMessageRetentionDuration = t.cfg.GetMessageRetentionDuration()
	sb := newSub(cfg.Name, cfg, f)
	s.subs[cfg.Name] = sb
	t.subs[cfg.Name] = sb
	var w writes
	w.putProto(nsSubs, cfg.Name, cfg)
	s.syncPush(sb)
	out := proto.Clone(cfg).(*pubsubpb.Subscription)
	s.mu.Unlock()
	return out, s.commit(&w)
}

// subscription looks up a subscription under s.mu.
func (s *Service) subscription(name string) (*sub, error) {
	sb, ok := s.subs[name]
	if !ok {
		return nil, notFound(name)
	}
	return sb, nil
}

func (p *subscriberServer) GetSubscription(ctx context.Context, req *pubsubpb.GetSubscriptionRequest) (*pubsubpb.Subscription, error) {
	s := p.s
	if _, _, err := parseName(req.GetSubscription(), kindSubscriptions); err != nil {
		return nil, err
	}
	if err := s.env.Auth.Check(ctx, "pubsub.subscriptions.get", fullName(req.GetSubscription())); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sb, err := s.subscription(req.GetSubscription())
	if err != nil {
		return nil, err
	}
	return proto.Clone(sb.cfg).(*pubsubpb.Subscription), nil
}

func (p *subscriberServer) UpdateSubscription(ctx context.Context, req *pubsubpb.UpdateSubscriptionRequest) (*pubsubpb.Subscription, error) {
	s := p.s
	if req.GetSubscription() == nil {
		return nil, apierr.InvalidArgument("The subscription field in the UpdateSubscriptionRequest must be set.")
	}
	name := req.GetSubscription().GetName()
	if _, _, err := parseName(name, kindSubscriptions); err != nil {
		return nil, err
	}
	if err := s.env.Auth.Check(ctx, "pubsub.subscriptions.update", fullName(name)); err != nil {
		return nil, err
	}
	s.mu.Lock()
	sb, err := s.subscription(name)
	var cur *pubsubpb.Subscription
	if err == nil {
		cur = proto.Clone(sb.cfg).(*pubsubpb.Subscription)
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := applyMask(cur, req.GetSubscription(), req.GetUpdateMask().GetPaths(), subUpdatable, "Subscription"); err != nil {
		return nil, err
	}
	return s.replaceSubscription(sb, cur)
}

// replaceSubscription validates and installs a new config for sb.
func (s *Service) replaceSubscription(sb *sub, cur *pubsubpb.Subscription) (*pubsubpb.Subscription, error) {
	f, err := normalizeSubscription(cur)
	if err != nil {
		return nil, err
	}
	var w writes
	s.mu.Lock()
	if s.subs[sb.name] != sb {
		s.mu.Unlock()
		return nil, notFound(sb.name)
	}
	cur.Detached = sb.cfg.GetDetached()
	cur.Topic = sb.cfg.GetTopic()
	cur.TopicMessageRetentionDuration = sb.cfg.GetTopicMessageRetentionDuration()
	ordering := sb.cfg.GetEnableMessageOrdering()
	sb.cfg = cur
	sb.filter = f
	if !cur.GetRetainAckedMessages() {
		for seq, e := range sb.entries {
			if e.acked {
				delete(sb.entries, seq)
				w.del(nsBacklog, backlogKey(sb.name, seq))
				s.unref(e.m, &w)
			}
		}
	}
	if ordering != cur.GetEnableMessageOrdering() {
		s.rebuildQueues(sb, time.Now())
	}
	w.putProto(nsSubs, sb.name, cur)
	s.syncPush(sb)
	sb.signal()
	out := proto.Clone(cur).(*pubsubpb.Subscription)
	s.mu.Unlock()
	return out, s.commit(&w)
}

func (p *subscriberServer) ListSubscriptions(ctx context.Context, req *pubsubpb.ListSubscriptionsRequest) (*pubsubpb.ListSubscriptionsResponse, error) {
	s := p.s
	project, err := parseProject(req.GetProject())
	if err != nil {
		return nil, err
	}
	if err := s.env.EnsureProject(project); err != nil {
		return nil, err
	}
	if err := s.env.Auth.Check(ctx, "pubsub.subscriptions.list", projectResource(project)); err != nil {
		return nil, err
	}
	s.mu.Lock()
	var names []string
	for n := range s.subs {
		if projectOf(n) == project {
			names = append(names, n)
		}
	}
	s.mu.Unlock()
	page, next, err := paginate(names, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	resp := &pubsubpb.ListSubscriptionsResponse{NextPageToken: next}
	s.mu.Lock()
	for _, n := range page {
		if sb, ok := s.subs[n]; ok {
			resp.Subscriptions = append(resp.Subscriptions, proto.Clone(sb.cfg).(*pubsubpb.Subscription))
		}
	}
	s.mu.Unlock()
	return resp, nil
}

func (p *subscriberServer) DeleteSubscription(ctx context.Context, req *pubsubpb.DeleteSubscriptionRequest) (*emptypb.Empty, error) {
	s := p.s
	if _, _, err := parseName(req.GetSubscription(), kindSubscriptions); err != nil {
		return nil, err
	}
	if err := s.env.Auth.Check(ctx, "pubsub.subscriptions.delete", fullName(req.GetSubscription())); err != nil {
		return nil, err
	}
	var w writes
	s.mu.Lock()
	sb, err := s.subscription(req.GetSubscription())
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	delete(s.subs, sb.name)
	if t := s.topics[sb.cfg.GetTopic()]; t != nil && t.subs[sb.name] == sb {
		delete(t.subs, sb.name)
	}
	s.dropBacklog(sb, &w)
	s.stopPush(sb)
	sb.deleted = true
	sb.signal()
	w.del(nsSubs, sb.name)
	s.mu.Unlock()
	return &emptypb.Empty{}, s.commit(&w)
}

func (p *subscriberServer) ModifyPushConfig(ctx context.Context, req *pubsubpb.ModifyPushConfigRequest) (*emptypb.Empty, error) {
	s := p.s
	if _, _, err := parseName(req.GetSubscription(), kindSubscriptions); err != nil {
		return nil, err
	}
	if err := s.env.Auth.Check(ctx, "pubsub.subscriptions.update", fullName(req.GetSubscription())); err != nil {
		return nil, err
	}
	s.mu.Lock()
	sb, err := s.subscription(req.GetSubscription())
	var cur *pubsubpb.Subscription
	if err == nil {
		cur = proto.Clone(sb.cfg).(*pubsubpb.Subscription)
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	cur.PushConfig = req.GetPushConfig()
	if _, err := s.replaceSubscription(sb, cur); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}
