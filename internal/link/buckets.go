package link

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"time"
)

// Bucket ownership (ADR 0027): Cloud Storage bucket names are global, so a
// request on a bucket names no Project. Once the front Instance delegates
// a Project to a link, it connects to the Linked Instance, which sends it
// the buckets of the Projects it shares with that link, and again each
// time they change. The front Instance routes a request on a bucket it
// does not have to the link that owns it.

// bucketPoll is how often a Linked Instance looks for changed buckets
// while a front Instance watches them, and bucketRetry the longest a front
// Instance waits to connect again.
var (
	bucketPoll  = 250 * time.Millisecond
	bucketRetry = 10 * time.Second
)

// bucketsReply is a Linked Instance's buckets, with the Projects they are
// in, and Seq, which orders its replies by when it looked at them: a reply
// on the watch stream may arrive after one to a later RefreshBuckets, and
// is then stale. A Linked Instance that predates Seq sends none.
type bucketsReply struct {
	Buckets map[string]string `json:"buckets"`
	Seq     int64             `json:"seq,omitempty"`
}

// sharedBuckets returns the buckets of the Projects shared with the link
// name, with their Projects, numbered past every earlier reply, which it
// looked at before. Seq is the time, so it also passes those this
// Instance sent before a restart.
func (m *Manager) sharedBuckets(name string) bucketsReply {
	m.bucketSeqMu.Lock()
	defer m.bucketSeqMu.Unlock()
	m.bucketSeq = max(time.Now().UnixNano(), m.bucketSeq+1)
	out := bucketsReply{Buckets: map[string]string{}, Seq: m.bucketSeq}
	if m.cfg.Buckets == nil {
		return out
	}
	for b, id := range m.cfg.Buckets() {
		if m.Shared(name, id) {
			out.Buckets[b] = id
		}
	}
	return out
}

// serveBuckets answers a paired Instance with the buckets of the Projects
// shared with it. With watch=1 it streams them, one JSON object per line,
// at once and then whenever they change, until the other Instance hangs
// up.
func (m *Manager) serveBuckets(w http.ResponseWriter, r *http.Request) {
	p, _, ok := m.admit(w, r)
	if !ok {
		return
	}
	if r.URL.Query().Get("watch") != "1" {
		writeJSON(w, http.StatusOK, m.sharedBuckets(p.Name))
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	enc := json.NewEncoder(w)
	tick := time.NewTicker(bucketPoll)
	defer tick.Stop()
	var sent map[string]string
	for {
		if b := m.sharedBuckets(p.Name); sent == nil || !maps.Equal(b.Buckets, sent) {
			if enc.Encode(b) != nil || rc.Flush() != nil {
				return
			}
			sent = b.Buckets
		}
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
		}
	}
}

// BucketOwner returns the link whose Linked Instance has the bucket, and
// its Project there, among those of the Projects it shares with this
// Instance.
func (m *Manager) BucketOwner(bucket string) (link, project string, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, bs := range m.buckets {
		if id, ok := bs[bucket]; ok {
			return name, id, true
		}
	}
	return "", "", false
}

// RefreshBuckets asks the link name for its buckets now, as after a
// request forwarded over it created or deleted one, so that the next
// request on that bucket is routed by the change.
func (m *Manager) RefreshBuckets(ctx context.Context, name string) {
	l, err := m.usable(name)
	if err != nil {
		return
	}
	var reply bucketsReply
	if _, err := m.callPaired(ctx, l, http.MethodGet, "/link/v1/buckets", &reply); err != nil {
		m.cfg.Log.Debug("could not ask a Linked Instance for its buckets", "link", name, "err", err)
		return
	}
	m.setBuckets(name, reply)
}

// setBuckets keeps the buckets the link name has, while it is watched,
// unless the link already sent a later reply.
func (m *Manager) setBuckets(name string, b bucketsReply) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, watched := m.watchers[name]; !watched || (b.Seq != 0 && b.Seq < m.bucketSeqs[name]) {
		return
	}
	m.buckets[name] = b.Buckets
	m.bucketSeqs[name] = b.Seq
}

// StartWatching has this Instance watch the buckets of every link it
// delegates a Project to, from now until Close.
func (m *Manager) StartWatching() {
	m.mu.Lock()
	m.watching = true
	m.mu.Unlock()
	m.watch()
}

// watch starts watching the buckets of each link a Project is delegated
// to, and stops watching, and forgets the buckets of, every other link.
func (m *Manager) watch() {
	m.watchMu.Lock()
	defer m.watchMu.Unlock()
	want := m.DelegatedLinks()
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.watching || m.closed {
		return
	}
	for name, stop := range m.watchers {
		if !slices.Contains(want, name) {
			stop()
			delete(m.watchers, name)
			delete(m.buckets, name)
			delete(m.bucketSeqs, name)
			delete(m.signingKeys, name)
		}
	}
	for _, name := range want {
		if _, ok := m.watchers[name]; !ok {
			ctx, stop := context.WithCancel(context.Background())
			m.watchers[name] = stop
			go m.watchBuckets(ctx, name)
		}
	}
}

// watchBuckets keeps the buckets of the link name up to date until ctx is
// done, connecting again whenever the connection drops. While the Linked
// Instance cannot be reached, the front Instance keeps the buckets it
// last heard of, so that requests on them fail UNAVAILABLE naming the
// link rather than reaching a local bucket.
func (m *Manager) watchBuckets(ctx context.Context, name string) {
	wait := time.Second / 4
	for ctx.Err() == nil {
		if err := m.watchOnce(ctx, name); err != nil && ctx.Err() == nil {
			m.cfg.Log.Debug("watching a Linked Instance's buckets", "link", name, "err", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
		wait = min(2*wait, bucketRetry)
	}
}

// watchOnce reads the buckets the link name streams until the stream ends.
func (m *Manager) watchOnce(ctx context.Context, name string) error {
	l, err := m.usable(name)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+Dialable(l.Address)+"/link/v1/buckets?watch=1", nil)
	if err != nil {
		return err
	}
	req.Header.Set(versionsHeader, m.cfg.Versions.String())
	req.Header.Set(instanceIDHeader, m.cfg.ID)
	resp, err := (&http.Client{Transport: m.transport(l)}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("link %q: %s", name, resp.Status)
	}
	// The Linked Instance may have restarted with another signing key.
	m.refreshSigningKey(ctx, name)
	dec := json.NewDecoder(resp.Body)
	for {
		var msg bucketsReply
		if err := dec.Decode(&msg); err != nil {
			return err
		}
		m.setBuckets(name, msg)
	}
}
