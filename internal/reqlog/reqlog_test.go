package reqlog

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNewBody(t *testing.T) {
	if b := NewBody("text/plain", []byte("hi"), 2); b.Text != "hi" || b.Truncated || b.Binary || b.Size != 2 {
		t.Errorf("short body = %+v", b)
	}
	// A cut through "é" drops its first byte rather than marking the
	// body binary.
	head := []byte(strings.Repeat("a", MaxBody-1) + "é")
	b := NewBody("", head, int64(len(head)))
	if !b.Truncated || b.Binary || len(b.Text) != MaxBody-1 || b.Size != MaxBody+1 {
		t.Errorf("cut body: truncated=%v binary=%v len=%d size=%d", b.Truncated, b.Binary, len(b.Text), b.Size)
	}
	if b := NewBody("application/octet-stream", []byte{0xff, 0xfe, 0}, 3); !b.Binary || b.Text != "" {
		t.Errorf("binary body = %+v", b)
	}
}

func TestCapture(t *testing.T) {
	var c Capture
	if c.Body("") != nil {
		t.Error("empty capture has a body")
	}
	for range 3 {
		_, _ = c.Write([]byte(strings.Repeat("x", MaxBody/2)))
	}
	b := c.Body("text/plain")
	if b.Size != 3*MaxBody/2 || len(b.Text) != MaxBody || !b.Truncated || b.ContentType != "text/plain" {
		t.Errorf("body: size=%d len=%d truncated=%v", b.Size, len(b.Text), b.Truncated)
	}
}

// TestBodiesOnlyFromGet: entries are numbered, listed and sent to OnAdd
// without bodies, and Get returns them with their bodies.
func TestBodiesOnlyFromGet(t *testing.T) {
	l := New(2, nil)
	var added []Entry
	l.OnAdd(func(e Entry) { added = append(added, e) })
	for _, s := range []string{"a", "b", "c"} {
		l.Add(Entry{Service: s, Bodies: &Bodies{Request: NewBody("", []byte(s), 1)}})
	}
	es := l.Entries("")
	if len(es) != 2 || es[0].ID != 2 || es[1].ID != 3 || es[0].Bodies != nil {
		t.Fatalf("entries = %+v", es)
	}
	if len(added) != 3 || added[2].ID != 3 || added[2].Bodies != nil {
		t.Errorf("OnAdd got %+v", added)
	}
	if _, ok := l.Get(1); ok {
		t.Error("Get returned an entry no longer retained")
	}
	d, ok := l.Get(3)
	if !ok || d.Service != "c" || d.Request == nil || d.Request.Text != "c" {
		t.Fatalf("Get(3) = %+v, %v", d, ok)
	}
	j, _ := json.Marshal(d)
	if !strings.Contains(string(j), `"id":3`) || !strings.Contains(string(j), `"requestBody":{"size":1,"text":"c"}`) {
		t.Errorf("detail JSON = %s", j)
	}
	l.Clear()
	l.Add(Entry{Service: "d"})
	if es := l.Entries(""); len(es) != 1 || es[0].ID != 4 {
		t.Errorf("after Clear: %+v", es)
	}
}
