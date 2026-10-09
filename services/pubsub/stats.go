package pubsub

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"
)

// Subscription statistics for the Web console's Pub/Sub view (SRS 4.8.3).
// GCP reports them as Cloud Monitoring metrics
// (subscription/num_undelivered_messages, oldest_unacked_message_age,
// dead_letter_message_count), which the emulator does not serve, so the
// admin API reports them instead:
//
//	GET /_emu/v1/pubsub/subscriptions[?project=P]

// subStats is one subscription's statistics.
type subStats struct {
	Name  string `json:"name"`
	Topic string `json:"topic"`
	// Backlog counts unacked messages, leased or not.
	Backlog      int   `json:"backlog"`
	BacklogBytes int64 `json:"backlogBytes"`
	// Outstanding counts messages leased to a subscriber now.
	Outstanding int `json:"outstanding"`
	// OldestUnackedPublishTime is the publish time of the oldest unacked
	// message; absent when the backlog is empty.
	OldestUnackedPublishTime time.Time `json:"oldestUnackedPublishTime,omitzero"`
	// DeadLettered counts messages forwarded to the dead-letter topic
	// since the Instance started.
	DeadLettered int64 `json:"deadLettered"`
}

// stats reports the subscriptions of project ("" for all), by name.
func (s *Service) stats(project string) []subStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []subStats{}
	for name, sb := range s.subs {
		if project != "" && projectOf(name) != project {
			continue
		}
		st := subStats{Name: name, Topic: sb.cfg.GetTopic(), DeadLettered: sb.deadLettered}
		for _, e := range sb.entries {
			if e.acked {
				continue
			}
			st.Backlog++
			st.BacklogBytes += e.m.size
			if e.leased() {
				st.Outstanding++
			}
			if st.OldestUnackedPublishTime.IsZero() || e.m.publish.Before(st.OldestUnackedPublishTime) {
				st.OldestUnackedPublishTime = e.m.publish
			}
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// serveStats is GET /_emu/v1/pubsub/subscriptions. now is the Emulator
// clock, which publish times follow: ages are measured against it, and
// seeking to it purges every message published so far.
func (s *Service) serveStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"now":           s.env.Clock.Now(),
		"subscriptions": s.stats(r.URL.Query().Get("project")),
	})
}
