// Package gateway is the control-plane gateway (FR-CORE-040): one listener
// serving every API over HTTP/1.1 REST and gRPC (h2c), routing by path
// prefix and Host, authenticating callers and recording the request log.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/fault"
	"github.com/linuxuser586/gcpemu/internal/reqlog"
)

// Gateway multiplexes REST and gRPC for all services.
type Gateway struct {
	env  *emu.Env
	log  *reqlog.Log
	mux  *http.ServeMux
	grpc *grpc.Server

	mu         sync.RWMutex
	hosts      map[string]http.Handler // Host → handler (host mode)
	grpcOwners map[string]string       // gRPC service name → emu service
	srv        *http.Server

	// Faults holds fault-injection rules applied to every request (FR-CORE-060).
	Faults fault.Set
}

// New creates a gateway. Call Router for each service, then Serve.
func New(env *emu.Env, log *reqlog.Log) *Gateway {
	g := &Gateway{
		env:        env,
		log:        log,
		mux:        http.NewServeMux(),
		hosts:      map[string]http.Handler{},
		grpcOwners: map[string]string{},
	}
	g.grpc = grpc.NewServer(g.GRPCOptions("")...)
	reflection.Register(g.grpc)
	return g
}

// GRPCOptions returns interceptors that authenticate and log gRPC calls;
// service names the owner for per-service servers ("" = look up by method).
func (g *Gateway) GRPCOptions(service string) []grpc.ServerOption {
	owner := func(full string) string {
		if service != "" {
			return service
		}
		return g.grpcOwner(full)
	}
	return []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (resp any, err error) {
			start := time.Now()
			ctx, ci := emu.WithCallInfo(ctx)
			ctx, err = g.authGRPC(ctx)
			if err == nil {
				err = g.injectGRPC(ctx, owner(info.FullMethod), info.FullMethod)
			}
			if err == nil {
				func() {
					defer func() {
						if r := recover(); r != nil {
							g.env.Log.Error("panic", "method", info.FullMethod, "panic", r, "stack", string(debug.Stack()))
							err = status.Error(codes.Internal, fmt.Sprint(r))
						}
					}()
					resp, err = h(ctx, req)
				}()
			}
			g.logGRPC(ctx, ci, owner(info.FullMethod), info.FullMethod, start, err)
			return resp, toGRPC(err)
		}),
		grpc.ChainStreamInterceptor(func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, h grpc.StreamHandler) error {
			start := time.Now()
			ctx, ci := emu.WithCallInfo(ss.Context())
			ctx, err := g.authGRPC(ctx)
			if err == nil {
				err = g.injectGRPC(ctx, owner(info.FullMethod), info.FullMethod)
			}
			if err == nil {
				err = h(srv, &wrappedStream{ServerStream: ss, ctx: ctx})
			}
			g.logGRPC(ctx, ci, owner(info.FullMethod), info.FullMethod, start, err)
			return toGRPC(err)
		}),
	}
}

// injectGRPC applies a matching fault rule. A dropped connection surfaces
// as UNAVAILABLE since gRPC streams share the HTTP/2 connection.
func (g *Gateway) injectGRPC(ctx context.Context, service, method string) error {
	a := g.Faults.Match(service, method, method)
	if a == nil {
		return nil
	}
	if a.Latency > 0 {
		select {
		case <-time.After(a.Latency):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if a.Drop {
		return status.Error(codes.Unavailable, "connection dropped by injected fault")
	}
	if a.Err != nil {
		return a.Err
	}
	return nil
}

type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedStream) Context() context.Context { return w.ctx }

func toGRPC(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	return apierr.From(err).GRPCStatus().Err()
}

func (g *Gateway) grpcOwner(full string) string {
	svc := strings.TrimPrefix(full, "/")
	if i := strings.IndexByte(svc, '/'); i >= 0 {
		svc = svc[:i]
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.grpcOwners[svc]
}

func (g *Gateway) logGRPC(ctx context.Context, ci *emu.CallInfo, service, method string, start time.Time, err error) {
	code := status.Code(toGRPC(err))
	g.log.Add(reqlog.Entry{
		Time: start, Service: service, Protocol: "grpc", Method: method, Resource: ci.Resource(),
		Principal: string(emu.PrincipalFrom(ctx)), Status: int(code),
		Code: apierr.CodeName(code), LatencyMS: ms(time.Since(start)),
	})
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// authGRPC resolves the caller from the authorization metadata.
func (g *Gateway) authGRPC(ctx context.Context) (context.Context, error) {
	var authz string
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get("authorization"); len(v) > 0 {
			authz = v[0]
		}
	}
	p, err := g.principal(ctx, authz)
	if err != nil {
		return ctx, err
	}
	return emu.WithPrincipal(ctx, p), nil
}

// principal implements FR-CORE-050/051.
func (g *Gateway) principal(ctx context.Context, authz string) (emu.Principal, error) {
	def := emu.Principal(g.env.Config.DefaultPrincipal)
	tok, ok := strings.CutPrefix(authz, "Bearer ")
	if !ok {
		tok, ok = strings.CutPrefix(authz, "bearer ")
	}
	if !ok || tok == "" || tok == "owner" { // "owner" is the Pub/Sub emulator convention
		return def, nil
	}
	if p, ok := g.env.Auth.Authenticate(ctx, tok); ok {
		return p, nil
	}
	if g.env.Auth.Mode() == config.IAMEnforce {
		return "", apierr.Unauthenticated("Request had invalid authentication credentials. Expected OAuth 2 access token, login cookie or other valid authentication credential.").
			WithReason("googleapis.com", "ACCESS_TOKEN_TYPE_UNSUPPORTED")
	}
	return def, nil
}

// Router returns the registration surface for one service.
func (g *Gateway) Router(service string) emu.Router { return &router{g: g, service: service} }

// Register calls s.Register and records which gRPC services it added.
func (g *Gateway) Register(s emu.Service) error {
	before := g.grpc.GetServiceInfo()
	if err := s.Register(g.Router(s.Name())); err != nil {
		return err
	}
	g.mu.Lock()
	for name := range g.grpc.GetServiceInfo() {
		if _, ok := before[name]; !ok {
			g.grpcOwners[name] = s.Name()
		}
	}
	g.mu.Unlock()
	return nil
}

type router struct {
	g       *Gateway
	service string
}

func (r *router) Mount(api string, hosts []string, h http.Handler) {
	wrapped := r.g.Middleware(r.service, h)
	prefix := "/" + api
	r.g.mux.Handle(prefix+"/", http.StripPrefix(prefix, wrapped))
	r.g.mu.Lock()
	for _, host := range hosts {
		r.g.hosts[host] = wrapped
	}
	r.g.mu.Unlock()
}

func (r *router) Handle(pattern string, h http.Handler) {
	r.g.mux.Handle(pattern, r.g.Middleware(r.service, h))
}

func (r *router) GRPC() *grpc.Server { return r.g.grpc }

// Hosts lists every host-routed name (see Gateway.Hosts). Services reach
// it by type-asserting their emu.Router to interface{ Hosts() []string }.
func (r *router) Hosts() []string { return r.g.Hosts() }

func (r *router) Fallback(h http.Handler) {
	r.g.mux.Handle("/", r.g.Middleware(r.service, h))
}

// PathHost is Google's legacy shared API host. Its URLs carry the API in
// the path (www.googleapis.com/storage/v1/..., /oauth2/v3/certs, the
// compute selfLink base), which is exactly the gateway's path routing, so
// it is always served without a host mount.
const PathHost = "www.googleapis.com"

// Hosts returns the sorted real hostnames mounted with host routing
// (e.g. "storage.googleapis.com") plus PathHost; the Google frontend
// (FR-CORE-043, FR-INT-007) serves exactly these names.
func (g *Gateway) Hosts() []string {
	g.mu.RLock()
	out := make([]string, 0, len(g.hosts)+1)
	out = append(out, PathHost)
	for h := range g.hosts {
		out = append(out, h)
	}
	g.mu.RUnlock()
	sort.Strings(out)
	return out
}

// HandleAdmin mounts the admin API (unauthenticated, not request-logged).
func (g *Gateway) HandleAdmin(h http.Handler) { g.mux.Handle("/_emu/", h) }

// ServeHTTP dispatches gRPC, host-mode and path-routed requests.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		g.grpc.ServeHTTP(w, r)
		return
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	g.mu.RLock()
	h, ok := g.hosts[host]
	g.mu.RUnlock()
	if ok {
		h.ServeHTTP(w, r)
		return
	}
	g.mux.ServeHTTP(w, r)
}

// Middleware authenticates, recovers from panics and logs each HTTP request.
func (g *Gateway) Middleware(service string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		ctx, ci := emu.WithCallInfo(r.Context())
		r = r.WithContext(ctx)
		p, err := g.principal(r.Context(), r.Header.Get("Authorization"))
		if err != nil {
			apierr.Write(sw, err)
		} else if g.injectHTTP(sw, r, service) {
			// fault injected; response already written or connection dropped
		} else if f, ferr := formatFor(r); ferr != nil {
			apierr.Write(sw, apierr.InvalidArgument("%v", ferr))
		} else {
			r = r.WithContext(emu.WithPrincipal(r.Context(), p))
			var hw http.ResponseWriter = sw
			var fw *formatWriter
			if f != nil && r.Header.Get("Upgrade") == "" {
				fw = newFormatWriter(sw, f)
				hw = fw
			}
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						if rec == http.ErrAbortHandler {
							panic(rec)
						}
						g.env.Log.Error("panic", "service", service, "path", r.URL.Path, "panic", rec, "stack", string(debug.Stack()))
						if !sw.wrote {
							apierr.Write(sw, apierr.Internal("internal error: %v", rec))
						}
					}
				}()
				h.ServeHTTP(hw, r)
				if fw != nil {
					fw.finish()
				}
			}()
		}
		g.log.Add(reqlog.Entry{
			Time: start, Service: service, Protocol: "http",
			Method: r.Method + " " + r.URL.Path, Resource: ci.Resource(), Principal: string(p),
			Status: sw.status, LatencyMS: ms(time.Since(start)),
		})
	})
}

// injectHTTP applies a matching fault rule and reports whether the request
// was answered by it.
func (g *Gateway) injectHTTP(w *statusWriter, r *http.Request, service string) bool {
	a := g.Faults.Match(service, r.Method+" "+r.URL.Path, r.URL.Path)
	if a == nil {
		return false
	}
	if a.Latency > 0 {
		select {
		case <-time.After(a.Latency):
		case <-r.Context().Done():
			return true
		}
	}
	if a.Drop {
		w.status = 0
		if conn, _, err := http.NewResponseController(w.ResponseWriter).Hijack(); err == nil {
			_ = conn.Close()
			return true
		}
		panic(http.ErrAbortHandler)
	}
	if a.Err != nil {
		apierr.Write(w, a.Err)
		return true
	}
	return false
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) Flush() {
	w.wrote = true
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// NewServer returns an http.Server for h that speaks HTTP/1.1 and h2c.
func NewServer(h http.Handler, log *slog.Logger) *http.Server {
	s := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
	s.Protocols = new(http.Protocols)
	s.Protocols.SetHTTP1(true)
	s.Protocols.SetHTTP2(true)
	s.Protocols.SetUnencryptedHTTP2(true)
	return s
}

// Serve serves on l until Shutdown.
func (g *Gateway) Serve(l net.Listener) error {
	g.mu.Lock()
	g.srv = NewServer(g, g.env.Log)
	srv := g.srv
	g.mu.Unlock()
	err := srv.Serve(l)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown stops the gateway.
func (g *Gateway) Shutdown(ctx context.Context) error {
	g.mu.RLock()
	srv := g.srv
	g.mu.RUnlock()
	g.grpc.Stop()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}
