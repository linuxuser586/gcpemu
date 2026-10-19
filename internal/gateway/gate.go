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

// errCutOff is the cause a closing gate cancels a request's context with.
var errCutOff = errors.New("cut off by an Instance Reset or State snapshot restore")

// errUnavailable is a request refused, or cut off, by the closed gate.
var errUnavailable = apierr.New(codes.Unavailable, "The service is unavailable while the Instance is reset or a State snapshot is restored; retry shortly.")

// retryAfter is the Retry-After, in seconds, of a refused REST request.
const retryAfter = "1"

// gate tracks the requests in progress through the API gateway so that an
// Instance Reset or State snapshot restore can refuse new ones and cut off
// those already running (#302). The Admin API never passes through it. The
// zero value is open.
type gate struct {
	mu      sync.Mutex
	closed  bool
	next    uint64
	running map[uint64]context.CancelCauseFunc
	work    inflight.Tracker // counts running, for close to wait on
}

// enter admits a request, returning its context, which is cancelled with
// errCutOff when the gate closes, and the func that ends it; or
// errUnavailable when the gate is closed.
func (gt *gate) enter(ctx context.Context) (context.Context, func(), error) {
	gt.mu.Lock()
	defer gt.mu.Unlock()
	if gt.closed {
		return ctx, nil, errUnavailable
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
// gate stays closed either way until open.
func (gt *gate) close(ctx context.Context) error {
	gt.mu.Lock()
	gt.closed = true
	for _, cancel := range gt.running {
		cancel(errCutOff)
	}
	gt.mu.Unlock()
	return gt.work.Wait(ctx)
}

func (gt *gate) open() {
	gt.mu.Lock()
	gt.closed = false
	gt.mu.Unlock()
}

// cutOff reports whether ctx, from enter, was cancelled by close.
func cutOff(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errCutOff)
}

// writeError writes err as a REST error response, telling the client when
// to retry a request the closed gate refused.
func writeError(w http.ResponseWriter, err error) {
	if err == errUnavailable {
		w.Header().Set("Retry-After", retryAfter)
	}
	apierr.Write(w, err)
}

// CloseGate refuses every new non-Admin request as UNAVAILABLE, cancels
// those in progress, streams included, and waits for their handlers to
// return, or returns ctx's error when it ends first. The gate stays closed
// until OpenGate, even on error. An Instance closes it for the length of a
// Reset or State snapshot restore (#302).
func (g *Gateway) CloseGate(ctx context.Context) error { return g.gate.close(ctx) }

// OpenGate serves requests again after CloseGate.
func (g *Gateway) OpenGate() { g.gate.open() }
