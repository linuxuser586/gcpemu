package events

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/store"
)

func types(evs []Event) string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return strings.Join(out, ",")
}

func TestReplayAndGap(t *testing.T) {
	h := NewHub(3)
	_, _, cancel := h.Subscribe("")
	cancel()
	for _, typ := range []string{"a", "b", "c", "d"} {
		h.Publish(typ, struct{}{})
	}
	// Retained: b, c, d (seq 2..4).
	id := func(seq string) string { return h.epoch + "-" + seq }
	for _, tc := range []struct{ last, want string }{
		{"", ""},
		{id("4"), ""},
		{id("3"), "d"},
		{id("1"), "b,c,d"},
		{id("0"), Gap},   // a is gone
		{id("9"), Gap},   // from the future
		{"other-2", Gap}, // another process
		{"garbage", Gap}, // not an ID
	} {
		backlog, _, cancel := h.Subscribe(tc.last)
		cancel()
		if got := types(backlog); got != tc.want {
			t.Errorf("Subscribe(%q) = %s, want %s", tc.last, got, tc.want)
		}
		if tc.want == Gap && backlog[0].ID != id("4") {
			t.Errorf("gap ID = %s, want the latest", backlog[0].ID)
		}
	}
}

func TestSlowSubscriberDropped(t *testing.T) {
	h := NewHub(10)
	_, ch, cancel := h.Subscribe("")
	defer cancel()
	for range subBuffer + 1 {
		h.Publish("x", 1)
	}
	n := 0
	for range ch {
		n++
	}
	if n != subBuffer {
		t.Errorf("received %d before close, want %d", n, subBuffer)
	}
}

func TestClose(t *testing.T) {
	h := NewHub(10)
	_, ch, cancel := h.Subscribe("")
	defer cancel()
	h.Close()
	if _, ok := <-ch; ok {
		t.Fatal("channel open after Close")
	}
	h.Publish("x", 1) // discarded, no panic
	if _, ch, _ := h.Subscribe(""); ch != nil {
		if _, ok := <-ch; ok {
			t.Fatal("subscription after Close is open")
		}
	}
	var nilHub *Hub
	nilHub.Publish("x", 1)
}

func TestStoreObserver(t *testing.T) {
	h := NewHub(100)
	st := store.Observe(store.NewMemory(), StoreObserver(h))
	_, ch, cancel := h.Subscribe("")
	defer cancel()
	next := func() (string, map[string]any) {
		select {
		case ev := <-ch:
			var m map[string]any
			_ = json.Unmarshal(ev.Data, &m)
			return ev.Type, m
		case <-time.After(time.Second):
			t.Fatal("no event")
			return "", nil
		}
	}
	_ = st.Update(func(tx store.Tx) error {
		_ = tx.Put("gcs/buckets", "b", []byte(`{}`))
		_ = tx.Put("lro/operations", "x", []byte(`{"service":"pubsub","op":{"name":"projects/p/operations/1","done":true}}`))
		_ = tx.Put("compute/operations", "projects/p/global/operations/o", []byte(`{"status":"RUNNING"}`))
		return tx.Put("dns/changes", "p/z/1", []byte(`{"status":"done"}`))
	})
	want := []struct {
		typ  string
		data string
	}{
		{Resource, `{"service":"gcs","namespace":"gcs/buckets","key":"b"}`},
		{Operation, `{"service":"pubsub","namespace":"lro/operations","name":"projects/p/operations/1","done":true}`},
		{Operation, `{"service":"compute","namespace":"compute/operations","name":"projects/p/global/operations/o","done":false}`},
		{Operation, `{"service":"dns","namespace":"dns/changes","name":"p/z/1","done":true}`},
	}
	for _, w := range want {
		typ, m := next()
		if b1, _ := json.Marshal(m); typ != w.typ || string(b1) != mustSort(t, w.data) {
			t.Errorf("event %s %s, want %s %s", typ, b1, w.typ, mustSort(t, w.data))
		}
	}
	_ = st.Update(func(tx store.Tx) error { return tx.Delete("gcs/buckets", "b") })
	if typ, m := next(); typ != Resource || m["deleted"] != true {
		t.Errorf("delete: %s %v", typ, m)
	}
	_ = st.Reset()
	if typ, _ := next(); typ != Reset {
		t.Errorf("reset: %s", typ)
	}
}

func mustSort(t *testing.T, s string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func TestServeHTTP(t *testing.T) {
	h := NewHub(100)
	srv := httptest.NewServer(h)
	defer srv.Close()
	h.Publish(Resource, ResourceChange{Service: "gcs", Namespace: "gcs/buckets", Key: "old"})
	first := h.lastID()

	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Last-Event-ID", first)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	h.Publish(Resource, ResourceChange{Service: "gcs", Namespace: "gcs/buckets", Key: "new"})

	r := bufio.NewReader(resp.Body)
	var frame []string
	deadline := time.AfterFunc(2*time.Second, func() { resp.Body.Close() })
	defer deadline.Stop()
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v (so far %q)", err, frame)
		}
		line = strings.TrimSuffix(line, "\n")
		if line == "" && len(frame) > 0 && strings.HasPrefix(frame[len(frame)-1], "data:") {
			break
		}
		if line != "" {
			frame = append(frame, line)
		}
	}
	want := []string{
		"retry: 1000",
		"id: " + h.lastID(),
		"event: resource",
		`data: {"service":"gcs","namespace":"gcs/buckets","key":"new"}`,
	}
	if strings.Join(frame, "\n") != strings.Join(want, "\n") {
		t.Errorf("stream:\n%s\nwant:\n%s", strings.Join(frame, "\n"), strings.Join(want, "\n"))
	}
	h.Close()
	if rest, err := io.ReadAll(r); err != nil || len(rest) > 0 {
		t.Errorf("after Close: %q, %v; want EOF", rest, err)
	}
}
