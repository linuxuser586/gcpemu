package store

import "slices"

// Change is one key written or deleted by a committed Update.
type Change struct {
	NS, Key string
	// Val is the value written; nil when Deleted.
	Val     []byte
	Deleted bool
}

// Observer is told what committed Updates changed.
type Observer interface {
	// Committed receives the changes of one Update in the order of their
	// last write; a key written several times appears once. It is called
	// after the Update commits, outside the store's lock, so concurrent
	// Updates may be reported out of order.
	Committed([]Change)
	// Replaced is called after Reset or Load replaced all contents.
	Replaced()
}

// Observe wraps st so that o learns of every committed change.
func Observe(st Store, o Observer) Store { return &observed{Store: st, o: o} }

type observed struct {
	Store
	o Observer
}

type observedTx struct {
	Tx
	changes []Change
}

// last returns changes with only the last write to each key, in order.
func last(changes []Change) []Change {
	seen := map[[2]string]bool{}
	out := make([]Change, 0, len(changes))
	for i := len(changes) - 1; i >= 0; i-- {
		k := [2]string{changes[i].NS, changes[i].Key}
		if !seen[k] {
			seen[k] = true
			out = append(out, changes[i])
		}
	}
	slices.Reverse(out)
	return out
}

func (t *observedTx) Put(ns, key string, val []byte) error {
	if err := t.Tx.Put(ns, key, val); err != nil {
		return err
	}
	t.changes = append(t.changes, Change{NS: ns, Key: key, Val: append([]byte{}, val...)})
	return nil
}

func (t *observedTx) Delete(ns, key string) error {
	_, existed := t.Tx.Get(ns, key)
	if err := t.Tx.Delete(ns, key); err != nil || !existed {
		return err
	}
	t.changes = append(t.changes, Change{NS: ns, Key: key, Deleted: true})
	return nil
}

func (s *observed) Update(fn func(Tx) error) error {
	var ot *observedTx
	err := s.Store.Update(func(tx Tx) error {
		// fn may be retried by the underlying store; start afresh each time.
		ot = &observedTx{Tx: tx}
		return fn(ot)
	})
	if err == nil && ot != nil && len(ot.changes) > 0 {
		s.o.Committed(last(ot.changes))
	}
	return err
}

func (s *observed) Reset() error {
	err := s.Store.Reset()
	s.o.Replaced()
	return err
}

func (s *observed) Load(data map[string]map[string][]byte) error {
	err := s.Store.Load(data)
	s.o.Replaced()
	return err
}
