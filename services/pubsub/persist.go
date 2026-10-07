package pubsub

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/linuxuser586/gcpemu/internal/store"
)

// Store namespaces. Configs are stored as protojson; messages as binary
// protos; the backlog as one small key per (holder, message).
const (
	nsTopics    = "pubsub/topics"
	nsSubs      = "pubsub/subscriptions"
	nsSnapshots = "pubsub/snapshots"
	nsSchemas   = "pubsub/schemas"
	nsIAM       = "pubsub/iam"
	nsMeta      = "pubsub/meta"
	// nsMessages: %020d seq → topic NUL proto bytes.
	nsMessages = "pubsub/messages"
	// nsBacklog: holder NUL %020d seq → state byte.
	nsBacklog = "pubsub/backlog"
)

var (
	stateUnacked = []byte{'u'}
	stateAcked   = []byte{'a'}
)

func seqKey(seq uint64) string { return fmt.Sprintf("%020d", seq) }

func backlogKey(holder string, seq uint64) string { return holder + "\x00" + seqKey(seq) }

func encodeMessage(m *message) ([]byte, error) {
	b, err := proto.Marshal(m.pb)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(m.topic)+1+len(b))
	out = append(out, m.topic...)
	out = append(out, 0)
	return append(out, b...), nil
}

func decodeMessage(key string, b []byte) (*message, error) {
	seq, err := strconv.ParseUint(key, 10, 64)
	if err != nil {
		return nil, err
	}
	i := strings.IndexByte(string(b), 0)
	if i < 0 {
		return nil, fmt.Errorf("corrupt message %s", key)
	}
	pb := &pubsubpb.PubsubMessage{}
	if err := proto.Unmarshal(b[i+1:], pb); err != nil {
		return nil, err
	}
	return &message{seq: seq, topic: string(b[:i]), pb: pb, size: int64(proto.Size(pb)), publish: pb.GetPublishTime().AsTime()}, nil
}

// writes is a batch of store mutations applied in one transaction.
type writes struct {
	ops []writeOp
}

type writeOp struct {
	ns, key string
	val     []byte // nil = delete
}

func (w *writes) put(ns, key string, val []byte) { w.ops = append(w.ops, writeOp{ns, key, val}) }
func (w *writes) del(ns, key string)             { w.ops = append(w.ops, writeOp{ns: ns, key: key}) }

func (w *writes) putProto(ns, key string, m proto.Message) {
	b, _ := protojson.Marshal(m)
	w.put(ns, key, b)
}

func (w *writes) apply(tx store.Tx) error {
	for _, op := range w.ops {
		var err error
		if op.val == nil {
			err = tx.Delete(op.ns, op.key)
		} else {
			err = tx.Put(op.ns, op.key, op.val)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// commit applies w in one store transaction.
func (s *Service) commit(w *writes) error {
	if len(w.ops) == 0 {
		return nil
	}
	return s.env.Store.Update(w.apply)
}

var unmarshalOpts = protojson.UnmarshalOptions{DiscardUnknown: true}

// snapshotRecord is the stored form of a snapshot.
type snapshotRecord struct {
	Config json.RawMessage `json:"config"`
	Filter string          `json:"filter,omitempty"`
}

func encodeSeq(seq uint64) []byte {
	return binary.BigEndian.AppendUint64(nil, seq)
}

// load rebuilds in-memory state from the store (FR-CORE-031, NFR-REL-001).
// Leases are not persisted: every unacked message becomes deliverable.
func (s *Service) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resetMemory()
	now := time.Now()
	var orphans writes
	err := s.env.Store.View(func(tx store.Tx) error {
		var ferr error
		tx.Scan(nsTopics, "", func(k string, v []byte) bool {
			cfg := &pubsubpb.Topic{}
			if ferr = unmarshalOpts.Unmarshal(v, cfg); ferr != nil {
				return false
			}
			s.topics[k] = &topic{name: k, cfg: cfg, subs: map[string]*sub{}}
			return true
		})
		if ferr != nil {
			return ferr
		}
		tx.Scan(nsSchemas, "", func(k string, v []byte) bool {
			sc := &pubsubpb.Schema{}
			if ferr = unmarshalOpts.Unmarshal(v, sc); ferr != nil {
				return false
			}
			s.addSchemaRevision(sc)
			return true
		})
		if ferr != nil {
			return ferr
		}
		for _, t := range s.topics {
			t.schema = s.topicSchema(t.cfg)
		}
		tx.Scan(nsSubs, "", func(k string, v []byte) bool {
			cfg := &pubsubpb.Subscription{}
			if ferr = unmarshalOpts.Unmarshal(v, cfg); ferr != nil {
				return false
			}
			f, _ := compileFilter(cfg.GetFilter())
			sb := newSub(k, cfg, f)
			s.subs[k] = sb
			if t := s.topics[cfg.GetTopic()]; t != nil && !cfg.GetDetached() {
				t.subs[k] = sb
			}
			return true
		})
		if ferr != nil {
			return ferr
		}
		tx.Scan(nsSnapshots, "", func(k string, v []byte) bool {
			var rec snapshotRecord
			if ferr = json.Unmarshal(v, &rec); ferr != nil {
				return false
			}
			cfg := &pubsubpb.Snapshot{}
			if ferr = unmarshalOpts.Unmarshal(rec.Config, cfg); ferr != nil {
				return false
			}
			f, _ := compileFilter(rec.Filter)
			s.snaps[k] = &snapshot{name: k, cfg: cfg, filter: f, filt: rec.Filter, msgs: map[uint64]*message{}}
			return true
		})
		if ferr != nil {
			return ferr
		}
		if b, ok := tx.Get(nsMeta, "seq"); ok && len(b) == 8 {
			s.seq = binary.BigEndian.Uint64(b)
		}
		clockNow := s.env.Clock.Now()
		tx.Scan(nsMessages, "", func(k string, v []byte) bool {
			m, err := decodeMessage(k, v)
			if err != nil {
				orphans.del(nsMessages, k)
				return true
			}
			s.msgs[m.seq] = m
			s.seq = max(s.seq, m.seq)
			if t := s.topics[m.topic]; t != nil && t.retention() > 0 && clockNow.Sub(m.publish) < t.retention() {
				t.log = append(t.log, m)
				m.refs++
			}
			return true
		})
		tx.Scan(nsBacklog, "", func(k string, v []byte) bool {
			holder, seqStr, ok := strings.Cut(k, "\x00")
			seq, err := strconv.ParseUint(seqStr, 10, 64)
			m := s.msgs[seq]
			if !ok || err != nil || m == nil {
				orphans.del(nsBacklog, k)
				return true
			}
			if sb := s.subs[holder]; sb != nil && !sb.detached() {
				e := newEntry(m)
				e.acked = len(v) > 0 && v[0] == 'a'
				sb.entries[seq] = e
				m.refs++
				return true
			}
			if sn := s.snaps[holder]; sn != nil {
				sn.msgs[seq] = m
				m.refs++
				return true
			}
			orphans.del(nsBacklog, k)
			return true
		})
		return nil
	})
	if err != nil {
		return err
	}
	for seq, m := range s.msgs {
		if m.refs == 0 {
			delete(s.msgs, seq)
			orphans.del(nsMessages, seqKey(seq))
		}
	}
	for _, sb := range s.subs {
		s.rebuildQueues(sb, now)
	}
	return s.commit(&orphans)
}
