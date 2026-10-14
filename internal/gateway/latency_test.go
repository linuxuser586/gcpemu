package gateway

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/reqlog"
)

// A fast call's Request log latency is above zero on every platform:
// Windows' monotonic clock advances only every ~0.5ms, so these calls are
// timed with a clock.Stopwatch (#259). The handlers spin for a few µs,
// well under one Windows tick, so the call is never too fast for the
// microsecond resolution of LatencyMS.

func spin() {
	sw := clock.StartStopwatch()
	for sw.Elapsed() < 20*time.Microsecond {
	}
}

func newTestGateway(t *testing.T) (*Gateway, *reqlog.Log) {
	t.Helper()
	log := reqlog.New(16, slog.New(slog.DiscardHandler))
	env := &emu.Env{Config: &config.Config{}, Log: slog.New(slog.DiscardHandler)}
	return New(env, log), log
}

func onlyEntry(t *testing.T, log *reqlog.Log, service string) reqlog.Entry {
	t.Helper()
	es := log.Entries(service)
	if len(es) != 1 {
		t.Fatalf("request log for %s: %+v, want one entry", service, es)
	}
	return es[0]
}

func TestHTTPLatency(t *testing.T) {
	g, log := newTestGateway(t)
	h := g.Middleware("svc", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		spin()
		w.WriteHeader(http.StatusNoContent)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/things", nil))
	if e := onlyEntry(t, log, "svc"); e.LatencyMS <= 0 {
		t.Errorf("LatencyMS = %v, want > 0", e.LatencyMS)
	}
}

func TestGRPCLatency(t *testing.T) {
	g, log := newTestGateway(t)
	const method = "/test.Slow/Wait"
	g.grpc.RegisterService(&grpc.ServiceDesc{
		ServiceName: "test.Slow",
		HandlerType: (*any)(nil),
		Methods: []grpc.MethodDesc{{MethodName: "Wait", Handler: func(_ any, ctx context.Context, dec func(any) error, ic grpc.UnaryServerInterceptor) (any, error) {
			in := new(emptypb.Empty)
			if err := dec(in); err != nil {
				return nil, err
			}
			h := func(context.Context, any) (any, error) { spin(); return &emptypb.Empty{}, nil }
			return ic(ctx, in, &grpc.UnaryServerInfo{FullMethod: method}, h)
		}}},
	}, struct{}{})
	g.grpcOwners["test.Slow"] = "svc"

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
	defer conn.Close()
	if err := conn.Invoke(context.Background(), method, &emptypb.Empty{}, &emptypb.Empty{}); err != nil {
		t.Fatal(err)
	}
	if e := onlyEntry(t, log, "svc"); e.LatencyMS <= 0 {
		t.Errorf("LatencyMS = %v, want > 0", e.LatencyMS)
	}
}
