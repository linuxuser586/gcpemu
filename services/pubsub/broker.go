package pubsub

import (
	"container/heap"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
)

// The broker keeps all hot-path state in memory, guarded by Service.mu, and
// writes through to the store (NFR-REL-001, NFR-PERF-006). Lease state is
// memory-only: after a restart every unacked message is deliverable again.

// message is one published message. Its proto is immutable once published.
type message struct {
	seq     uint64
	topic   string
	pb      *pubsubpb.PubsubMessage
	size    int64
	publish time.Time
	// refs counts subscriptions, snapshots and the topic retention log
	// holding the message; at zero the message is deleted.
	refs int
}

// topic is a topic and its retention log.
type topic struct {
	name   string
	cfg    *pubsubpb.Topic
	subs   map[string]*sub
	schema *compiledSchema
	// log holds messages retained by messageRetentionDuration (FR-PS-008),
	// in publish order.
	log []*message
}

func (t *topic) retention() time.Duration {
	if d := t.cfg.GetMessageRetentionDuration(); d != nil {
		return d.AsDuration()
	}
	return 0
}

// entry is a message in a subscription's backlog.
type entry struct {
	m        *message
	acked    bool // retained after ack (retainAckedMessages)
	attempts int32
	// Lease state (memory only).
	ackID    string
	deadline time.Time
	owner    *flow
	hidx     int // index in sub.leases, -1 when absent
	// Queue state.
	queued  bool
	qgen    uint64
	availAt time.Time
	// dlq is set while the message is being forwarded to the dead-letter topic.
	dlq bool
}

func (e *entry) leased() bool { return e.ackID != "" }

// keyQueue serialises delivery of one ordering key (FR-PS-003).
type keyQueue struct {
	key     string
	q       []*entry // pending, sorted by seq
	out     map[*entry]struct{}
	inReady bool
	availAt time.Time
}

// flow tracks outstanding messages for one consumer (a streaming pull
// stream or a push worker) against its flow-control limits.
type flow struct {
	maxMsgs, maxBytes int64
	outMsgs, outBytes int64
	wake              chan struct{}
}

func newFlow(maxMsgs, maxBytes int64) *flow {
	return &flow{maxMsgs: maxMsgs, maxBytes: maxBytes, wake: make(chan struct{}, 1)}
}

func (f *flow) allows(size int64) bool {
	if f == nil {
		return true
	}
	if f.maxMsgs > 0 && f.outMsgs >= f.maxMsgs {
		return false
	}
	if f.maxBytes > 0 && f.outMsgs > 0 && f.outBytes+size > f.maxBytes {
		return false
	}
	return true
}

func (f *flow) release(size int64) {
	f.outMsgs--
	f.outBytes -= size
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// readyItem is either an unordered entry or an ordering key.
type readyItem struct {
	e   *entry
	gen uint64
	key string
}

type delayedItem struct {
	at time.Time
	readyItem
}

type delayedHeap []delayedItem

func (h delayedHeap) Len() int           { return len(h) }
func (h delayedHeap) Less(i, j int) bool { return h[i].at.Before(h[j].at) }
func (h delayedHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *delayedHeap) Push(x any)        { *h = append(*h, x.(delayedItem)) }
func (h *delayedHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// leaseHeap orders leased entries by deadline; each entry knows its index
// so acks and extensions update the heap in place.
type leaseHeap []*entry

func (h leaseHeap) Len() int           { return len(h) }
func (h leaseHeap) Less(i, j int) bool { return h[i].deadline.Before(h[j].deadline) }
func (h leaseHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].hidx, h[j].hidx = i, j
}
func (h *leaseHeap) Push(x any) {
	e := x.(*entry)
	e.hidx = len(*h)
	*h = append(*h, e)
}
func (h *leaseHeap) Pop() any {
	old := *h
	e := old[len(old)-1]
	old[len(old)-1] = nil
	*h = old[:len(old)-1]
	e.hidx = -1
	return e
}

// sub is a subscription with its backlog and delivery state.
type sub struct {
	name    string
	cfg     *pubsubpb.Subscription
	filter  filterExpr
	entries map[uint64]*entry

	ready   []readyItem
	delayed delayedHeap
	keys    map[string]*keyQueue
	leases  leaseHeap

	notify  chan struct{}
	deleted bool
	// push worker control (FR-PS-006)
	pushStop  func()
	pushState string
}

func newSub(name string, cfg *pubsubpb.Subscription, f filterExpr) *sub {
	return &sub{
		name: name, cfg: cfg, filter: f,
		entries: map[uint64]*entry{},
		keys:    map[string]*keyQueue{},
		notify:  make(chan struct{}),
	}
}

func (sb *sub) ordered(e *entry) bool {
	return sb.cfg.GetEnableMessageOrdering() && e.m.pb.GetOrderingKey() != ""
}

func (sb *sub) detached() bool { return sb.cfg.GetDetached() }

// signal wakes every waiter on the subscription.
func (sb *sub) signal() {
	close(sb.notify)
	sb.notify = make(chan struct{})
}

// snapshot captures a subscription's unacked messages plus everything
// published to the topic afterwards (FR-PS-008).
type snapshot struct {
	name   string
	cfg    *pubsubpb.Snapshot
	filter filterExpr
	filt   string
	msgs   map[uint64]*message
}

// enqueue makes an unacked, unleased entry deliverable (now or at availAt).
func (s *Service) enqueue(sb *sub, e *entry, now time.Time) {
	if e.acked || e.dlq || e.leased() || e.queued {
		return
	}
	e.queued = true
	e.qgen++
	if sb.ordered(e) {
		k := sb.keys[e.m.pb.OrderingKey]
		if k == nil {
			k = &keyQueue{key: e.m.pb.OrderingKey, out: map[*entry]struct{}{}}
			sb.keys[k.key] = k
		}
		i := sort.Search(len(k.q), func(i int) bool { return k.q[i].m.seq >= e.m.seq })
		k.q = append(k.q, nil)
		copy(k.q[i+1:], k.q[i:])
		k.q[i] = e
		s.keyReady(sb, k, now)
		return
	}
	it := readyItem{e: e, gen: e.qgen}
	if e.availAt.After(now) {
		heap.Push(&sb.delayed, delayedItem{at: e.availAt, readyItem: it})
		return
	}
	sb.ready = append(sb.ready, it)
	sb.signal()
}

// keyReady schedules an ordering key whose head may be delivered.
func (s *Service) keyReady(sb *sub, k *keyQueue, now time.Time) {
	if k.inReady || len(k.out) > 0 || len(k.q) == 0 {
		if len(k.out) == 0 && len(k.q) == 0 {
			delete(sb.keys, k.key)
		}
		return
	}
	k.inReady = true
	if k.availAt.After(now) {
		heap.Push(&sb.delayed, delayedItem{at: k.availAt, readyItem: readyItem{key: k.key}})
		return
	}
	sb.ready = append(sb.ready, readyItem{key: k.key})
	sb.signal()
}

// dequeue removes an entry from whichever queue holds it.
func (s *Service) dequeue(sb *sub, e *entry) {
	if !e.queued {
		return
	}
	e.queued = false
	e.qgen++
	if sb.ordered(e) {
		if k := sb.keys[e.m.pb.OrderingKey]; k != nil {
			for i, x := range k.q {
				if x == e {
					k.q = append(k.q[:i], k.q[i+1:]...)
					break
				}
			}
		}
	}
}

// promote moves delayed items whose time has come into the ready queue.
func (s *Service) promote(sb *sub, now time.Time) {
	moved := false
	for len(sb.delayed) > 0 && !sb.delayed[0].at.After(now) {
		it := heap.Pop(&sb.delayed).(delayedItem)
		if it.e != nil {
			if !it.e.queued || it.e.qgen != it.gen {
				continue
			}
		}
		sb.ready = append(sb.ready, it.readyItem)
		moved = true
	}
	if moved {
		sb.signal()
	}
}

// take leases up to maxN deliverable entries (and maxBytes total, if > 0)
// subject to flow control fl.
func (s *Service) take(sb *sub, maxN int, maxBytes int64, fl *flow, ackDeadline time.Duration, now time.Time) []*entry {
	if sb.deleted || sb.detached() {
		return nil
	}
	s.promote(sb, now)
	var out []*entry
	var bytes int64
	full := func(size int64) bool {
		if len(out) >= maxN || !fl.allows(size) {
			return true
		}
		return maxBytes > 0 && len(out) > 0 && bytes+size > maxBytes
	}
	for len(sb.ready) > 0 {
		it := sb.ready[0]
		if it.e != nil {
			e := it.e
			if !e.queued || e.qgen != it.gen {
				sb.ready = sb.ready[1:]
				continue
			}
			if full(e.m.size) {
				break
			}
			sb.ready = sb.ready[1:]
			e.queued = false
			e.qgen++
			s.lease(sb, e, fl, ackDeadline, now)
			out = append(out, e)
			bytes += e.m.size
			continue
		}
		k := sb.keys[it.key]
		if k == nil || len(k.out) > 0 || len(k.q) == 0 {
			sb.ready = sb.ready[1:]
			if k != nil {
				k.inReady = false
			}
			continue
		}
		if full(k.q[0].m.size) {
			break
		}
		sb.ready = sb.ready[1:]
		k.inReady = false
		for len(k.q) > 0 && !full(k.q[0].m.size) {
			e := k.q[0]
			k.q = k.q[1:]
			e.queued = false
			e.qgen++
			s.lease(sb, e, fl, ackDeadline, now)
			k.out[e] = struct{}{}
			out = append(out, e)
			bytes += e.m.size
		}
	}
	if len(sb.ready) == 0 {
		sb.ready = nil // release the backing array
	}
	return out
}

// lease starts a delivery attempt.
func (s *Service) lease(sb *sub, e *entry, fl *flow, d time.Duration, now time.Time) {
	e.attempts++
	s.leaseN++
	e.ackID = strconv.FormatUint(e.m.seq, 10) + "-" + strconv.FormatUint(s.leaseN, 36)
	e.deadline = now.Add(d)
	e.owner = fl
	if fl != nil {
		fl.outMsgs++
		fl.outBytes += e.m.size
	}
	heap.Push(&sb.leases, e)
}

// endLease clears an entry's lease and returns its flow credit.
func (s *Service) endLease(sb *sub, e *entry) {
	if !e.leased() {
		return
	}
	if e.hidx >= 0 && e.hidx < len(sb.leases) && sb.leases[e.hidx] == e {
		heap.Remove(&sb.leases, e.hidx)
	}
	e.hidx = -1
	e.ackID = ""
	if e.owner != nil {
		e.owner.release(e.m.size)
		e.owner = nil
	}
}

// extend moves a lease deadline.
func (s *Service) extend(sb *sub, e *entry, d time.Duration, now time.Time) {
	e.deadline = now.Add(d)
	if e.hidx >= 0 && e.hidx < len(sb.leases) && sb.leases[e.hidx] == e {
		heap.Fix(&sb.leases, e.hidx)
	}
}

// parseAckID returns the message sequence encoded in an ack ID.
func parseAckID(id string) (uint64, bool) {
	seqStr, _, ok := strings.Cut(id, "-")
	if !ok {
		return 0, false
	}
	seq, err := strconv.ParseUint(seqStr, 10, 64)
	return seq, err == nil
}

// lookupLease finds the entry currently leased under ackID.
func (sb *sub) lookupLease(ackID string) *entry {
	seq, ok := parseAckID(ackID)
	if !ok {
		return nil
	}
	e := sb.entries[seq]
	if e == nil || e.ackID != ackID {
		return nil
	}
	return e
}

// lookupAny finds the (unacked) entry an ack ID refers to, whether or not
// the lease is still current (at-least-once acks are best effort).
func (sb *sub) lookupAny(ackID string) *entry {
	seq, ok := parseAckID(ackID)
	if !ok {
		return nil
	}
	e := sb.entries[seq]
	if e == nil || e.acked {
		return nil
	}
	return e
}

// ackEntry acknowledges an entry and records the store writes in w.
func (s *Service) ackEntry(sb *sub, e *entry, w *writes, now time.Time) {
	wasOrdered := sb.ordered(e)
	s.endLease(sb, e)
	s.dequeue(sb, e)
	e.dlq = false
	if wasOrdered {
		if k := sb.keys[e.m.pb.OrderingKey]; k != nil {
			delete(k.out, e)
			s.keyReady(sb, k, now)
		}
	}
	if sb.cfg.GetRetainAckedMessages() {
		e.acked = true
		w.put(nsBacklog, backlogKey(sb.name, e.m.seq), stateAcked)
		return
	}
	delete(sb.entries, e.m.seq)
	w.del(nsBacklog, backlogKey(sb.name, e.m.seq))
	s.unref(e.m, w)
}

// nack ends a delivery attempt without ack (deadline 0, expiry or push
// failure): the message is redelivered after the retry backoff, or
// forwarded to the dead-letter topic once maxDeliveryAttempts is reached
// (FR-PS-003, FR-PS-004).
func (s *Service) nack(sb *sub, e *entry, now time.Time, push bool) {
	if !e.leased() {
		return
	}
	s.endLease(sb, e)
	dead := s.shouldDeadLetter(sb, e)
	backoff := retryBackoff(sb.cfg, e.attempts, push)
	if sb.ordered(e) {
		k := sb.keys[e.m.pb.OrderingKey]
		if k != nil {
			// Every later outstanding message of the key is redelivered too,
			// after the backoff.
			others := make([]*entry, 0, len(k.out))
			for o := range k.out {
				if o != e {
					s.endLease(sb, o)
					others = append(others, o)
				}
			}
			clear(k.out)
			k.availAt = now.Add(backoff)
			for _, o := range others {
				s.enqueue(sb, o, now)
			}
		}
		if dead {
			e.dlq = true
			s.queueDeadLetter(sb, e)
			if k != nil {
				s.keyReady(sb, k, now)
			}
			return
		}
		s.enqueue(sb, e, now)
		return
	}
	if dead {
		e.dlq = true
		s.queueDeadLetter(sb, e)
		return
	}
	e.availAt = now.Add(backoff)
	s.enqueue(sb, e, now)
}

func (s *Service) shouldDeadLetter(sb *sub, e *entry) bool {
	dl := sb.cfg.GetDeadLetterPolicy()
	if dl == nil || dl.GetDeadLetterTopic() == "" {
		return false
	}
	max := dl.GetMaxDeliveryAttempts()
	if max == 0 {
		max = defaultMaxDeliveryAttempts
	}
	return e.attempts >= max
}

// retryBackoff implements the retry policy's exponential backoff
// (FR-PS-004). Without a policy pull subscriptions redeliver immediately and
// push subscriptions back off from 100ms to 60s.
func retryBackoff(cfg *pubsubpb.Subscription, attempts int32, push bool) time.Duration {
	rp := cfg.GetRetryPolicy()
	var lo, hi time.Duration
	switch {
	case rp != nil:
		lo, hi = defaultMinBackoff, defaultMaxBackoff
		if d := rp.GetMinimumBackoff(); d != nil {
			lo = d.AsDuration()
		}
		if d := rp.GetMaximumBackoff(); d != nil {
			hi = d.AsDuration()
		}
	case push:
		lo, hi = 100*time.Millisecond, 60*time.Second
	default:
		return 0
	}
	d := lo
	for i := int32(1); i < attempts && d < hi; i++ {
		d *= 2
	}
	return min(d, hi)
}

// expire handles lease deadlines that have passed.
func (s *Service) expire(sb *sub, now time.Time) {
	push := sb.cfg.GetPushConfig().GetPushEndpoint() != ""
	for len(sb.leases) > 0 && !sb.leases[0].deadline.After(now) {
		s.nack(sb, sb.leases[0], now, push)
	}
}

// rebuildQueues resets all delivery state of a subscription and requeues
// every unacked entry in publish order (load and seek).
func (s *Service) rebuildQueues(sb *sub, now time.Time) {
	seqs := make([]uint64, 0, len(sb.entries))
	for seq, e := range sb.entries {
		s.endLease(sb, e)
		e.queued = false
		e.qgen++
		e.dlq = false
		e.availAt = time.Time{}
		if !e.acked {
			seqs = append(seqs, seq)
		}
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	sb.ready, sb.delayed, sb.leases = nil, nil, nil
	sb.keys = map[string]*keyQueue{}
	for _, seq := range seqs {
		s.enqueue(sb, sb.entries[seq], now)
	}
	sb.signal()
}

// unref drops one reference to m, deleting it when unreferenced.
func (s *Service) unref(m *message, w *writes) {
	m.refs--
	if m.refs > 0 {
		return
	}
	delete(s.msgs, m.seq)
	w.del(nsMessages, seqKey(m.seq))
}

// receivedMessage renders a leased entry for Pull/StreamingPull.
func receivedMessage(sb *sub, e *entry) *pubsubpb.ReceivedMessage {
	rm := &pubsubpb.ReceivedMessage{AckId: e.ackID, Message: e.m.pb}
	if sb.cfg.GetDeadLetterPolicy().GetDeadLetterTopic() != "" {
		rm.DeliveryAttempt = e.attempts
	}
	return rm
}

func newEntry(m *message) *entry { return &entry{m: m, hidx: -1} }
