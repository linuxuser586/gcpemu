package store

import (
	"bytes"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Bolt is a crash-safe file-backed Store built on bbolt.
type Bolt struct {
	db *bolt.DB
}

// OpenBolt opens (creating if needed) a bbolt database at path.
func OpenBolt(path string) (*Bolt, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, err
	}
	return &Bolt{db: db}, nil
}

type boltTx struct{ tx *bolt.Tx }

func (t boltTx) Get(ns, key string) ([]byte, bool) {
	b := t.tx.Bucket([]byte(ns))
	if b == nil {
		return nil, false
	}
	v := b.Get([]byte(key))
	if v == nil {
		return nil, false
	}
	return append([]byte(nil), v...), true
}

func (t boltTx) Put(ns, key string, val []byte) error {
	if !t.tx.Writable() {
		return errReadOnly
	}
	b, err := t.tx.CreateBucketIfNotExists([]byte(ns))
	if err != nil {
		return err
	}
	if val == nil {
		val = []byte{}
	}
	return b.Put([]byte(key), val)
}

func (t boltTx) Delete(ns, key string) error {
	if !t.tx.Writable() {
		return errReadOnly
	}
	b := t.tx.Bucket([]byte(ns))
	if b == nil {
		return nil
	}
	return b.Delete([]byte(key))
}

func (t boltTx) Scan(ns, prefix string, fn func(string, []byte) bool) {
	b := t.tx.Bucket([]byte(ns))
	if b == nil {
		return
	}
	// Collect first so callers may mutate the bucket from fn.
	type kv struct {
		k string
		v []byte
	}
	var items []kv
	p := []byte(prefix)
	c := b.Cursor()
	for k, v := c.Seek(p); k != nil && bytes.HasPrefix(k, p); k, v = c.Next() {
		items = append(items, kv{string(k), append([]byte(nil), v...)})
	}
	for _, it := range items {
		if !fn(it.k, it.v) {
			return
		}
	}
}

func (s *Bolt) View(fn func(Tx) error) error {
	return s.db.View(func(tx *bolt.Tx) error { return fn(boltTx{tx}) })
}

func (s *Bolt) Update(fn func(Tx) error) error {
	return s.db.Update(func(tx *bolt.Tx) error { return fn(boltTx{tx}) })
}

func (s *Bolt) Reset() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		var names [][]byte
		_ = tx.ForEach(func(name []byte, _ *bolt.Bucket) error {
			names = append(names, append([]byte(nil), name...))
			return nil
		})
		for _, n := range names {
			if err := tx.DeleteBucket(n); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Bolt) Dump() (map[string]map[string][]byte, error) {
	out := map[string]map[string][]byte{}
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.ForEach(func(name []byte, b *bolt.Bucket) error {
			m := map[string][]byte{}
			if err := b.ForEach(func(k, v []byte) error {
				m[string(k)] = append([]byte(nil), v...)
				return nil
			}); err != nil {
				return err
			}
			out[string(name)] = m
			return nil
		})
	})
	return out, err
}

func (s *Bolt) Load(data map[string]map[string][]byte) error {
	if err := s.Reset(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		for ns, m := range data {
			b, err := tx.CreateBucketIfNotExists([]byte(ns))
			if err != nil {
				return err
			}
			for k, v := range m {
				if err := b.Put([]byte(k), v); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (s *Bolt) Close() error { return s.db.Close() }
