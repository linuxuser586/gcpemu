package pubsub

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/protobuf/proto"

	"github.com/linuxuser586/gcpemu/internal/emu"
)

// maxPushOutstanding bounds concurrent push requests per subscription.
const maxPushOutstanding = 32

// pushKey identifies the push configuration a worker was started for.
func pushKey(cfg *pubsubpb.Subscription) string {
	if cfg.GetDetached() || cfg.GetPushConfig().GetPushEndpoint() == "" {
		return ""
	}
	b, _ := proto.Marshal(cfg.GetPushConfig())
	return string(b)
}

// syncPush starts, restarts or stops the push worker to match the
// subscription's config (s.mu held). Workers only run after Start.
func (s *Service) syncPush(sb *sub) {
	key := pushKey(sb.cfg)
	if sb.deleted {
		key = ""
	}
	if key == sb.pushState && (key == "" || sb.pushStop != nil) {
		return
	}
	s.stopPush(sb)
	if key == "" || !s.started.Load() || s.ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	sb.pushStop, sb.pushState = cancel, key
	s.wg.Add(1)
	go s.pushLoop(ctx, sb)
}

func (s *Service) stopPush(sb *sub) {
	if sb.pushStop != nil {
		sb.pushStop()
		sb.pushStop = nil
	}
	sb.pushState = ""
}

// pushDelivery is one push attempt, captured under s.mu.
type pushDelivery struct {
	ackID    string
	msg      *pubsubpb.PubsubMessage
	attempt  int32
	withDLQ  bool
	cfg      *pubsubpb.PushConfig
	deadline time.Duration
}

// pushLoop delivers a push subscription's messages (FR-PS-006).
func (s *Service) pushLoop(ctx context.Context, sb *sub) {
	defer s.wg.Done()
	fl := newFlow(maxPushOutstanding, 0)
	for {
		s.mu.Lock()
		if sb.deleted || ctx.Err() != nil {
			s.mu.Unlock()
			return
		}
		deadline := time.Duration(sb.cfg.GetAckDeadlineSeconds()) * time.Second
		es := s.take(sb, maxPushOutstanding, 0, fl, deadline, time.Now())
		ds := make([]pushDelivery, len(es))
		for i, e := range es {
			ds[i] = pushDelivery{
				ackID: e.ackID, msg: e.m.pb, attempt: e.attempts,
				withDLQ:  sb.cfg.GetDeadLetterPolicy().GetDeadLetterTopic() != "",
				cfg:      sb.cfg.GetPushConfig(),
				deadline: deadline,
			}
		}
		ch := sb.notify
		s.mu.Unlock()
		for _, d := range ds {
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.pushOne(ctx, sb, d)
			}()
		}
		if len(ds) > 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ch:
		case <-fl.wake:
		}
	}
}

// pushOne POSTs one message; 102 and 2xx acknowledge it, anything else
// nacks it for redelivery with backoff.
func (s *Service) pushOne(ctx context.Context, sb *sub, d pushDelivery) {
	ok := s.doPush(ctx, sb.name, d)
	now := time.Now()
	var w writes
	s.mu.Lock()
	if e := sb.lookupLease(d.ackID); e != nil && s.subs[sb.name] == sb {
		if ok {
			s.ackEntry(sb, e, &w, now)
		} else {
			s.nack(sb, e, now, true)
		}
	}
	s.mu.Unlock()
	_ = s.commit(&w)
}

// pushBody is the wrapped push payload.
type pushBody struct {
	Message         pushMessage `json:"message"`
	Subscription    string      `json:"subscription"`
	DeliveryAttempt int32       `json:"deliveryAttempt,omitempty"`
}

type pushMessage struct {
	Attributes   map[string]string `json:"attributes,omitempty"`
	Data         string            `json:"data,omitempty"`
	MessageID    string            `json:"messageId"`
	MessageID2   string            `json:"message_id"`
	OrderingKey  string            `json:"orderingKey,omitempty"`
	PublishTime  string            `json:"publishTime"`
	PublishTime2 string            `json:"publish_time"`
}

func (s *Service) doPush(ctx context.Context, subName string, d pushDelivery) bool {
	endpoint := d.cfg.GetPushEndpoint()
	m := d.msg
	publishTime := m.GetPublishTime().AsTime().Format(time.RFC3339Nano)
	var body []byte
	hdr := http.Header{}
	if nw := d.cfg.GetNoWrapper(); nw != nil {
		body = m.GetData()
		if nw.GetWriteMetadata() {
			hdr.Set("x-goog-pubsub-subscription-name", subName)
			hdr.Set("x-goog-pubsub-message-id", m.GetMessageId())
			hdr.Set("x-goog-pubsub-publish-time", publishTime)
			if m.GetOrderingKey() != "" {
				hdr.Set("x-goog-pubsub-ordering-key", m.GetOrderingKey())
			}
			for k, v := range m.GetAttributes() {
				hdr.Set(k, v)
			}
		}
	} else {
		pb := pushBody{
			Message: pushMessage{
				Attributes: m.GetAttributes(), MessageID: m.GetMessageId(), MessageID2: m.GetMessageId(),
				OrderingKey: m.GetOrderingKey(), PublishTime: publishTime, PublishTime2: publishTime,
			},
			Subscription: subName,
		}
		if len(m.GetData()) > 0 {
			pb.Message.Data = base64.StdEncoding.EncodeToString(m.GetData())
		}
		if d.withDLQ {
			pb.DeliveryAttempt = d.attempt
		}
		body, _ = json.Marshal(pb)
		hdr.Set("Content-Type", "application/json")
	}
	if oidc := d.cfg.GetOidcToken(); oidc != nil {
		aud := oidc.GetAudience()
		if aud == "" {
			aud = endpoint
		}
		tok, err := s.idToken(ctx, oidc.GetServiceAccountEmail(), aud)
		if err != nil {
			s.env.Log.Warn("pubsub: push OIDC token", "subscription", subName, "err", err)
			return false
		}
		if tok != "" {
			hdr.Set("Authorization", "Bearer "+tok)
		}
	}
	rctx, cancel := context.WithTimeout(ctx, d.deadline)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header = hdr
	req.Header.Set("User-Agent", "APIs-Google; (+https://developers.google.com/webmasters/APIs-Google.html)")
	resp, err := s.push.Do(req)
	if err != nil {
		s.env.Log.Debug("pubsub: push failed", "subscription", subName, "endpoint", endpoint, "err", err)
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusProcessing || (resp.StatusCode >= 200 && resp.StatusCode < 300)
}

// idToken mints an OIDC token via the IAM service; without IAM the push is
// sent unauthenticated.
func (s *Service) idToken(ctx context.Context, email, aud string) (string, error) {
	svc, ok := s.env.Lookup("iam")
	if !ok {
		return "", nil
	}
	keys, ok := svc.(emu.ServiceAccountKeys)
	if !ok {
		return "", nil
	}
	return keys.IDToken(ctx, email, aud)
}
