// Package store is the control-plane state store (FR-CORE-032): a small
// transactional, namespaced key-value interface with an in-memory
// implementation for --ephemeral and a bbolt (pure Go) one for --data-dir.
package store

import (
	"encoding/json"
	"errors"
)

// ErrNotFound is returned by GetJSON when a key is absent.
var ErrNotFound = errors.New("store: not found")

// Store is a transactional namespaced KV store. Keys within a namespace are
// iterated in byte order.
type Store interface {
	View(fn func(Tx) error) error
	Update(fn func(Tx) error) error
	// Reset deletes every namespace.
	Reset() error
	// Dump returns every namespace's contents (for snapshots and admin dumps).
	Dump() (map[string]map[string][]byte, error)
	// Load replaces all contents with data.
	Load(data map[string]map[string][]byte) error
	Close() error
}

// Tx is a store transaction. Writes on a View transaction return an error.
type Tx interface {
	Get(ns, key string) ([]byte, bool)
	Put(ns, key string, val []byte) error
	Delete(ns, key string) error
	// Scan calls fn for every key in ns with the given prefix, in order,
	// until fn returns false.
	Scan(ns, prefix string, fn func(key string, val []byte) bool)
}

// GetJSON unmarshals the value at ns/key into v.
func GetJSON(tx Tx, ns, key string, v any) error {
	b, ok := tx.Get(ns, key)
	if !ok {
		return ErrNotFound
	}
	return json.Unmarshal(b, v)
}

// PutJSON marshals v and stores it at ns/key.
func PutJSON(tx Tx, ns, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return tx.Put(ns, key, b)
}

// Exists reports whether ns/key is present.
func Exists(tx Tx, ns, key string) bool {
	_, ok := tx.Get(ns, key)
	return ok
}

// HasPrefix reports whether any key in ns starts with prefix.
func HasPrefix(tx Tx, ns, prefix string) bool {
	found := false
	tx.Scan(ns, prefix, func(string, []byte) bool { found = true; return false })
	return found
}

// Count returns the number of keys in ns that start with prefix.
func Count(tx Tx, ns, prefix string) int {
	n := 0
	tx.Scan(ns, prefix, func(string, []byte) bool { n++; return true })
	return n
}

// Counts returns, for each resource type in nss, the number of keys in
// its namespace.
func Counts(st Store, nss map[string]string) map[string]int {
	out := make(map[string]int, len(nss))
	_ = st.View(func(tx Tx) error {
		for typ, ns := range nss {
			out[typ] = Count(tx, ns, "")
		}
		return nil
	})
	return out
}

// ListJSON decodes every value under prefix into a slice of T.
func ListJSON[T any](tx Tx, ns, prefix string) ([]T, error) {
	var out []T
	var err error
	tx.Scan(ns, prefix, func(_ string, b []byte) bool {
		var v T
		if err = json.Unmarshal(b, &v); err != nil {
			return false
		}
		out = append(out, v)
		return true
	})
	return out, err
}
