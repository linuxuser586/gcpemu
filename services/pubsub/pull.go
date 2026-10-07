package pubsub

import (
	"context"
	"errors"
	"io"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

const (
	// maxPullWait bounds how long a non-immediate Pull blocks when no
	// message is available.
	maxPullWait = 10 * time.Second
	// maxResponseBytes bounds one StreamingPull response (a single larger
	// message is still delivered on its own).
	maxResponseBytes = 4_000_000
	// Ack ID failure reasons understood by the client libraries (FR-PS-007).
	permanentInvalidAckID = "PERMANENT_FAILURE_INVALID_ACK_ID"
)

func detachedErr() error {
	return apierr.FailedPrecondition("Subscription has been detached from its topic.").
		WithReason("pubsub.googleapis.com", "SUBSCRIPTION_DETACHED")
}

// consumable looks up a subscription that can be pulled from (s.mu held).
func (s *Service) consumable(name string) (*sub, error) {
	sb, err := s.subscription(name)
	if err != nil {
		return nil, err
	}
	if sb.detached() {
		return nil, detachedErr()
	}
	return sb, nil
}

func (s *Service) checkConsume(ctx context.Context, name string) error {
	if _, _, err := parseName(name, kindSubscriptions); err != nil {
		return err
	}
	return s.env.Auth.Check(ctx, "pubsub.subscriptions.consume", fullName(name))
}

func (p *subscriberServer) Pull(ctx context.Context, req *pubsubpb.PullRequest) (*pubsubpb.PullResponse, error) {
	s := p.s
	if err := s.checkConsume(ctx, req.GetSubscription()); err != nil {
		return nil, err
	}
	if req.GetMaxMessages() <= 0 {
		return nil, apierr.InvalidArgument("The value for max_messages must be greater than 0.")
	}
	maxN := min(int(req.GetMaxMessages()), maxPullMessages)
	timer := time.NewTimer(maxPullWait)
	defer timer.Stop()
	for {
		s.mu.Lock()
		sb, err := s.consumable(req.GetSubscription())
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		es := s.take(sb, maxN, 0, nil, time.Duration(sb.cfg.GetAckDeadlineSeconds())*time.Second, time.Now())
		resp := &pubsubpb.PullResponse{}
		for _, e := range es {
			resp.ReceivedMessages = append(resp.ReceivedMessages, receivedMessage(sb, e))
		}
		ch := sb.notify
		s.mu.Unlock()
		//lint:ignore SA1019 return_immediately is deprecated but still honoured.
		if len(es) > 0 || req.GetReturnImmediately() { //nolint:staticcheck
			return resp, nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return resp, nil
		case <-timer.C:
			return resp, nil
		case <-s.stopping:
			return nil, apierr.New(codes.Unavailable, "The service is shutting down.")
		}
	}
}

func (p *subscriberServer) Acknowledge(ctx context.Context, req *pubsubpb.AcknowledgeRequest) (*emptypb.Empty, error) {
	s := p.s
	if err := s.checkConsume(ctx, req.GetSubscription()); err != nil {
		return nil, err
	}
	if len(req.GetAckIds()) == 0 {
		return nil, apierr.InvalidArgument("No ack ids specified.")
	}
	_, failed, err := s.acknowledge(req.GetSubscription(), req.GetAckIds())
	if err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, ackFailure(failed)
}

// ackFailure renders exactly-once ack ID failures the way the client
// libraries expect: INVALID_ARGUMENT with ErrorInfo metadata keyed by ack ID.
func ackFailure(failed []string) error {
	if len(failed) == 0 {
		return nil
	}
	md := make(map[string]string, len(failed))
	for _, id := range failed {
		md[id] = permanentInvalidAckID
	}
	e := apierr.InvalidArgument("Some acknowledgement ids in the request were invalid. This could be because the acknowledgement ids have expired or the acknowledgement ids were malformed.").
		WithReason("pubsub.googleapis.com", "EXACTLY_ONCE_ACKID_FAILURE")
	e.Metadata = md
	return e
}

// acknowledge acks ids; with exactly-once delivery only current,
// unexpired leases may be acked and the others are returned as failed.
func (s *Service) acknowledge(name string, ids []string) (ok, failed []string, err error) {
	var w writes
	now := time.Now()
	s.mu.Lock()
	sb, err := s.consumable(name)
	if err != nil {
		s.mu.Unlock()
		return nil, nil, err
	}
	eod := sb.cfg.GetEnableExactlyOnceDelivery()
	for _, id := range ids {
		var e *entry
		if eod {
			e = sb.lookupLease(id)
			if e == nil || now.After(e.deadline) {
				failed = append(failed, id)
				continue
			}
		} else {
			e = sb.lookupAny(id)
			if e == nil || e.dlq {
				ok = append(ok, id)
				continue
			}
		}
		s.ackEntry(sb, e, &w, now)
		ok = append(ok, id)
	}
	s.mu.Unlock()
	if err := s.commit(&w); err != nil {
		return nil, nil, apierr.Internal("acknowledge: %v", err)
	}
	return ok, failed, nil
}

func (p *subscriberServer) ModifyAckDeadline(ctx context.Context, req *pubsubpb.ModifyAckDeadlineRequest) (*emptypb.Empty, error) {
	s := p.s
	if err := s.checkConsume(ctx, req.GetSubscription()); err != nil {
		return nil, err
	}
	if len(req.GetAckIds()) == 0 {
		return nil, apierr.InvalidArgument("No ack ids specified.")
	}
	d := req.GetAckDeadlineSeconds()
	if d < 0 || d > maxAckDeadline {
		return nil, apierr.InvalidArgument("Invalid ack deadline given: %d. The ack deadline must be between 0 and 600 seconds.", d)
	}
	_, failed, err := s.modifyDeadline(req.GetSubscription(), req.GetAckIds(), d)
	if err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, ackFailure(failed)
}

// modifyDeadline extends leases, or nacks them when seconds is 0.
func (s *Service) modifyDeadline(name string, ids []string, seconds int32) (ok, failed []string, err error) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	sb, err := s.consumable(name)
	if err != nil {
		return nil, nil, err
	}
	eod := sb.cfg.GetEnableExactlyOnceDelivery()
	for _, id := range ids {
		e := sb.lookupLease(id)
		if e == nil || (eod && now.After(e.deadline)) {
			if eod {
				failed = append(failed, id)
			} else {
				ok = append(ok, id)
			}
			continue
		}
		if seconds == 0 {
			s.nack(sb, e, now, false)
		} else {
			s.extend(sb, e, time.Duration(seconds)*time.Second, now)
		}
		ok = append(ok, id)
	}
	return ok, failed, nil
}

func (p *subscriberServer) StreamingPull(stream pubsubpb.Subscriber_StreamingPullServer) error {
	s := p.s
	ctx := stream.Context()
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	name := first.GetSubscription()
	if name == "" {
		return apierr.InvalidArgument("The subscription field must be set in the first StreamingPullRequest.")
	}
	if err := s.checkConsume(ctx, name); err != nil {
		return err
	}
	ackDeadline := streamDeadline(first.GetStreamAckDeadlineSeconds())
	s.mu.Lock()
	sb, err := s.consumable(name)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if ackDeadline == 0 {
		ackDeadline = time.Duration(sb.cfg.GetAckDeadlineSeconds()) * time.Second
	}
	fl := newFlow(first.GetMaxOutstandingMessages(), first.GetMaxOutstandingBytes())
	s.mu.Unlock()

	st := &pullStream{s: s, name: name, fl: fl, deadline: ackDeadline,
		confirms: make(chan *pubsubpb.StreamingPullResponse, 64), errc: make(chan error, 1)}
	if err := st.handle(ctx, first); err != nil {
		return err
	}
	go st.read(ctx, stream)

	for {
		select {
		case c := <-st.confirms:
			if err := stream.Send(c); err != nil {
				return err
			}
			continue
		default:
		}
		s.mu.Lock()
		sb, err := s.consumable(name)
		if err != nil {
			s.mu.Unlock()
			return err
		}
		es := s.take(sb, maxPullMessages, maxResponseBytes, fl, st.deadline, time.Now())
		var resp *pubsubpb.StreamingPullResponse
		if len(es) > 0 {
			resp = &pubsubpb.StreamingPullResponse{SubscriptionProperties: subProps(sb)}
			for _, e := range es {
				resp.ReceivedMessages = append(resp.ReceivedMessages, receivedMessage(sb, e))
			}
		}
		ch := sb.notify
		s.mu.Unlock()
		if resp != nil {
			if err := stream.Send(resp); err != nil {
				return err
			}
			continue
		}
		select {
		case <-ch:
		case <-fl.wake:
		case c := <-st.confirms:
			if err := stream.Send(c); err != nil {
				return err
			}
		case err := <-st.errc:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case <-ctx.Done():
			return ctx.Err()
		case <-s.stopping:
			return apierr.New(codes.Unavailable, "The service is shutting down.")
		}
	}
}

func subProps(sb *sub) *pubsubpb.StreamingPullResponse_SubscriptionProperties {
	return &pubsubpb.StreamingPullResponse_SubscriptionProperties{
		ExactlyOnceDeliveryEnabled: sb.cfg.GetEnableExactlyOnceDelivery(),
		MessageOrderingEnabled:     sb.cfg.GetEnableMessageOrdering(),
	}
}

// streamDeadline clamps a stream ack deadline to GCP's 10–600s range
// (0 means "use the subscription's").
func streamDeadline(sec int32) time.Duration {
	if sec <= 0 {
		return 0
	}
	return time.Duration(min(max(sec, minAckDeadline), maxAckDeadline)) * time.Second
}

// pullStream is one StreamingPull call (FR-PS-002).
type pullStream struct {
	s        *Service
	name     string
	fl       *flow
	deadline time.Duration
	confirms chan *pubsubpb.StreamingPullResponse
	errc     chan error
}

func (st *pullStream) read(ctx context.Context, stream pubsubpb.Subscriber_StreamingPullServer) {
	for {
		req, err := stream.Recv()
		if err != nil {
			st.errc <- err
			return
		}
		if err := st.handle(ctx, req); err != nil {
			st.errc <- err
			return
		}
	}
}

// handle applies the acks, deadline changes and flow-control updates in
// one request; with exactly-once delivery it queues confirmations.
func (st *pullStream) handle(ctx context.Context, req *pubsubpb.StreamingPullRequest) error {
	s := st.s
	if n := len(req.GetModifyDeadlineAckIds()); n != len(req.GetModifyDeadlineSeconds()) {
		return apierr.InvalidArgument("modify_deadline_seconds and modify_deadline_ack_ids must be the same size.")
	}
	if d := streamDeadline(req.GetStreamAckDeadlineSeconds()); d > 0 {
		st.deadline = d
	}
	if req.GetMaxOutstandingMessages() > 0 || req.GetMaxOutstandingBytes() > 0 {
		s.mu.Lock()
		if v := req.GetMaxOutstandingMessages(); v > 0 {
			st.fl.maxMsgs = v
		}
		if v := req.GetMaxOutstandingBytes(); v > 0 {
			st.fl.maxBytes = v
		}
		s.mu.Unlock()
	}
	var ackConf *pubsubpb.StreamingPullResponse_AcknowledgeConfirmation
	var modConf *pubsubpb.StreamingPullResponse_ModifyAckDeadlineConfirmation
	if ids := req.GetAckIds(); len(ids) > 0 {
		ok, failed, err := s.acknowledge(st.name, ids)
		if err != nil {
			return err
		}
		ackConf = &pubsubpb.StreamingPullResponse_AcknowledgeConfirmation{AckIds: ok, InvalidAckIds: failed}
	}
	if ids := req.GetModifyDeadlineAckIds(); len(ids) > 0 {
		// Group by deadline so each value is applied once.
		byDeadline := map[int32][]string{}
		for i, id := range ids {
			d := req.GetModifyDeadlineSeconds()[i]
			if d < 0 || d > maxAckDeadline {
				return apierr.InvalidArgument("Invalid ack deadline given: %d. The ack deadline must be between 0 and 600 seconds.", d)
			}
			byDeadline[d] = append(byDeadline[d], id)
		}
		modConf = &pubsubpb.StreamingPullResponse_ModifyAckDeadlineConfirmation{}
		for d, group := range byDeadline {
			ok, failed, err := s.modifyDeadline(st.name, group, d)
			if err != nil {
				return err
			}
			modConf.AckIds = append(modConf.AckIds, ok...)
			modConf.InvalidAckIds = append(modConf.InvalidAckIds, failed...)
		}
	}
	if ackConf == nil && modConf == nil {
		return nil
	}
	s.mu.Lock()
	sb, err := s.consumable(st.name)
	var props *pubsubpb.StreamingPullResponse_SubscriptionProperties
	eod := false
	if err == nil {
		props = subProps(sb)
		eod = sb.cfg.GetEnableExactlyOnceDelivery()
	}
	s.mu.Unlock()
	if !eod {
		return nil
	}
	resp := &pubsubpb.StreamingPullResponse{
		AcknowledgeConfirmation:       ackConf,
		ModifyAckDeadlineConfirmation: modConf,
		SubscriptionProperties:        props,
	}
	select {
	case st.confirms <- resp:
	case <-ctx.Done():
	}
	return nil
}
