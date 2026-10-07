package cdn

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Memory tier: bodies up to memObjectMax are also kept in memory while the
// memory tier stays under memBudget, so hits avoid disk I/O (NFR-PERF-007).
const (
	memObjectMax = 1 << 20
	memBudget    = 64 << 20
	// entryOverhead approximates per-entry metadata for the size limit.
	entryOverhead = 512
)

// entryMeta is the persisted description of one cached response.
type entryMeta struct {
	ID         string      `json:"id"`      // primary + vary values
	Primary    string      `json:"primary"` // backend ID + cache key
	BackendID  string      `json:"backendId"`
	Host       string      `json:"host"`
	Path       string      `json:"path"`
	VaryNames  []string    `json:"varyNames,omitempty"`
	VaryValues string      `json:"varyValues,omitempty"`
	Tags       []string    `json:"tags,omitempty"`
	Status     int         `json:"status"`
	Header     http.Header `json:"header"`
	Size       int64       `json:"size"`
	Stored     time.Time   `json:"stored"`     // when the response was received or revalidated
	InitialAge int64       `json:"initialAge"` // origin Age at Stored
	TTL        int64       `json:"ttl"`
	NoStale    bool        `json:"noStale,omitempty"`
	SWR        int64       `json:"swr,omitempty"`
	ClientCC   string      `json:"clientCC,omitempty"`
	File       string      `json:"-"`
}

type entry struct {
	entryMeta
	body []byte // in-memory copy; nil when only on disk
	elem *list.Element
}

// age returns the current age of the response in seconds.
func (e *entryMeta) age(now time.Time) int64 {
	a := int64(now.Sub(e.Stored)/time.Second) + e.InitialAge
	return max(a, 0)
}

func (e *entry) cost() int64 { return e.Size + entryOverhead }

// store is the size-limited LRU cache index with a memory tier and an
// optional disk tier (FR-CDN-008).
type store struct {
	dir   string // "" = memory only
	limit int64

	mu       sync.Mutex
	entries  map[string]*entry
	variants map[string][]*entry // primary → vary variants
	lru      *list.List          // front = most recent
	used     int64
	memUsed  int64
	seq      uint64
}

func newStore(dir string, limit int64) *store {
	return &store{dir: dir, limit: limit, entries: map[string]*entry{}, variants: map[string][]*entry{}, lru: list.New()}
}

// lookup returns the variant of primary matching r, marking it recently used.
func (s *store) lookup(primary string, r *http.Request) *entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.variants[primary] {
		if e.VaryValues == varyValues(r, e.VaryNames) {
			s.lru.MoveToFront(e.elem)
			return e
		}
	}
	return nil
}

// open returns a reader for e's body.
func (s *store) open(e *entry) (io.ReadSeeker, func(), error) {
	s.mu.Lock()
	body, file := e.body, e.File
	s.mu.Unlock()
	if body != nil || e.Size == 0 {
		return bytes.NewReader(body), func() {}, nil
	}
	f, err := os.Open(filepath.Join(s.dir, file+".body"))
	if err != nil {
		return nil, nil, err
	}
	return f, func() { f.Close() }, nil
}

// fits reports whether an object of size n can be stored at all.
func (s *store) fits(n int64) bool { return n+entryOverhead <= s.limit }

// newBodyFile creates a temp file for spilling a fill to disk.
func (s *store) newBodyFile() (*os.File, error) {
	if s.dir == "" {
		return nil, fmt.Errorf("no disk tier")
	}
	return os.CreateTemp(s.dir, "fill-*.tmp")
}

// put inserts e (replacing any entry with the same ID) with its body given
// either in memory (body) or as a temp file (tmp, owned by put).
func (s *store) put(m entryMeta, body []byte, tmp string) error {
	if !s.fits(m.Size) {
		if tmp != "" {
			os.Remove(tmp)
		}
		return fmt.Errorf("object too large")
	}
	s.mu.Lock()
	s.seq++
	seq := s.seq
	s.mu.Unlock()
	if s.dir != "" { // memory-only stores have no files
		sum := sha256.Sum256([]byte(m.ID))
		m.File = fmt.Sprintf("%s-%d", hex.EncodeToString(sum[:8]), seq)
	}

	e := &entry{entryMeta: m}
	if s.dir != "" {
		if err := s.writeFiles(&m, body, tmp); err != nil {
			if body == nil {
				return err
			}
			m.File = "" // fall back to memory only
		}
		e.entryMeta = m
		if body != nil && m.Size > memObjectMax {
			body = nil // on disk only
		}
	}
	e.body = body

	s.mu.Lock()
	defer s.mu.Unlock()
	if old := s.entries[m.ID]; old != nil {
		s.removeLocked(old)
	}
	if e.body != nil && s.dir != "" && s.memUsed+e.Size > memBudget {
		if e.File != "" {
			e.body = nil
		}
	}
	s.entries[m.ID] = e
	s.variants[m.Primary] = append(s.variants[m.Primary], e)
	e.elem = s.lru.PushFront(e)
	s.used += e.cost()
	if e.body != nil && s.dir != "" {
		s.memUsed += e.Size
	}
	s.evictLocked(e)
	return nil
}

// writeFiles persists the body (from memory or by adopting tmp) and meta.
func (s *store) writeFiles(m *entryMeta, body []byte, tmp string) error {
	bodyPath := filepath.Join(s.dir, m.File+".body")
	if tmp != "" {
		if err := os.Rename(tmp, bodyPath); err != nil {
			os.Remove(tmp)
			return err
		}
	} else if err := os.WriteFile(bodyPath, body, 0o600); err != nil {
		return err
	}
	mb, err := json.Marshal(m)
	if err != nil {
		return err
	}
	// The meta file is the commit point for index rebuilds.
	tmpMeta := filepath.Join(s.dir, m.File+".meta.tmp")
	if err := os.WriteFile(tmpMeta, mb, 0o600); err != nil {
		os.Remove(bodyPath)
		return err
	}
	return os.Rename(tmpMeta, filepath.Join(s.dir, m.File+".meta"))
}

// evictLocked removes least recently used entries (never keep) until the
// cache is within its limit.
func (s *store) evictLocked(keep *entry) {
	for s.used > s.limit {
		el := s.lru.Back()
		if el == nil {
			return
		}
		e := el.Value.(*entry)
		if e == keep {
			if el = el.Prev(); el == nil {
				return
			}
			e = el.Value.(*entry)
		}
		s.removeLocked(e)
	}
}

func (s *store) removeLocked(e *entry) {
	if s.entries[e.ID] != e {
		return
	}
	delete(s.entries, e.ID)
	vs := slices.DeleteFunc(s.variants[e.Primary], func(x *entry) bool { return x == e })
	if len(vs) == 0 {
		delete(s.variants, e.Primary)
	} else {
		s.variants[e.Primary] = vs
	}
	s.lru.Remove(e.elem)
	s.used -= e.cost()
	if e.body != nil && s.dir != "" {
		s.memUsed -= e.Size
	}
	if e.File != "" {
		// Readers holding the file open keep reading after unlink.
		os.Remove(filepath.Join(s.dir, e.File+".meta"))
		os.Remove(filepath.Join(s.dir, e.File+".body"))
	}
}

// remove deletes e if it is still the current entry for its ID.
func (s *store) remove(e *entry) {
	s.mu.Lock()
	s.removeLocked(e)
	s.mu.Unlock()
}

// refresh records a successful revalidation of e (FR-CDN-004).
func (s *store) refresh(e *entry, h http.Header, d decision, now time.Time, initialAge int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e.Header = h
	e.Stored = now
	e.InitialAge = initialAge
	e.TTL = d.ttl
	e.NoStale = d.noStale
	e.SWR = d.swr
	e.ClientCC = d.clientCC
	if e.File != "" && s.entries[e.ID] == e {
		if mb, err := json.Marshal(&e.entryMeta); err == nil {
			tmp := filepath.Join(s.dir, e.File+".meta.tmp")
			if os.WriteFile(tmp, mb, 0o600) == nil {
				os.Rename(tmp, filepath.Join(s.dir, e.File+".meta"))
			}
		}
	}
}

// snapshot returns a copy of e's mutable fields under the lock.
func (s *store) snapshot(e *entry) entryMeta {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := e.entryMeta
	m.Header = e.Header.Clone()
	return m
}

// invalidate removes entries matching the filter and returns the count.
func (s *store) invalidate(match func(*entry) bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.entries {
		if match(e) {
			s.removeLocked(e)
			n++
		}
	}
	return n
}

// purge removes every entry and any stray files (FR-CDN-008).
func (s *store) purge() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.entries)
	s.entries = map[string]*entry{}
	s.variants = map[string][]*entry{}
	s.lru.Init()
	s.used, s.memUsed = 0, 0
	if s.dir != "" {
		des, _ := os.ReadDir(s.dir)
		for _, de := range des {
			os.Remove(filepath.Join(s.dir, de.Name()))
		}
	}
	return n
}

// stats reports the entry count and bytes used.
func (s *store) stats() (entries int, used, mem int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries), s.used, s.memUsed
}

// load rebuilds the index from the disk tier (--data-dir restarts).
// Incomplete or unreadable files are removed.
func (s *store) load() error {
	if s.dir == "" {
		return nil
	}
	des, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	type item struct {
		m   entryMeta
		mod time.Time
	}
	var items []item
	haveMeta := map[string]bool{}
	for _, de := range des {
		name := de.Name()
		file, ok := strings.CutSuffix(name, ".meta")
		if !ok {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.dir, name))
		var m entryMeta
		if err == nil {
			err = json.Unmarshal(b, &m)
		}
		fi, serr := os.Stat(filepath.Join(s.dir, file+".body"))
		if err != nil || serr != nil || fi.Size() != m.Size {
			continue
		}
		m.File = file
		info, _ := de.Info()
		mod := time.Time{}
		if info != nil {
			mod = info.ModTime()
		}
		items = append(items, item{m, mod})
		haveMeta[file] = true
	}
	// Remove stray files (temp fills, orphan bodies, broken metas).
	for _, de := range des {
		name := de.Name()
		base := strings.TrimSuffix(strings.TrimSuffix(name, ".meta"), ".body")
		if !haveMeta[base] {
			os.Remove(filepath.Join(s.dir, name))
		}
	}
	// Oldest first so the newest end up most recently used.
	slices.SortFunc(items, func(a, b item) int { return a.mod.Compare(b.mod) })
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range items {
		if old := s.entries[it.m.ID]; old != nil {
			s.removeLocked(old)
		}
		e := &entry{entryMeta: it.m}
		s.entries[e.ID] = e
		s.variants[e.Primary] = append(s.variants[e.Primary], e)
		e.elem = s.lru.PushFront(e)
		s.used += e.cost()
		var seq uint64
		if _, after, ok := strings.Cut(e.File, "-"); ok {
			fmt.Sscan(after, &seq)
		}
		s.seq = max(s.seq, seq)
	}
	s.evictLocked(nil)
	return nil
}
