package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func stores(t *testing.T) map[string]Store {
	b, err := OpenBolt(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return map[string]Store{"memory": NewMemory(), "bolt": b}
}

func TestStore(t *testing.T) {
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			if err := s.Update(func(tx Tx) error {
				for _, k := range []string{"b/2", "a/1", "b/1", "c"} {
					if err := tx.Put("ns", k, []byte(k)); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			boom := errors.New("boom")
			err := s.Update(func(tx Tx) error {
				_ = tx.Put("ns", "b/3", []byte("x"))
				_ = tx.Delete("ns", "a/1")
				_ = tx.Put("ns", "c", []byte("changed"))
				return boom
			})
			if !errors.Is(err, boom) {
				t.Fatalf("err = %v", err)
			}
			_ = s.View(func(tx Tx) error {
				var keys []string
				tx.Scan("ns", "b/", func(k string, _ []byte) bool { keys = append(keys, k); return true })
				if len(keys) != 2 || keys[0] != "b/1" || keys[1] != "b/2" {
					t.Errorf("scan = %v", keys)
				}
				if v, ok := tx.Get("ns", "c"); !ok || string(v) != "c" {
					t.Errorf("rollback failed: c = %q", v)
				}
				if _, ok := tx.Get("ns", "a/1"); !ok {
					t.Errorf("rollback failed: a/1 missing")
				}
				if err := tx.Put("ns", "z", nil); err == nil {
					t.Errorf("write in view succeeded")
				}
				return nil
			})
			d, _ := s.Dump()
			if len(d["ns"]) != 4 {
				t.Errorf("dump = %v", d)
			}
			_ = s.Reset()
			_ = s.Load(d)
			_ = s.View(func(tx Tx) error {
				if !HasPrefix(tx, "ns", "b/") {
					t.Error("load lost data")
				}
				return nil
			})
		})
	}
}
