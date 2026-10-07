package pubsub

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"

	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

type fakeRouter struct {
	g    *grpc.Server
	rest http.Handler
}

func (r *fakeRouter) Mount(_ string, _ []string, h http.Handler) { r.rest = h }
func (r *fakeRouter) Handle(string, http.Handler)                {}
func (r *fakeRouter) GRPC() *grpc.Server                         { return r.g }
func (r *fakeRouter) Fallback(http.Handler)                      {}

// openService builds a Pub/Sub service over a bbolt store at path, as in
// --data-dir mode, and returns its REST handler.
func openService(t *testing.T, path string) (*Service, http.Handler, func()) {
	t.Helper()
	svc, h, _, closeFn := openServiceClock(t, path)
	return svc, h, closeFn
}

// openServiceClock is openService with an advanceable clock.
func openServiceClock(t *testing.T, path string) (*Service, http.Handler, *clock.Offset, func()) {
	t.Helper()
	clk := clock.NewOffset(clock.Real{})
	st, err := store.OpenBolt(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.IAMMode = config.IAMOff
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	env := &emu.Env{
		Config: &cfg, Store: st, Clock: clk, IDs: emu.NewIDs(false), Log: log,
		DataDir: t.TempDir(), Auth: emu.NewPolicyAuthorizer(config.IAMOff, log), Endpoints: emu.NewEndpoints(),
	}
	svc := New(env).(*Service)
	env.SetServices(map[string]emu.Service{"pubsub": svc})
	r := &fakeRouter{g: grpc.NewServer()}
	if err := svc.Register(r); err != nil {
		t.Fatal(err)
	}
	return svc, r.rest, clk, func() {
		_ = svc.Stop(context.Background())
		_ = st.Close()
	}
}

func do(t *testing.T, h http.Handler, method, path string, body any) map[string]any {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, rd))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body)
	}
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

func pulledData(t *testing.T, resp map[string]any) (data []string, ackIDs []string) {
	t.Helper()
	msgs, _ := resp["receivedMessages"].([]any)
	for _, m := range msgs {
		rm := m.(map[string]any)
		b, _ := base64.StdEncoding.DecodeString(rm["message"].(map[string]any)["data"].(string))
		data = append(data, string(b))
		ackIDs = append(ackIDs, rm["ackId"].(string))
	}
	return data, ackIDs
}

// TestDurability checks NFR-REL-001: published messages and acks survive a
// restart; leased-but-unacked messages become deliverable again.
func TestDurability(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	const v1 = "/v1/projects/dur-proj"
	svc, h, closeFn := openService(t, path)
	_ = svc
	do(t, h, http.MethodPut, v1+"/topics/dur-topic", nil)
	do(t, h, http.MethodPut, v1+"/subscriptions/dur-sub", map[string]any{"topic": "projects/dur-proj/topics/dur-topic"})
	do(t, h, http.MethodPut, v1+"/subscriptions/dur-filtered", map[string]any{"topic": "projects/dur-proj/topics/dur-topic", "filter": `attributes:x`})
	var msgs []map[string]any
	for _, d := range []string{"a", "b", "c"} {
		msgs = append(msgs, map[string]any{"data": base64.StdEncoding.EncodeToString([]byte(d))})
	}
	do(t, h, http.MethodPost, v1+"/topics/dur-topic:publish", map[string]any{"messages": msgs})
	data, ids := pulledData(t, do(t, h, http.MethodPost, v1+"/subscriptions/dur-sub:pull", map[string]any{"maxMessages": 10, "returnImmediately": true}))
	if len(data) != 3 {
		t.Fatalf("pulled %v", data)
	}
	// Ack "a" only; "b" and "c" stay leased when the process dies.
	var ackA string
	for i, d := range data {
		if d == "a" {
			ackA = ids[i]
		}
	}
	do(t, h, http.MethodPost, v1+"/subscriptions/dur-sub:acknowledge", map[string]any{"ackIds": []string{ackA}})
	closeFn()

	_, h, closeFn = openService(t, path)
	defer closeFn()
	data, _ = pulledData(t, do(t, h, http.MethodPost, v1+"/subscriptions/dur-sub:pull", map[string]any{"maxMessages": 10, "returnImmediately": true}))
	if len(data) != 2 || data[0] != "b" || data[1] != "c" {
		t.Fatalf("after restart pulled %v, want [b c]", data)
	}
	if d, _ := pulledData(t, do(t, h, http.MethodPost, v1+"/subscriptions/dur-filtered:pull", map[string]any{"maxMessages": 10, "returnImmediately": true})); len(d) != 0 {
		t.Fatalf("filtered sub got %v", d)
	}
	// New message IDs continue after the persisted sequence.
	pub := do(t, h, http.MethodPost, v1+"/topics/dur-topic:publish", map[string]any{"messages": msgs[:1]})
	if id := pub["messageIds"].([]any)[0]; id != "4" {
		t.Errorf("message id after restart = %v, want 4", id)
	}
}

// TestPublisherInterface covers the emu.Publisher contract used by GCS
// notifications (FR-GCS-007).
func TestPublisherInterface(t *testing.T) {
	svc, h, closeFn := openService(t, filepath.Join(t.TempDir(), "state.db"))
	defer closeFn()
	var p emu.Publisher = svc
	ctx := context.Background()
	if p.TopicExists(ctx, "projects/pub-proj/topics/notify") {
		t.Fatal("topic exists before creation")
	}
	if _, err := p.PublishInternal(ctx, "projects/pub-proj/topics/notify", []byte("x"), nil); err == nil {
		t.Fatal("publish to missing topic succeeded")
	}
	do(t, h, http.MethodPut, "/v1/projects/pub-proj/topics/notify", nil)
	do(t, h, http.MethodPut, "/v1/projects/pub-proj/subscriptions/notify-sub", map[string]any{"topic": "projects/pub-proj/topics/notify"})
	if !p.TopicExists(ctx, "projects/pub-proj/topics/notify") {
		t.Fatal("topic missing")
	}
	id, err := p.PublishInternal(ctx, "projects/pub-proj/topics/notify", []byte(`{"bucket":"b"}`), map[string]string{"eventType": "OBJECT_FINALIZE"})
	if err != nil || id == "" {
		t.Fatalf("PublishInternal = %q, %v", id, err)
	}
	data, _ := pulledData(t, do(t, h, http.MethodPost, "/v1/projects/pub-proj/subscriptions/notify-sub:pull", map[string]any{"maxMessages": 1, "returnImmediately": true}))
	if len(data) != 1 || data[0] != `{"bucket":"b"}` {
		t.Fatalf("pulled %v", data)
	}
}
