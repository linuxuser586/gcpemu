package gateway

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"google.golang.org/grpc/codes"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/inflight"
)

// GateReason is why the API gateway's gate closes, which decides what a
// refused or cut-off request is told.
type GateReason int

const (
	// GateStateChange closes the gate for an Instance Reset or State
	// snapshot restore (#302): clients are told to retry shortly.
	GateStateChange GateReason = iota
	// GateShutdown closes the gate for the Instance's Teardown (#1118):
	// clients are told it is shutting down, with no Retry-After, and the
	// gate never opens again.
	GateShutdown
)

var (
	// errCutOff and errShutdownCutOff are the causes a closing gate
	// cancels a request's context with.
	errCutOff         = errors.New("cut off by an Instance Reset or State snapshot restore")
	errShutdownCutOff = errors.New("cut off by the Instance's shutdown")

	// errUnavailable and errShuttingDown are a request refused, or cut
	// off, by the gate closed for each reason.
	errUnavailable  = apierr.New(codes.Unavailable, "The service is unavailable while the Instance is reset or a State snapshot is restored; retry shortly.")
	errShuttingDown = apierr.New(codes.Unavailable, "The service is unavailable because the Instance is shutting down.")
)

// retryAfter is the Retry-After, in seconds, of a REST request refused
// while the Instance is reset or a State snapshot restored.
const retryAfter = "1"

// gate tracks the requests in progress through the API gateway so that an
// Instance Reset or State snapshot restore can refuse new ones and cut off
// those already running (#302), and so that Teardown can do the same for
// good (#1118). The Admin API never passes through it. The zero value is
// open.
type gate struct {
	mu       sync.Mutex
	closed   bool
	shutdown bool // closed for the Instance's shutdown, so never opens
	next     uint64
	running  map[uint64]context.CancelCauseFunc
	work     inflight.Tracker // counts running, for close to wait on
}

// enter admits a request, returning its context, which is cancelled with
// the gate's cut-off cause when it closes, and the func that ends it; or
// the closed gate's refusal.
func (gt *gate) enter(ctx context.Context) (context.Context, func(), error) {
	gt.mu.Lock()
	defer gt.mu.Unlock()
	if gt.closed {
		return ctx, nil, gt.refusal()
	}
	ctx, cancel := context.WithCancelCause(ctx)
	if gt.running == nil {
		gt.running = map[uint64]context.CancelCauseFunc{}
	}
	id := gt.next
	gt.next++
	gt.running[id] = cancel
	gt.work.Enter()
	return ctx, func() {
		cancel(nil)
		gt.mu.Lock()
		delete(gt.running, id)
		gt.mu.Unlock()
		gt.work.Exit()
	}, nil
}

// close refuses new requests, cancels those in progress and waits for
// their handlers to return, or returns ctx's error when it ends first. The
// gate stays closed either way until open; once closed for GateShutdown,
// for good.
func (gt *gate) close(ctx context.Context, reason GateReason) error {
	gt.mu.Lock()
	gt.closed = true
	if reason == GateShutdown {
		gt.shutdown = true
	}
	cause := gt.cause()
	for _, cancel := range gt.running {
		cancel(cause)
	}
	gt.mu.Unlock()
	return gt.work.Wait(ctx)
}

func (gt *gate) open() {
	gt.mu.Lock()
	gt.closed = gt.shutdown
	gt.mu.Unlock()
}

// cause is what a closing gate cancels requests with. Callers hold mu.
func (gt *gate) cause() error {
	if gt.shutdown {
		return errShutdownCutOff
	}
	return errCutOff
}

// refusal is what the closed gate refuses a request with. Callers hold mu.
func (gt *gate) refusal() error {
	if gt.shutdown {
		return errShuttingDown
	}
	return errUnavailable
}

// count counts the requests in their handlers.
func (gt *gate) count() int {
	gt.mu.Lock()
	defer gt.mu.Unlock()
	return len(gt.running)
}

// cutOff returns what a request whose context, from enter, was cancelled
// by close is answered with, or nil if it was not. The answer follows the
// gate's reason now, so that a request a Reset cut off just before the
// Instance's shutdown is told it is shutting down.
func (gt *gate) cutOff(ctx context.Context) error {
	switch context.Cause(ctx) {
	case errCutOff, errShutdownCutOff:
		gt.mu.Lock()
		defer gt.mu.Unlock()
		return gt.refusal()
	}
	return nil
}

// writeError writes err as a REST error response, telling the client when
// to retry a request the gate refused for a Reset or State snapshot
// restore.
func writeError(w http.ResponseWriter, err error) {
	if err == errUnavailable {
		w.Header().Set("Retry-After", retryAfter)
	}
	apierr.Write(w, err)
}

// CloseGate refuses every new non-Admin request as UNAVAILABLE, cancels
// those in progress, streams included, and waits for their handlers to
// return, or returns ctx's error when it ends first. reason decides what
// clients are told. The gate stays closed until OpenGate, even on error;
// closed for GateShutdown, it never opens again. An Instance closes it for
// the length of a Reset or State snapshot restore (#302), and at the start
// of Teardown (#1118).
func (g *Gateway) CloseGate(ctx context.Context, reason GateReason) error {
	return g.gate.close(ctx, reason)
}

// OpenGate serves requests again after CloseGate, unless it closed for
// GateShutdown.
func (g *Gateway) OpenGate() { g.gate.open() }

// RunningRequests counts the requests through the gate still in their
// handlers, such as those a CloseGate whose ctx ended did not wait for.
func (g *Gateway) RunningRequests() int { return g.gate.count() }
