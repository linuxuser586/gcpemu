// Package events is the Instance's change feed, served as server-sent
// events at /_emu/v1/events (FR-CORE-045, FR-UI-006): resource changes,
// Operation progress and Request log entries.
//
// A Hub numbers every event and keeps the most recent ones so that a
// client reconnecting with Last-Event-ID receives what it missed. When it
// cannot (the events are gone, or the ID is from another process), the
// client is told it has a gap and must refetch everything.
package events

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Event types.
const (
	// Resource is a store key written or deleted (ResourceChange).
	Resource = "resource"
	// Operation is an Operation created, progressed or deleted (OperationChange).
	Operation = "operation"
	// Request is a Request log entry (reqlog.Entry).
	Request = "request"
	// Reset says all state was replaced (Reset or State snapshot restore).
	Reset = "reset"
	// Gap says events were missed and cannot be replayed. Only Subscribe
	// produces it.
	Gap = "gap"
)

// Event is one numbered event.
type Event struct {
	// ID is "<epoch>-<seq>": epoch identifies the Hub, so that IDs from an
	// earlier process are recognised as a gap.
	ID   string
	Type string
	Data json.RawMessage
}

// subBuffer is how many events a subscriber may lag before it is dropped.
const subBuffer = 512

// Hub fans events out to subscribers and retains the latest ones.
type Hub struct {
	epoch string

	mu     sync.Mutex
	seq    uint64 // last assigned
	ring   []Event
	start  int // index of the oldest retained event in ring
	n      int // retained events
	subs   map[chan Event]struct{}
	closed bool
}

// NewHub returns a Hub retaining the last size events.
func NewHub(size int) *Hub {
	return &Hub{
		epoch: strconv.FormatInt(time.Now().UnixNano(), 36),
		ring:  make([]Event, size),
		subs:  map[chan Event]struct{}{},
	}
}

// Publish sends an event of type typ with data marshalled as JSON. A nil
// Hub discards it.
func (h *Hub) Publish(typ string, data any) {
	if h == nil {
		return
	}
	b, err := json.Marshal(data)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.seq++
	ev := Event{ID: h.epoch + "-" + strconv.FormatUint(h.seq, 10), Type: typ, Data: b}
	if h.n < len(h.ring) {
		h.ring[(h.start+h.n)%len(h.ring)] = ev
		h.n++
	} else {
		h.ring[h.start] = ev
		h.start = (h.start + 1) % len(h.ring)
	}
	for ch := range h.subs {
		select {
		case ch <- ev:
		default:
			// Too slow: drop it. Its client reconnects with
			// Last-Event-ID and is replayed from the ring, or told of a gap.
			delete(h.subs, ch)
			close(ch)
		}
	}
}

// Subscribe returns the events after lastID (empty for none) and a channel
// of later ones. If the events after lastID are not all retained, backlog
// is a single Gap event. The channel is closed when the subscriber falls
// too far behind or the Hub closes; cancel releases it.
func (h *Hub) Subscribe(lastID string) (backlog []Event, ch <-chan Event, cancel func()) {
	c := make(chan Event, subBuffer)
	h.mu.Lock()
	defer h.mu.Unlock()
	if lastID != "" {
		backlog = h.since(lastID)
	}
	if h.closed {
		close(c)
		return backlog, c, func() {}
	}
	h.subs[c] = struct{}{}
	return backlog, c, func() {
		h.mu.Lock()
		if _, ok := h.subs[c]; ok {
			delete(h.subs, c)
			close(c)
		}
		h.mu.Unlock()
	}
}

// since returns the retained events after lastID, or a Gap.
func (h *Hub) since(lastID string) []Event {
	gap := []Event{{ID: h.lastID(), Type: Gap, Data: json.RawMessage("{}")}}
	epoch, s, ok := strings.Cut(lastID, "-")
	seq, err := strconv.ParseUint(s, 10, 64)
	if !ok || err != nil || epoch != h.epoch || seq > h.seq {
		return gap
	}
	oldest := h.seq - uint64(h.n) + 1 // seq of ring[start]
	if seq+1 < oldest {
		return gap
	}
	var out []Event
	for i := seq + 1 - oldest; i < uint64(h.n); i++ {
		out = append(out, h.ring[(h.start+int(i))%len(h.ring)])
	}
	return out
}

func (h *Hub) lastID() string { return fmt.Sprintf("%s-%d", h.epoch, h.seq) }

// Close ends every subscription; later events are discarded.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for ch := range h.subs {
		delete(h.subs, ch)
		close(ch)
	}
}
