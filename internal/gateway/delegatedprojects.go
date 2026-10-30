package gateway

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/reqlog"
)

// Delegated Projects (ADR 0027): the API gateway forwards every request
// naming a Project delegated to a Linked Instance over the link, whatever
// the Service, having authenticated the caller, whose Principal it
// asserts. On the Linked Instance it serves the requests forwarded to it,
// for the Projects it shares with that link, as the asserted Principal.

// Links is what the API gateway needs of the Instance's links.
type Links interface {
	// Delegating reports whether any Project is delegated.
	Delegating() bool
	// DelegatedLinks returns the links any Project is delegated to, by
	// name.
	DelegatedLinks() []string
	// DelegatedTo returns the link the Project ref, an ID or number, is
	// delegated to.
	DelegatedTo(ref string) (string, bool)
	// Shared reports whether the Project ref is shared with the link name.
	Shared(name, ref string) bool
	// BucketOwner returns the link whose Linked Instance has the bucket,
	// and its Project there.
	BucketOwner(bucket string) (link, project string, ok bool)
	// RefreshBuckets asks the link name for its buckets now.
	RefreshBuckets(ctx context.Context, name string)
	// Forward forwards r over the link name, asserting principal, or calls
	// fail, having written nothing. answered, when not nil, sees the
	// Linked Instance's answer before it is written.
	Forward(w http.ResponseWriter, r *http.Request, name, principal string, fail func(http.ResponseWriter, error), answered func(*http.Response))
}

// SetLinks has the API gateway forward Delegated Projects over links and
// serve the requests forwarded to it. Call it before Serve.
func (g *Gateway) SetLinks(l Links) {
	g.mu.Lock()
	g.links = l
	g.mu.Unlock()
}

func (g *Gateway) linker() Links {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.links
}

// isGRPC reports whether r is a gRPC call.
func isGRPC(r *http.Request) bool {
	return r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc")
}

// delegation returns the link the Projects r names are delegated to. A
// request naming both a Delegated Project and another Project, local or
// delegated to another link, is answered here with an error, since no one
// Instance holds both.
func (g *Gateway) delegation(w http.ResponseWriter, r *http.Request) (name string, answered bool) {
	l := g.linker()
	if l == nil || !l.Delegating() {
		return "", false
	}
	ids := g.projectsOf(r)
	if len(ids) == 0 {
		return g.bucketLink(r, l), false
	}
	var to []string // the links, "" for a local Project
	for _, id := range ids {
		name, _ := l.DelegatedTo(id)
		if !slices.Contains(to, name) {
			to = append(to, name)
		}
	}
	switch len(to) {
	case 0:
		return "", false
	case 1:
		return to[0], false
	}
	writeErrorFor(w, r, apierr.FailedPrecondition("The request names the Projects %s, which are not all delegated to one Linked Instance, so no one Instance can serve it.", strings.Join(ids, ", ")))
	return "", true
}

// owner returns the Service that would serve r on this Instance, by
// which fault rules match a forwarded request.
func (g *Gateway) owner(r *http.Request) string {
	if isGRPC(r) {
		return g.grpcOwner(r.URL.Path)
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if svc, ok := g.apiOwners[hostOnly(r.Host)]; ok {
		return svc
	}
	api, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	return g.apiOwners[api]
}

// writeErrorFor answers r, REST or gRPC, with err.
func writeErrorFor(w http.ResponseWriter, r *http.Request, err error) {
	if isGRPC(r) {
		writeGRPCError(w, apierr.From(err))
		return
	}
	apierr.Write(w, err)
}

// serveDelegated forwards r over the link name: it authenticates the
// caller, applies the fault rules, and records the request in the Request
// log marked as delegated. It neither moves a Deterministic Instance's
// clock nor waits for Reset, which act on local Projects only.
func (g *Gateway) serveDelegated(w http.ResponseWriter, r *http.Request, name string) {
	start := time.Now()
	timer := clock.StartStopwatch()
	service, grpcCall := g.owner(r), isGRPC(r)
	method := r.URL.Path
	if !grpcCall {
		method = r.Method + " " + r.URL.Path
	}
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	var reqBody reqlog.Capture
	if !grpcCall && r.Body != nil && r.Body != http.NoBody {
		r.Body = &teeBody{ReadCloser: r.Body, c: &reqBody}
	}
	links := g.linker()
	p, err := g.principal(r.Context(), r.Header.Get("Authorization"))
	var answered func(*http.Response)
	if g.bucketChange(r) {
		// So that the next request on the bucket is routed by the change.
		answered = func(resp *http.Response) {
			if resp.StatusCode < 300 {
				links.RefreshBuckets(r.Context(), name)
			}
		}
	}
	switch {
	case err != nil:
		writeErrorFor(sw, r, err)
	case grpcCall && g.injectGRPCError(sw, r, service, method):
	case !grpcCall && g.injectHTTP(sw, r, service):
	case g.bucketTaken(sw, r, links, name):
	default:
		links.Forward(sw, r, name, string(p), func(w http.ResponseWriter, err error) {
			if stsExchange(r) {
				// STS answers in OAuth's error shape, as its clients expect.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "temporarily_unavailable", "error_description": err.Error()})
				return
			}
			writeErrorFor(w, r, apierr.New(codes.Unavailable, "%v", err))
		}, answered)
	}
	e := reqlog.Entry{
		Time: start, Service: service, Protocol: "http", Method: method, Principal: string(p),
		Status: sw.status, LatencyMS: ms(timer.Elapsed()), Delegated: name,
	}
	if grpcCall {
		code := grpcStatus(sw.Header())
		e.Protocol, e.Status, e.Code = "grpc", int(code), apierr.CodeName(code)
	} else {
		e.Bodies = &reqlog.Bodies{
			Request:  reqBody.Body(r.Header.Get("Content-Type")),
			Response: sw.body.Body(sw.Header().Get("Content-Type")),
		}
	}
	g.log.Add(e)
}

// injectGRPCError applies a matching fault rule to a gRPC call and
// reports whether it answered the call.
func (g *Gateway) injectGRPCError(w http.ResponseWriter, r *http.Request, service, method string) bool {
	err := g.injectGRPC(r.Context(), service, method)
	if err != nil {
		writeErrorFor(w, r, err)
	}
	return err != nil
}

// grpcStatus is the code of a forwarded gRPC call, from the grpc-status
// its answer carried as a header or a trailer.
func grpcStatus(h http.Header) codes.Code {
	v := h.Get("Grpc-Status")
	if v == "" {
		v = h.Get(http.TrailerPrefix + "Grpc-Status")
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return codes.Unknown
	}
	return codes.Code(n)
}

// ServeLinked serves r, which a paired Instance forwarded over the link
// from, asserting that its caller is principal. It serves only requests
// naming Projects, all of which this Instance shares with that link, as
// its API gateway serves them otherwise, but never forwards them on, and
// never serves the Admin API or the Web console. A merged list it answers
// with the items of the Projects shared with that link.
func (g *Gateway) ServeLinked(w http.ResponseWriter, r *http.Request, from, principal string) {
	r = linkCaller(r)
	// As in ServeHTTP: cookies are never a public API's (#888).
	r.Header.Del("Cookie")
	ids, l := g.projectsOf(r), g.linker()
	refuse := func(what string) {
		writeErrorFor(w, r, apierr.PermissionDenied("The request forwarded by the Linked Instance %q %s.", from, what))
	}
	if len(ids) == 0 && l != nil && g.mergedList(r) {
		g.serveSharedList(w, r.WithContext(emu.WithAsserted(r.Context(), from, emu.Principal(principal))), l, from)
		return
	}
	if len(ids) == 0 || l == nil {
		refuse("names no Project")
		return
	}
	for _, id := range ids {
		if !l.Shared(from, id) {
			refuse("names Project " + strconv.Quote(id) + ", which is not shared with it")
			return
		}
	}
	r = r.WithContext(emu.WithAsserted(r.Context(), from, emu.Principal(principal)))
	g.serve(w, r)
}

// linkCaller marks r as a request from the LAN, at the --link-bind address
// it arrived at, so that Services report the Data plane addresses of their
// LAN listeners, which the caller's machine can reach (ADR 0027).
func linkCaller(r *http.Request) *http.Request {
	var local string
	if a, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		local, _, _ = net.SplitHostPort(a.String())
	}
	return r.WithContext(emu.WithLANCaller(r.Context(), local))
}
