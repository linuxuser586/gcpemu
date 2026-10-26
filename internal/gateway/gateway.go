// Package gateway is the control-plane gateway (FR-CORE-040): one listener
// serving every API over HTTP/1.1 REST and gRPC (h2c), routing by path
// prefix and Host, authenticating callers and recording the request log.
package gateway

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/linuxuser586/gcpemu/internal/apidef"
	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/clock"
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
	apiOwners  map[string]string       // mounted API name or host → emu service
	srv        *http.Server
	adminToken string   // see SetAdminToken
	gate       gate     // see CloseGate
	front      *handoff // see ServeFrontend
	// delegated are the Delegated APIs, delegatedHosts them by hostname
	// (see Delegate).
	delegated      []*delegatedAPI
	delegatedHosts map[string]*delegatedAPI
	// links forward Delegated Projects (see SetLinks).
	links Links
	// fallback is the Service that serves paths no other route matches.
	fallback string

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
		apiOwners:  map[string]string{},
		front:      newHandoff(),

		delegatedHosts: map[string]*delegatedAPI{},
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
			timer := clock.StartStopwatch()
			clock.RequestStep(g.env.Clock)
			ctx, leave, err := g.gate.enter(ctx)
			if err == nil {
				defer leave()
			}
			ctx, ci := emu.WithCallInfo(ctx)
			ci.SetCall(owner(info.FullMethod), info.FullMethod)
			if err == nil {
				ctx, err = g.authGRPC(ctx)
			}
			if err == nil {
				err = g.injectGRPC(ctx, owner(info.FullMethod), info.FullMethod)
			}
			if m, ok := req.(proto.Message); ok && err == nil {
				// Field behaviours (NFR-MNT-001): required, output-only and
				// immutable fields, from the generated API definitions.
				err = apidef.CheckProto(info.FullMethod, m)
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
			if cutOff(ctx) {
				resp, err = nil, errUnavailable
			}
			g.logGRPC(ctx, ci, owner(info.FullMethod), info.FullMethod, start, timer, err,
				&reqlog.Bodies{Request: grpcBody(req), Response: grpcBody(resp)})
			return resp, toGRPC(err)
		}),
		grpc.ChainStreamInterceptor(func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, h grpc.StreamHandler) error {
			start := time.Now()
			timer := clock.StartStopwatch()
			clock.RequestStep(g.env.Clock) // once, when the stream opens
			ctx, leave, err := g.gate.enter(ss.Context())
			if err == nil {
				defer leave()
			}
			ctx, ci := emu.WithCallInfo(ctx)
			ci.SetCall(owner(info.FullMethod), info.FullMethod)
			if err == nil {
				ctx, err = g.authGRPC(ctx)
			}
			if err == nil {
				err = g.injectGRPC(ctx, owner(info.FullMethod), info.FullMethod)
			}
			if err == nil {
				err = h(srv, &wrappedStream{ServerStream: ss, ctx: ctx})
			}
			if cutOff(ctx) {
				err = errUnavailable
			}
			g.logGRPC(ctx, ci, owner(info.FullMethod), info.FullMethod, start, timer, err, nil)
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

func (g *Gateway) logGRPC(ctx context.Context, ci *emu.CallInfo, service, method string, start time.Time, timer clock.Stopwatch, err error, bodies *reqlog.Bodies) {
	code := status.Code(toGRPC(err))
	g.log.Add(reqlog.Entry{
		Time: start, Service: service, Protocol: "grpc", Method: method, Resource: ci.Resource(),
		Principal: string(emu.PrincipalFrom(ctx)), Status: int(code),
		Code: apierr.CodeName(code), LatencyMS: ms(timer.Elapsed()), Bodies: bodies,
	})
}

// grpcBody is a unary call's message for the Request log, as protojson
// with its wire size; nil for no message.
func grpcBody(m any) *reqlog.Body {
	pm, ok := m.(proto.Message)
	if !ok || pm == nil || !pm.ProtoReflect().IsValid() {
		return nil
	}
	b, err := protojson.Marshal(pm)
	if err != nil {
		return nil
	}
	return reqlog.NewBody("application/json", b, int64(len(b)))
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

// emulatorToken prefixes the access tokens Instances issue.
const emulatorToken = "ya29.gcpemu."

// principal implements FR-CORE-050/051. A request a Linked Instance
// forwarded is its asserted Principal's, whatever its credentials say.
func (g *Gateway) principal(ctx context.Context, authz string) (emu.Principal, error) {
	def := emu.Principal(g.env.Config.DefaultPrincipal)
	if _, p, ok := emu.AssertedBy(ctx); ok {
		return cmp.Or(p, def), nil
	}
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
	// A token for a service account of a Delegated Project was issued by
	// its Linked Instance (ADR 0027). Only a token an Instance could have
	// issued is sent over a link, never Foreign credentials. While a Linked
	// Instance that may have issued it cannot be asked, no IAM mode answers
	// for it (#733).
	if l := g.linker(); l != nil && l.Delegating() && strings.HasPrefix(tok, emulatorToken) {
		p, unreachable := l.Authenticate(ctx, tok)
		if p != "" {
			return emu.Principal(p), nil
		}
		if len(unreachable) > 0 {
			return "", unreachableLinks(unreachable)
		}
	}
	if g.env.Auth.Mode() == config.IAMEnforce {
		return "", apierr.Unauthenticated("Request had invalid authentication credentials. Expected OAuth 2 access token, login cookie or other valid authentication credential.").
			WithReason("googleapis.com", "ACCESS_TOKEN_TYPE_UNSUPPORTED")
	}
	return def, nil
}

// unreachableLinks is the UNAVAILABLE error for an access token this
// Instance did not issue while the links unreachable, whose Linked
// Instances may have issued it, cannot be asked about it.
func unreachableLinks(unreachable map[string]error) error {
	why := make([]string, 0, len(unreachable))
	for _, name := range slices.Sorted(maps.Keys(unreachable)) {
		why = append(why, fmt.Sprintf("link %q: %v", name, unreachable[name]))
	}
	return apierr.New(codes.Unavailable, "Cannot authenticate the access token: this Instance did not issue it, and the Linked Instance that may have cannot be reached (%s).", strings.Join(why, "; "))
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
	r.g.apiOwners[api] = r.service
	for _, host := range hosts {
		r.g.hosts[host] = wrapped
		r.g.apiOwners[host] = r.service
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
	r.g.mu.Lock()
	r.g.fallback = r.service
	r.g.mu.Unlock()
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

// HandleAdmin mounts the admin API (not request-logged; only the Admin
// token guards it, see SetAdminToken).
func (g *Gateway) HandleAdmin(h http.Handler) { g.mux.Handle("/_emu/", h) }

// HandleConsole mounts the Web console at /console and /console/
// (not request-logged; FR-UI-002). Only the Admin token guards it.
func (g *Gateway) HandleConsole(h http.Handler) {
	g.mux.Handle("/console", h)
	g.mux.Handle("/console/", h)
}

// ServeHTTP dispatches gRPC, host-mode and path-routed requests, and
// forwards those naming a Delegated Project.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	markFrontend(r)
	name, answered := g.delegation(w, r)
	l := g.linker()
	delegating := l != nil && l.Delegating()
	switch {
	case answered:
	case name != "":
		g.serveDelegated(w, r, name)
	case delegating && g.mergedList(r):
		g.serveMerged(w, r, l)
	case delegating && g.bucketTaken(w, r, l, ""):
	default:
		g.serve(w, r)
	}
}

// serve dispatches gRPC, host-mode and path-routed requests.
func (g *Gateway) serve(w http.ResponseWriter, r *http.Request) {
	if isGRPC(r) {
		if d := g.grpcDelegated(r); d != nil {
			d.grpc.ServeHTTP(w, r)
			return
		}
		g.grpc.ServeHTTP(w, r)
		return
	}
	g.mu.RLock()
	h, ok := g.hosts[hostOnly(r.Host)]
	g.mu.RUnlock()
	if ok {
		h.ServeHTTP(w, r)
		return
	}
	if d := g.restDelegated(r); d != nil {
		d.rest.ServeHTTP(w, r)
		return
	}
	if !g.checkAdmin(w, r) {
		return
	}
	g.mux.ServeHTTP(w, r)
}

// Middleware authenticates, recovers from panics and logs each HTTP request.
func (g *Gateway) Middleware(service string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		timer := clock.StartStopwatch()
		if isAPIRequest(r.URL.Path) {
			// Before authentication, which reads the clock (ADR 0020).
			clock.RequestStep(g.env.Clock)
		}
		ctx, leave, err := g.gate.enter(r.Context())
		if err == nil {
			defer leave()
		}
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK, gateCtx: ctx}
		var reqBody reqlog.Capture
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = &teeBody{ReadCloser: r.Body, c: &reqBody}
		}
		ctx, ci := emu.WithCallInfo(ctx)
		ci.SetCall(service, r.Method+" "+r.URL.Path)
		r = r.WithContext(ctx)
		var p emu.Principal
		if err == nil {
			p, err = g.principal(r.Context(), r.Header.Get("Authorization"))
		}
		if err != nil {
			writeError(sw, err)
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
		sw.answerCutOff()
		g.log.Add(reqlog.Entry{
			Time: start, Service: service, Protocol: "http",
			Method: r.Method + " " + r.URL.Path, Resource: ci.Resource(), Principal: string(p),
			Status: sw.status, LatencyMS: ms(timer.Elapsed()),
			Bodies: &reqlog.Bodies{
				Request:  reqBody.Body(r.Header.Get("Content-Type")),
				Response: sw.body.Body(sw.Header().Get("Content-Type")),
			},
		})
	})
}

// isAPIRequest reports whether an HTTP request to a service handler is a
// GCP API call, which moves a deterministic clock one step (ADR 0020).
// Service-owned admin routes and internal endpoints (/_emu/...) and
// in-container agent callbacks (/_agent/..., .../_gcpemu/...) arrive on
// wall-clock schedules and do not. path is as the service handler sees
// it, so a mount's API prefix may already be stripped.
func isAPIRequest(path string) bool {
	return !strings.HasPrefix(path, "/_emu/") && !strings.HasPrefix(path, "/_agent/") &&
		!strings.Contains(path, "/_gcpemu/")
}

// teeBody copies what the handler reads of a request body to c.
type teeBody struct {
	io.ReadCloser
	c *reqlog.Capture
}

func (t *teeBody) Read(p []byte) (int, error) {
	n, err := t.ReadCloser.Read(p)
	_, _ = t.c.Write(p[:n])
	return n, err
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
	body   reqlog.Capture // what was written, for the Request log
	// gateCtx is the request's context from the gate: once the gate cuts
	// the request off, a handler that has not yet responded cannot, so that
	// Middleware answers UNAVAILABLE instead (answerCutOff).
	gateCtx context.Context
}

// refused reports whether a handler's response is dropped as cut off.
func (w *statusWriter) refused() bool {
	return !w.wrote && w.gateCtx != nil && cutOff(w.gateCtx)
}

// answerCutOff answers UNAVAILABLE, without any headers the handler set,
// when the gate cut the request off before its handler responded.
func (w *statusWriter) answerCutOff() {
	if !w.refused() {
		return
	}
	w.gateCtx = nil
	clear(w.Header())
	writeError(w, errUnavailable)
}

func (w *statusWriter) WriteHeader(code int) {
	if w.refused() {
		return
	}
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.refused() {
		return 0, errCutOff
	}
	w.wrote = true
	n, err := w.ResponseWriter.Write(b)
	_, _ = w.body.Write(b[:n])
	return n, err
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) Flush() {
	if w.refused() {
		return
	}
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

// Serve serves on l, and connections from ServeFrontend, until Shutdown.
func (g *Gateway) Serve(l net.Listener) error {
	g.mu.Lock()
	g.srv = NewServer(g, g.env.Log)
	g.srv.ConnContext = connContext
	srv := g.srv
	g.mu.Unlock()
	// Shutdown closes g.front before srv, so its Serve can end with
	// net.ErrClosed rather than ErrServerClosed. After Shutdown, Serve
	// waits for it, so nothing logs after Serve returns.
	front := make(chan struct{})
	go func() {
		defer close(front)
		if err := srv.Serve(g.front); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			g.env.Log.Error("API gateway stopped serving the Google front end", "err", err)
		}
	}()
	err := srv.Serve(l)
	if errors.Is(err, http.ErrServerClosed) {
		<-front
		return nil
	}
	return err
}

// Shutdown stops the gateway.
func (g *Gateway) Shutdown(ctx context.Context) error {
	g.mu.RLock()
	srv := g.srv
	g.mu.RUnlock()
	g.front.Close()
	g.grpc.Stop()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}
