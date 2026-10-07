package store

import (
	"errors"
	"sort"
	"strings"
	"sync"
)

var errReadOnly = errors.New("store: write in read-only transaction")

// Memory is an in-memory Store. Update transactions are serialised and
// rolled back on error using an undo log.
type Memory struct {
	mu   sync.RWMutex
	data map[string]map[string][]byte
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory { return &Memory{data: map[string]map[string][]byte{}} }

type undo struct {
	ns, key string
	val     []byte
	existed bool
}

type memTx struct {
	m        *Memory
	writable bool
	log      []undo
}

func (t *memTx) Get(ns, key string) ([]byte, bool) {
	v, ok := t.m.data[ns][key]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), v...), true
}

func (t *memTx) record(ns, key string) {
	old, ok := t.m.data[ns][key]
	t.log = append(t.log, undo{ns: ns, key: key, val: old, existed: ok})
}

func (t *memTx) Put(ns, key string, val []byte) error {
	if !t.writable {
		return errReadOnly
	}
	t.record(ns, key)
	b := t.m.data[ns]
	if b == nil {
		b = map[string][]byte{}
		t.m.data[ns] = b
	}
	b[key] = append([]byte(nil), val...)
	return nil
}

func (t *memTx) Delete(ns, key string) error {
	if !t.writable {
		return errReadOnly
	}
	if _, ok := t.m.data[ns][key]; !ok {
		return nil
	}
	t.record(ns, key)
	delete(t.m.data[ns], key)
	return nil
}

func (t *memTx) Scan(ns, prefix string, fn func(string, []byte) bool) {
	b := t.m.data[ns]
	keys := make([]string, 0, len(b))
	for k := range b {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		v, ok := b[k]
		if !ok { // deleted during iteration
			continue
		}
		if !fn(k, append([]byte(nil), v...)) {
			return
		}
	}
}

func (t *memTx) rollback() {
	for i := len(t.log) - 1; i >= 0; i-- {
		u := t.log[i]
		if u.existed {
			b := t.m.data[u.ns]
			if b == nil {
				b = map[string][]byte{}
				t.m.data[u.ns] = b
			}
			b[u.key] = u.val
		} else {
			delete(t.m.data[u.ns], u.key)
		}
	}
}

func (m *Memory) View(fn func(Tx) error) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return fn(&memTx{m: m})
}

func (m *Memory) Update(fn func(Tx) error) (err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tx := &memTx{m: m, writable: true}
	defer func() {
		if r := recover(); r != nil {
			tx.rollback()
			panic(r)
		}
	}()
	if err = fn(tx); err != nil {
		tx.rollback()
	}
	return err
}

func (m *Memory) Reset() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data = map[string]map[string][]byte{}
	return nil
}

func (m *Memory) Dump() (map[string]map[string][]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]map[string][]byte, len(m.data))
	for ns, b := range m.data {
		c := make(map[string][]byte, len(b))
		for k, v := range b {
			c[k] = append([]byte(nil), v...)
		}
		out[ns] = c
	}
	return out, nil
}

func (m *Memory) Load(data map[string]map[string][]byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data = map[string]map[string][]byte{}
	for ns, b := range data {
		c := make(map[string][]byte, len(b))
		for k, v := range b {
			c[k] = append([]byte(nil), v...)
		}
		m.data[ns] = c
	}
	return nil
}

func (m *Memory) Close() error { return nil }
