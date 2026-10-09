package pubsub

import (
	"maps"
	"strconv"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
)

// dlqItem is a message waiting to be forwarded to a dead-letter topic.
type dlqItem struct {
	sb *sub
	e  *entry
}

// queueDeadLetter hands e to the forwarding loop (s.mu held). Forwarding
// publishes, which takes s.mu, so it cannot happen inline.
func (s *Service) queueDeadLetter(sb *sub, e *entry) {
	s.dlqMu.Lock()
	s.dlqQ = append(s.dlqQ, dlqItem{sb, e})
	s.dlqMu.Unlock()
	select {
	case s.dlqWake <- struct{}{}:
	default:
	}
}

// dlqLoop forwards dead-lettered messages (FR-PS-004).
func (s *Service) dlqLoop() {
	defer s.wg.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.dlqWake:
		}
		s.dlqMu.Lock()
		items := s.dlqQ
		s.dlqQ = nil
		s.dlqMu.Unlock()
		for _, it := range items {
			s.forwardDeadLetter(it.sb, it.e)
		}
	}
}

// forwardDeadLetter publishes e to the dead-letter topic with the source
// attributes GCP adds, then acks it on the source subscription. If the
// dead-letter topic is unavailable the message stays on the subscription.
func (s *Service) forwardDeadLetter(sb *sub, e *entry) {
	s.mu.Lock()
	if s.subs[sb.name] != sb || sb.entries[e.m.seq] != e || !e.dlq {
		s.mu.Unlock()
		return
	}
	dlt := sb.cfg.GetDeadLetterPolicy().GetDeadLetterTopic()
	attempts := e.attempts
	s.mu.Unlock()

	src := e.m.pb
	attrs := make(map[string]string, len(src.GetAttributes())+4)
	maps.Copy(attrs, src.GetAttributes())
	attrs["CloudPubSubDeadLetterSourceDeliveryCount"] = strconv.Itoa(int(attempts))
	attrs["CloudPubSubDeadLetterSourceSubscription"] = lastSegment(sb.name)
	attrs["CloudPubSubDeadLetterSourceSubscriptionProject"] = projectOf(sb.name)
	attrs["CloudPubSubDeadLetterSourceTopicPublishTime"] = src.GetPublishTime().AsTime().Format(time.RFC3339Nano)
	_, err := s.publish(dlt, []*pubsubpb.PubsubMessage{{Data: src.GetData(), Attributes: attrs, OrderingKey: src.GetOrderingKey()}})

	var w writes
	now := time.Now()
	s.mu.Lock()
	if s.subs[sb.name] == sb && sb.entries[e.m.seq] == e && e.dlq {
		if err != nil {
			s.env.Log.Warn("pubsub: dead-letter forwarding failed", "subscription", sb.name, "topic", dlt, "err", err)
			e.dlq = false
			e.availAt = now.Add(retryBackoff(sb.cfg, e.attempts, true))
			s.enqueue(sb, e, now)
		} else {
			s.ackEntry(sb, e, &w, now)
			sb.deadLettered++
		}
	}
	s.mu.Unlock()
	_ = s.commit(&w)
}
