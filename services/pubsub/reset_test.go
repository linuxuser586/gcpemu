package pubsub

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// holdStore holds, once armed, the first Update matching hold until
// release is closed: the background work that commits it is then in
// flight. committed is closed once that Update has run.
type holdStore struct {
	store.Store
	hold      func(probeTx) bool
	armed     atomic.Bool
	entered   chan struct{}
	release   chan struct{}
	committed chan struct{}
	// close stops the service and closes the store (startHeld).
	close func()
}

func newHoldStore(st store.Store, hold func(probeTx) bool) *holdStore {
	return &holdStore{Store: st, hold: hold, entered: make(chan struct{}), release: make(chan struct{}), committed: make(chan struct{})}
}

func (h *holdStore) Update(fn func(store.Tx) error) error {
	if h.armed.Load() {
		p := probeTx{}
		_ = fn(&p)
		if h.hold(p) && h.armed.CompareAndSwap(true, false) {
			close(h.entered)
			<-h.release
			defer close(h.committed)
		}
	}
	return h.Store.Update(fn)
}

// probeTx records the writes of an Update without making them.
type probeTx struct{ puts, dels []string }

func (p *probeTx) Get(ns, key string) ([]byte, bool)              { return nil, false }
func (p *probeTx) Scan(string, string, func(string, []byte) bool) {}
func (p *probeTx) Put(ns, key string, val []byte) error {
	p.puts = append(p.puts, ns)
	return nil
}
func (p *probeTx) Delete(ns, key string) error {
	p.dels = append(p.dels, ns)
	return nil
}

// startHeld starts a service over a bbolt store at path wrapped by a
// holdStore, and returns its REST handler.
func startHeld(t *testing.T, path string, hold func(probeTx) bool) (*Service, http.Handler, *holdStore, *clock.Offset) {
	t.Helper()
	bolt, err := store.OpenBolt(path)
	if err != nil {
		t.Fatal(err)
	}
	st := newHoldStore(bolt, hold)
	clk := clock.NewOffset(clock.Real{})
	cfg := config.Defaults()
	cfg.Ports[config.AllPorts] = 0
	cfg.IAMMode = config.IAMOff
	cfg.Bind = "127.0.0.1"
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
	if err := svc.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	st.close = sync.OnceFunc(func() {
		_ = svc.Stop(context.Background())
		_ = bolt.Close()
	})
	t.Cleanup(st.close)
	return svc, r.rest, st, clk
}

// resetAcross runs an Instance Reset of svc and its store and then after,
// with a commit held in flight by st. Reset either returns at once,
// leaving the commit to land after it, or waits for it, which is released
// once Reset is seen waiting. It returns once the held commit has run.
func resetAcross(t *testing.T, svc *Service, st *holdStore, after func()) {
	t.Helper()
	select {
	case <-st.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("no background commit to hold")
	}
	done := make(chan error, 1)
	go func() {
		if err := svc.Reset(context.Background()); err != nil {
			done <- err
			return
		}
		done <- st.Reset()
	}()
	select {
	case err := <-done:
		after()
		close(st.release)
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(200 * time.Millisecond):
		close(st.release) // Reset is waiting for the commit
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		after()
	}
	<-st.committed
}

func deletesBacklog(p probeTx) bool { return slices.Contains(p.dels, nsBacklog) }

func publishData(t *testing.T, h http.Handler, topic string, data ...string) {
	t.Helper()
	var msgs []map[string]any
	for _, d := range data {
		msgs = append(msgs, map[string]any{"data": base64.StdEncoding.EncodeToString([]byte(d))})
	}
	do(t, h, http.MethodPost, "/v1/"+topic+":publish", map[string]any{"messages": msgs})
}

// pullAllAfterRestart restarts the service held by st on its store at
// path and pulls sub.
func pullAllAfterRestart(t *testing.T, st *holdStore, path, sub string) []string {
	t.Helper()
	st.close()
	_, h, closeFn := openService(t, path)
	defer closeFn()
	data, _ := pulledData(t, do(t, h, http.MethodPost, "/v1/"+sub+":pull", map[string]any{"maxMessages": 10, "returnImmediately": true}))
	slices.Sort(data)
	return data
}

const (
	rsTopic = "projects/rs-proj/topics/rs-topic"
	rsSub   = "projects/rs-proj/subscriptions/rs-sub"
)

// recreate creates the topic and a pull subscription again and publishes
// data, as a test does after an Instance Reset.
func recreate(t *testing.T, h http.Handler, data ...string) {
	do(t, h, http.MethodPut, "/v1/"+rsTopic, nil)
	do(t, h, http.MethodPut, "/v1/"+rsSub, map[string]any{"topic": rsTopic})
	publishData(t, h, rsTopic, data...)
}

// A push acknowledgement in flight across a Reset writes nothing
// afterwards: it does not remove a message published after the Reset
// under the same subscription and message ID (#307).
func TestResetWaitsForPushAck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	var st *holdStore
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.armed.Store(true) // hold the acknowledgement's commit
	}))
	defer endpoint.Close()
	svc, h, held, _ := startHeld(t, path, deletesBacklog)
	st = held
	do(t, h, http.MethodPut, "/v1/"+rsTopic, nil)
	do(t, h, http.MethodPut, "/v1/"+rsSub, map[string]any{"topic": rsTopic, "pushConfig": map[string]any{"pushEndpoint": endpoint.URL}})
	publishData(t, h, rsTopic, "old")

	resetAcross(t, svc, st, func() { recreate(t, h, "new") })
	if got := pullAllAfterRestart(t, st, path, rsSub); !slices.Equal(got, []string{"new"}) {
		t.Fatalf("after a restart the subscription holds %v, want [new]", got)
	}
}

// A retention pass in flight across a Reset writes nothing afterwards: it
// does not remove a message published after the Reset under the same
// subscription and message ID (#307).
func TestResetWaitsForRetentionPass(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	svc, h, st, clk := startHeld(t, path, deletesBacklog)
	do(t, h, http.MethodPut, "/v1/"+rsTopic, nil)
	do(t, h, http.MethodPut, "/v1/"+rsSub, map[string]any{"topic": rsTopic})
	publishData(t, h, rsTopic, "old")
	st.armed.Store(true)
	clk.Advance(8 * 24 * time.Hour) // past the default retention

	resetAcross(t, svc, st, func() { recreate(t, h, "new") })
	if got := pullAllAfterRestart(t, st, path, rsSub); !slices.Equal(got, []string{"new"}) {
		t.Fatalf("after a restart the subscription holds %v, want [new]", got)
	}
}

// A dead-letter forward in flight across a Reset writes nothing
// afterwards: the message it read before the Reset does not replace one
// published after it to a dead-letter topic of the same name (#307).
func TestResetWaitsForDeadLetterForward(t *testing.T) {
	const (
		dlq    = "projects/rs-proj/topics/rs-dlq"
		dlqSub = "projects/rs-proj/subscriptions/dlq-sub"
	)
	path := filepath.Join(t.TempDir(), "state.db")
	svc, h, st, _ := startHeld(t, path, func(p probeTx) bool { return slices.Contains(p.puts, nsMessages) })
	do(t, h, http.MethodPut, "/v1/"+rsTopic, nil)
	do(t, h, http.MethodPut, "/v1/"+dlq, nil)
	do(t, h, http.MethodPut, "/v1/"+dlqSub, map[string]any{"topic": dlq})
	do(t, h, http.MethodPut, "/v1/"+rsSub, map[string]any{"topic": rsTopic,
		"deadLetterPolicy": map[string]any{"deadLetterTopic": dlq, "maxDeliveryAttempts": 5}})
	publishData(t, h, rsTopic, "old")
	for attempt := 1; attempt <= 5; attempt++ {
		var ids []string
		deadline := time.Now().Add(10 * time.Second)
		for len(ids) == 0 {
			if time.Now().After(deadline) {
				t.Fatalf("attempt %d: nothing pulled", attempt)
			}
			_, ids = pulledData(t, do(t, h, http.MethodPost, "/v1/"+rsSub+":pull", map[string]any{"maxMessages": 1, "returnImmediately": true}))
		}
		if attempt == 5 {
			st.armed.Store(true) // hold the forward's publish
		}
		do(t, h, http.MethodPost, "/v1/"+rsSub+":modifyAckDeadline", map[string]any{"ackIds": ids, "ackDeadlineSeconds": 0})
	}

	resetAcross(t, svc, st, func() {
		do(t, h, http.MethodPut, "/v1/"+dlq, nil)
		do(t, h, http.MethodPut, "/v1/"+dlqSub, map[string]any{"topic": dlq})
		publishData(t, h, dlq, "new1", "new2")
	})
	if got := pullAllAfterRestart(t, st, path, dlqSub); !slices.Equal(got, []string{"new1", "new2"}) {
		t.Fatalf("after a restart the dead-letter subscription holds %v, want [new1 new2]", got)
	}
}

// After a Reset background delivery runs again: a push subscription
// created afterwards delivers and its acknowledgement commits (#307).
func TestPushRunsAfterReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	got := make(chan struct{}, 1)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case got <- struct{}{}:
		default:
		}
	}))
	defer endpoint.Close()
	svc, h, st, _ := startHeld(t, path, func(probeTx) bool { return false })
	if err := svc.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	do(t, h, http.MethodPut, "/v1/"+rsTopic, nil)
	do(t, h, http.MethodPut, "/v1/"+rsSub, map[string]any{"topic": rsTopic, "pushConfig": map[string]any{"pushEndpoint": endpoint.URL}})
	publishData(t, h, rsTopic, "after")
	select {
	case <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("a push subscription created after Reset delivered nothing")
	}
	time.Sleep(500 * time.Millisecond) // the acknowledgement commits
	if left := pullAllAfterRestart(t, st, path, rsSub); len(left) != 0 {
		t.Fatalf("after a restart the subscription still holds %v", left)
	}
}
