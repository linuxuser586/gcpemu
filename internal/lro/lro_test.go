package lro

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/certificatemanager/apiv1/certificatemanagerpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// router is a gateway stub; only GRPC is used by Register.
type router struct {
	emu.Router
	g *grpc.Server
}

func (r router) GRPC() *grpc.Server { return r.g }

func newEnv(latency string) *emu.Env {
	cfg := config.Defaults()
	cfg.LROLatency = map[string]string{"svc": latency}
	return &emu.Env{
		Config: &cfg, Store: store.NewMemory(), Clock: clock.Real{}, IDs: emu.NewIDs(true),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func setup(t *testing.T, latency string) (*Manager, *Server) {
	env := newEnv(latency)
	m := NewManager(env, "svc")
	g := grpc.NewServer()
	m.Register(router{g: g})
	t.Cleanup(m.Close)
	return m, m.server
}

func resp(v string) RunFunc {
	return func(context.Context) (proto.Message, error) { return structpb.NewStringValue(v), nil }
}

func TestInstantOperation(t *testing.T) {
	m, srv := setup(t, "")
	op, err := m.Run(context.Background(), "projects/p/locations/l", nil, resp("ok"))
	if err != nil || !op.Done || op.GetResponse() == nil {
		t.Fatalf("op = %v %v", op, err)
	}
	if !strings.HasPrefix(op.Name, "projects/p/locations/l/operations/") || len(op.Name) != len("projects/p/locations/l/operations/")+36 {
		t.Fatalf("name %q", op.Name)
	}
	got, err := srv.GetOperation(context.Background(), &longrunningpb.GetOperationRequest{Name: op.Name})
	if err != nil || !proto.Equal(got, op) {
		t.Fatalf("get = %v %v", got, err)
	}
	failed, _ := m.Run(context.Background(), "projects/p/locations/l", nil, func(context.Context) (proto.Message, error) {
		return nil, apierr.NotFound("gone")
	})
	if failed.GetError().GetCode() != 5 || failed.GetError().GetMessage() != "gone" {
		t.Fatalf("failed op = %v", failed)
	}
	list, err := srv.ListOperations(context.Background(), &longrunningpb.ListOperationsRequest{Name: "projects/p/locations/l", PageSize: 1})
	if err != nil || len(list.Operations) != 1 || list.NextPageToken == "" {
		t.Fatalf("list = %v %v", list, err)
	}
	list, _ = srv.ListOperations(context.Background(), &longrunningpb.ListOperationsRequest{Name: "projects/p/locations/l", PageToken: list.NextPageToken})
	if len(list.Operations) != 1 || list.NextPageToken != "" {
		t.Fatalf("page 2 = %v", list)
	}
	if _, err := srv.DeleteOperation(context.Background(), &longrunningpb.DeleteOperationRequest{Name: op.Name}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.GetOperation(context.Background(), &longrunningpb.GetOperationRequest{Name: op.Name}); apierr.From(err).Code != 5 {
		t.Fatalf("get deleted = %v", err)
	}
}

func TestDelayedOperationAndWait(t *testing.T) {
	m, srv := setup(t, "100ms")
	ran := make(chan struct{})
	op, err := m.Run(context.Background(), "projects/p/locations/l", structpb.NewStringValue("md"), func(context.Context) (proto.Message, error) {
		close(ran)
		return structpb.NewStringValue("done"), nil
	})
	if err != nil || op.Done || op.GetMetadata() == nil {
		t.Fatalf("op = %v %v", op, err)
	}
	select {
	case <-ran:
		t.Fatal("ran before latency")
	default:
	}
	short, _ := srv.WaitOperation(context.Background(), &longrunningpb.WaitOperationRequest{Name: op.Name, Timeout: durationpb.New(time.Millisecond)})
	if short.Done {
		t.Fatal("short wait returned done")
	}
	got, err := srv.WaitOperation(context.Background(), &longrunningpb.WaitOperationRequest{Name: op.Name, Timeout: durationpb.New(5 * time.Second)})
	if err != nil || !got.Done || got.GetResponse() == nil {
		t.Fatalf("wait = %v %v", got, err)
	}
}

func TestCancelAndRecover(t *testing.T) {
	m, srv := setup(t, "1h")
	op, _ := m.Run(context.Background(), "projects/p/locations/l", nil, resp("never"))
	if _, err := srv.CancelOperation(context.Background(), &longrunningpb.CancelOperationRequest{Name: op.Name}); err != nil {
		t.Fatal(err)
	}
	got, _ := srv.GetOperation(context.Background(), &longrunningpb.GetOperationRequest{Name: op.Name})
	if !got.Done || got.GetError().GetCode() != 1 {
		t.Fatalf("cancelled = %v", got)
	}
	// A pending operation left by a previous process is aborted on Register.
	pending, _ := m.Run(context.Background(), "projects/p/locations/l", nil, resp("never"))
	m.Close()
	m2 := NewManager(m.env, "svc")
	m2.Register(router{g: grpc.NewServer()})
	defer m2.Close()
	got, err := m2.Get(pending.Name)
	if err != nil || !got.Done || got.GetError().GetCode() != 10 {
		t.Fatalf("recovered = %v %v", got, err)
	}
}

func TestServeREST(t *testing.T) {
	m, _ := setup(t, "")
	op, _ := m.Run(context.Background(), "projects/p/locations/l", nil, resp("ok"))
	call := func(method, path string) (int, map[string]any, bool) {
		rec := httptest.NewRecorder()
		ok := m.ServeREST(rec, httptest.NewRequest(method, "/v1/"+path, nil), path)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out, ok
	}
	if code, out, ok := call("GET", op.Name); !ok || code != 200 || out["name"] != op.Name || out["done"] != true {
		t.Fatalf("GET = %d %v", code, out)
	}
	if code, out, _ := call("GET", "projects/p/locations/l/operations"); code != 200 || len(out["operations"].([]any)) != 1 {
		t.Fatalf("list = %d %v", code, out)
	}
	if code, _, _ := call("POST", op.Name+":wait"); code != 200 {
		t.Fatalf("wait = %d", code)
	}
	if _, _, ok := call("GET", "projects/p/locations/l/repositories/r"); ok {
		t.Fatal("non-operation path handled")
	}
	if code, _, _ := call("DELETE", op.Name); code != 200 {
		t.Fatalf("delete = %d", code)
	}
	if code, out, _ := call("GET", op.Name); code != 404 || out["error"] == nil {
		t.Fatalf("get deleted = %d %v", code, out)
	}
}

func TestOperationsSummary(t *testing.T) {
	m, _ := setup(t, "1h")
	md := &certificatemanagerpb.OperationMetadata{Target: "projects/p/locations/l/things/a", Verb: "create"}
	pending, err := m.Run(context.Background(), "projects/p/locations/l", md, resp("ok"))
	if err != nil {
		t.Fatal(err)
	}
	other := NewManager(m.env, "other")
	if _, err := other.Run(context.Background(), "projects/q/locations/l", nil, resp("x")); err != nil {
		t.Fatal(err)
	}
	ops := m.Operations()
	if len(ops) != 1 {
		t.Fatalf("ops = %+v, want only svc's", ops)
	}
	got := ops[0]
	if got.Name != pending.Name || got.Project != "p" || got.Location != "l" || got.Done || got.Status != "RUNNING" ||
		got.Type != "create" || got.Target != "projects/p/locations/l/things/a" || got.StartTime.IsZero() || !got.EndTime.IsZero() {
		t.Fatalf("pending = %+v", got)
	}
	m.cancel(pending.Name)
	got = m.Operations()[0]
	if !got.Done || got.Status != "DONE" || got.EndTime.Before(got.StartTime) ||
		got.Error == nil || got.Error.Code != "CANCELLED" || got.Error.Message != "Operation was cancelled." {
		t.Fatalf("cancelled = %+v %+v", got, got.Error)
	}
	var wire map[string]any
	if json.Unmarshal(got.Operation, &wire) != nil || wire["name"] != pending.Name {
		t.Fatalf("operation = %s", got.Operation)
	}
}

func TestResetDropsPendingOperation(t *testing.T) {
	m, _ := setup(t, "20ms")
	ran := make(chan struct{}, 1)
	op, err := m.Run(context.Background(), "projects/p/locations/l", nil, func(context.Context) (proto.Message, error) {
		ran <- struct{}{}
		return structpb.NewStringValue("late"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.env.Store.Reset(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	select {
	case <-ran:
		t.Fatal("RunFunc ran after Reset")
	default:
	}
	if _, err := m.Get(op.Name); apierr.From(err).Code != 5 {
		t.Fatalf("get after reset = %v", err)
	}
	// The manager stays usable.
	next, err := m.Run(context.Background(), "projects/p/locations/l", nil, resp("ok"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.server.WaitOperation(context.Background(), &longrunningpb.WaitOperationRequest{Name: next.Name, Timeout: durationpb.New(5 * time.Second)})
	if err != nil || !got.Done || got.GetResponse() == nil {
		t.Fatalf("operation after reset = %v %v", got, err)
	}
}

func TestResetWaitsForRunningOperation(t *testing.T) {
	m, _ := setup(t, "1ms")
	started, release := make(chan struct{}), make(chan struct{})
	if _, err := m.Run(context.Background(), "projects/p/locations/l", nil, func(context.Context) (proto.Message, error) {
		close(started)
		<-release
		return structpb.NewStringValue("late"), nil
	}); err != nil {
		t.Fatal(err)
	}
	<-started
	// Reset is bounded by its ctx while the work is blocked.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := m.Reset(ctx); err == nil {
		t.Fatal("Reset returned nil while an operation was still running")
	}
	reset := make(chan error, 1)
	go func() { reset <- m.Reset(context.Background()) }()
	select {
	case err := <-reset:
		t.Fatalf("Reset returned %v before the running operation finished", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-reset; err != nil {
		t.Fatal(err)
	}
	// The operation's completion was written before Reset returned, so a
	// Store reset leaves nothing behind.
	if err := m.env.Store.Reset(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if ops := m.Operations(); len(ops) != 0 {
		t.Fatalf("operations after reset = %+v", ops)
	}
}

// hookStore runs after once, after the next Update commits.
type hookStore struct {
	store.Store
	after func()
}

func (h *hookStore) Update(fn func(store.Tx) error) error {
	err := h.Store.Update(fn)
	if f := h.after; f != nil {
		h.after = nil
		f()
	}
	return err
}

func TestResetDuringRunAbortsOperation(t *testing.T) {
	m, _ := setup(t, "20ms")
	hs := &hookStore{Store: m.env.Store}
	m.env.Store = hs
	// A whole Instance Reset lands between the pending record's write and
	// the operation's timer.
	hs.after = func() {
		if err := m.Reset(context.Background()); err != nil {
			t.Error(err)
		}
		if err := hs.Store.Reset(); err != nil {
			t.Error(err)
		}
	}
	ran := make(chan struct{}, 1)
	op, err := m.Run(context.Background(), "projects/p/locations/l", nil, func(context.Context) (proto.Message, error) {
		ran <- struct{}{}
		return structpb.NewStringValue("late"), nil
	})
	if err == nil || apierr.From(err).Code != 10 {
		t.Fatalf("Run across a Reset = %v %v, want Aborted", op, err)
	}
	time.Sleep(100 * time.Millisecond)
	if len(ran) != 0 || len(m.Operations()) != 0 {
		t.Fatalf("after reset: %d ran, operations %+v", len(ran), m.Operations())
	}
}

// An Operation begun by the caller stays pending until the caller
// finishes it, with new metadata; cancelling it asks the caller (#392).
func TestBeginAndFinish(t *testing.T) {
	m, srv := setup(t, "")
	var cancelled string
	m.OnCancel = func(name string) { cancelled = name }
	op, err := m.Begin("projects/p/locations/l", structpb.NewStringValue("queued"))
	if err != nil || op.Done {
		t.Fatalf("begin = %v %v", op, err)
	}
	ctx := context.Background()
	if _, err := srv.CancelOperation(ctx, &longrunningpb.CancelOperationRequest{Name: op.Name}); err != nil || cancelled != op.Name {
		t.Fatalf("cancel = %v, hook saw %q", err, cancelled)
	}
	if got, _ := srv.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: op.Name}); got.Done {
		t.Fatalf("done after the hook alone: %v", got)
	}
	m.Finish(op.Name, structpb.NewStringValue("done"), structpb.NewStringValue("result"), nil)
	got, err := srv.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: op.Name})
	if err != nil || !got.Done || got.GetResponse() == nil {
		t.Fatalf("finished = %v %v", got, err)
	}
	var md structpb.Value
	if err := got.Metadata.UnmarshalTo(&md); err != nil || md.GetStringValue() != "done" {
		t.Errorf("metadata = %v %v, want done", &md, err)
	}
	m.Finish(op.Name, nil, nil, apierr.Aborted("late"))
	if again, _ := srv.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: op.Name}); again.GetError() != nil {
		t.Errorf("a finished Operation changed: %v", again)
	}
}
