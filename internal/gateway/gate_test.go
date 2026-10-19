package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// While the gate is closed (an Instance Reset or State snapshot restore,
// #302), a new REST request is refused as UNAVAILABLE.
func TestClosedGateRefusesHTTP(t *testing.T) {
	g, _ := newTestGateway(t)
	called := false
	h := g.Middleware("svc", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
	}))
	if err := g.CloseGate(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/things", nil))
	if called {
		t.Error("handler ran while the gate was closed")
	}
	assertUnavailableHTTP(t, rec)

	g.OpenGate()
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/things", nil))
	if !called || rec.Code != http.StatusOK {
		t.Errorf("after OpenGate: called=%v status=%d, want the handler to serve 200", called, rec.Code)
	}
}

func assertUnavailableHTTP(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After header")
	}
	var body struct {
		Error struct {
			Code   int    `json:"code"`
			Status string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", rec.Body, err)
	}
	if body.Error.Code != 503 || body.Error.Status != "UNAVAILABLE" {
		t.Errorf("error = %+v, want code 503 status UNAVAILABLE", body.Error)
	}
}

// A request in progress when the gate closes has its context cancelled and
// ends UNAVAILABLE, whatever its handler then writes; CloseGate returns
// only after the handler has.
func TestClosingGateCutsOffHTTP(t *testing.T) {
	g, _ := newTestGateway(t)
	started, returned := make(chan struct{}), make(chan struct{})
	h := g.Middleware("svc", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000") // must not reach the 503
		w.Header().Set("ETag", "v1")
		close(started)
		<-r.Context().Done()
		time.Sleep(20 * time.Millisecond) // CloseGate must wait for this
		apierr.Write(w, apierr.New(codes.Canceled, "%v", r.Context().Err()))
		close(returned)
	}))
	rec := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/things", nil))
		close(served)
	}()
	<-started
	if err := g.CloseGate(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-returned:
	default:
		t.Fatal("CloseGate returned before the in-progress handler")
	}
	<-served
	assertUnavailableHTTP(t, rec)
	for _, h := range []string{"Content-Length", "ETag"} {
		if v := rec.Header().Get(h); v != "" {
			t.Errorf("503 carries the cut-off handler's %s: %q", h, v)
		}
	}
}

// When a handler outlasts CloseGate's ctx, CloseGate returns ctx's error
// and the gate stays closed until OpenGate.
func TestCloseGateBoundedByCtx(t *testing.T) {
	g, _ := newTestGateway(t)
	started, release := make(chan struct{}), make(chan struct{})
	h := g.Middleware("svc", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stuck" {
			close(started)
			<-release
		}
	}))
	go h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/stuck", nil))
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := g.CloseGate(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CloseGate = %v, want %v", err, context.DeadlineExceeded)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/things", nil))
	assertUnavailableHTTP(t, rec)
	close(release)

	g.OpenGate()
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/things", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("after OpenGate: status %d, want 200", rec.Code)
	}
}

// serveGated serves a unary test.Gate/Call and a server-streaming
// test.Gate/Watch through g's interceptors. Both block until their context
// ends, signalling started first, and then fail with CANCELLED.
func serveGated(t *testing.T, g *Gateway, started chan<- struct{}) *grpc.ClientConn {
	t.Helper()
	block := func(ctx context.Context) error {
		started <- struct{}{}
		<-ctx.Done()
		return status.Error(codes.Canceled, ctx.Err().Error())
	}
	g.grpc.RegisterService(&grpc.ServiceDesc{
		ServiceName: "test.Gate",
		HandlerType: (*any)(nil),
		Methods: []grpc.MethodDesc{{MethodName: "Call", Handler: func(_ any, ctx context.Context, dec func(any) error, ic grpc.UnaryServerInterceptor) (any, error) {
			in := new(emptypb.Empty)
			if err := dec(in); err != nil {
				return nil, err
			}
			h := func(ctx context.Context, _ any) (any, error) { return nil, block(ctx) }
			return ic(ctx, in, &grpc.UnaryServerInfo{FullMethod: "/test.Gate/Call"}, h)
		}}},
		Streams: []grpc.StreamDesc{{StreamName: "Watch", ServerStreams: true, Handler: func(_ any, ss grpc.ServerStream) error {
			return block(ss.Context())
		}}},
	}, struct{}{})
	g.grpcOwners["test.Gate"] = "svc"
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.grpc.Serve(lis) }()
	t.Cleanup(g.grpc.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func callGated(ctx context.Context, conn *grpc.ClientConn) error {
	return conn.Invoke(ctx, "/test.Gate/Call", &emptypb.Empty{}, &emptypb.Empty{})
}

func watchGated(ctx context.Context, conn *grpc.ClientConn) error {
	s, err := conn.NewStream(ctx, &grpc.StreamDesc{StreamName: "Watch", ServerStreams: true}, "/test.Gate/Watch")
	if err == nil {
		err = s.SendMsg(&emptypb.Empty{})
	}
	if err == nil {
		err = s.CloseSend()
	}
	if err == nil {
		err = s.RecvMsg(&emptypb.Empty{})
	}
	return err
}

// While the gate is closed, new unary and streaming calls are refused as
// UNAVAILABLE without reaching their handlers.
func TestClosedGateRefusesGRPC(t *testing.T) {
	g, _ := newTestGateway(t)
	started := make(chan struct{}, 2)
	conn := serveGated(t, g, started)
	if err := g.CloseGate(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for name, call := range map[string]func(context.Context, *grpc.ClientConn) error{"unary": callGated, "stream": watchGated} {
		if err := call(ctx, conn); status.Code(err) != codes.Unavailable {
			t.Errorf("%s: %v, want UNAVAILABLE", name, err)
		}
	}
	if len(started) != 0 {
		t.Error("a handler ran while the gate was closed")
	}
}

// Unary and streaming calls in progress when the gate closes end
// UNAVAILABLE, and CloseGate waits for their handlers.
func TestClosingGateCutsOffGRPC(t *testing.T) {
	g, _ := newTestGateway(t)
	started := make(chan struct{}, 2)
	conn := serveGated(t, g, started)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	errs := make(chan error, 2)
	go func() { errs <- callGated(ctx, conn) }()
	go func() { errs <- watchGated(ctx, conn) }()
	<-started
	<-started
	if err := g.CloseGate(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := <-errs; status.Code(err) != codes.Unavailable {
			t.Errorf("call cut off by the gate: %v, want UNAVAILABLE", err)
		}
	}
}
