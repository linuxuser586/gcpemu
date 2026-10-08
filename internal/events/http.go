package events

import (
	"fmt"
	"net/http"
	"time"
)

// heartbeat is how often an idle stream sends a comment, so that proxies
// and clients see it is alive.
var heartbeat = 15 * time.Second

// ServeHTTP streams events as text/event-stream. A client resuming with
// the Last-Event-ID header (or lastEventId query parameter) first receives
// what it missed, or a Gap event.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	last := r.Header.Get("Last-Event-ID")
	if last == "" {
		last = r.URL.Query().Get("lastEventId")
	}
	backlog, ch, cancel := h.Subscribe(last)
	defer cancel()

	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// Reconnect quickly after the stream ends (shutdown, slow client).
	if _, err := fmt.Fprint(w, "retry: 1000\n\n"); err != nil {
		return
	}
	for _, ev := range backlog {
		if write(w, ev) != nil {
			return
		}
	}
	if rc.Flush() != nil {
		return
	}

	t := time.NewTicker(heartbeat)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if write(w, ev) != nil {
				return
			}
			// Send whatever else is queued before flushing.
			for drained := false; !drained; {
				select {
				case ev, ok := <-ch:
					if !ok {
						_ = rc.Flush()
						return
					}
					if write(w, ev) != nil {
						return
					}
				default:
					drained = true
				}
			}
		case <-t.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
		}
		if rc.Flush() != nil {
			return
		}
	}
}

func write(w http.ResponseWriter, ev Event) error {
	_, err := fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", ev.ID, ev.Type, ev.Data)
	return err
}
