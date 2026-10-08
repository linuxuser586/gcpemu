package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

type recorder struct{ got []string }

func (r *recorder) Committed(cs []Change) {
	var s []string
	for _, c := range cs {
		if c.Deleted {
			s = append(s, "-"+c.NS+"/"+c.Key)
		} else {
			s = append(s, fmt.Sprintf("+%s/%s=%s", c.NS, c.Key, c.Val))
		}
	}
	r.got = append(r.got, strings.Join(s, " "))
}

func (r *recorder) Replaced() { r.got = append(r.got, "replaced") }

func TestObserve(t *testing.T) {
	for name, inner := range stores(t) {
		t.Run(name, func(t *testing.T) {
			r := &recorder{}
			s := Observe(inner, r)
			_ = s.Update(func(tx Tx) error {
				_ = tx.Put("ns", "a", []byte("1"))
				_ = tx.Put("ns", "b", []byte("1"))
				_ = tx.Put("ns", "a", []byte("2")) // reported once, after b
				return tx.Delete("ns", "missing")  // not reported
			})
			_ = s.Update(func(tx Tx) error {
				_ = tx.Put("ns", "c", []byte("1"))
				return errors.New("rolled back") // not reported
			})
			_ = s.Update(func(tx Tx) error { return tx.Delete("ns", "b") })
			_ = s.Update(func(tx Tx) error { return nil }) // nothing changed
			_ = s.View(func(tx Tx) error { return nil })
			_ = s.Load(map[string]map[string][]byte{})
			_ = s.Reset()
			want := []string{"+ns/b=1 +ns/a=2", "-ns/b", "replaced", "replaced"}
			if strings.Join(r.got, "|") != strings.Join(want, "|") {
				t.Errorf("observed %q, want %q", r.got, want)
			}
		})
	}
}
