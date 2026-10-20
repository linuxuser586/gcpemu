package pubsub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// Service subscriptions (emu.Subscriber): push subscriptions another
// Service owns, such as a Cloud Build pubsubConfig Build trigger's. They
// are ordinary subscriptions, listed with the rest, but not exported:
// the owner makes them again from its own Seed file section. Their
// pushes, to a Google API host the owner serves, stay in process.

// ServePushHost implements emu.Subscriber.
func (s *Service) ServePushHost(host string, h http.Handler) {
	s.hostsMu.Lock()
	defer s.hostsMu.Unlock()
	if s.hosts == nil {
		s.hosts = map[string]http.Handler{}
	}
	s.hosts[host] = h
}

// EnsurePushSubscription implements emu.Subscriber.
func (s *Service) EnsurePushSubscription(ctx context.Context, name, topic, endpoint string) error {
	cfg := &pubsubpb.Subscription{Name: name, Topic: topic, PushConfig: &pubsubpb.PushConfig{PushEndpoint: endpoint}}
	_, err := s.createSubscription(ctx, cfg, false)
	if err == nil || apierr.From(err).Code != codes.AlreadyExists {
		return err
	}
	s.mu.Lock()
	sb := s.subs[name]
	var cur *pubsubpb.Subscription
	if sb != nil {
		cur = proto.Clone(sb.cfg).(*pubsubpb.Subscription)
	}
	s.mu.Unlock()
	if sb == nil {
		return s.EnsurePushSubscription(ctx, name, topic, endpoint)
	}
	if cur.GetTopic() != topic {
		// A subscription's topic cannot change: make it again.
		if err := s.deleteSubscription(name); err != nil && apierr.From(err).Code != codes.NotFound {
			return err
		}
		return s.EnsurePushSubscription(ctx, name, topic, endpoint)
	}
	if cur.GetPushConfig().GetPushEndpoint() == endpoint {
		return nil
	}
	cur.PushConfig = cfg.PushConfig
	_, err = s.replaceSubscription(sb, cur)
	return err
}

// DeleteSubscriptionInternal implements emu.Subscriber.
func (s *Service) DeleteSubscriptionInternal(ctx context.Context, name string) error {
	if err := s.deleteSubscription(name); err != nil && apierr.From(err).Code != codes.NotFound {
		return err
	}
	return nil
}

// servicePush reports whether a subscription pushes to a host another
// Service serves, which owns it.
func (s *Service) servicePush(cfg *pubsubpb.Subscription) bool {
	u, err := url.Parse(cfg.GetPushConfig().GetPushEndpoint())
	if err != nil || u.Scheme != "https" {
		return false
	}
	s.hostsMu.Lock()
	defer s.hostsMu.Unlock()
	return s.hosts[u.Hostname()] != nil
}

// hostTransport serves the pushes to a host in s.hosts in process, and
// sends the rest with next.
type hostTransport struct {
	s    *Service
	next http.RoundTripper
}

func (t hostTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.s.hostsMu.Lock()
	h := t.s.hosts[r.URL.Hostname()]
	t.s.hostsMu.Unlock()
	if h == nil || r.URL.Scheme != "https" {
		return t.next.RoundTrip(r)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Result(), nil
}
