package gateway

import (
	"context"
	"net"
	"net/http"
	"sync"
)

// ServeFrontend serves c, a connection whose TLS the Google front end
// (internal/frontend) terminated, as plain HTTP/1.1 or prior-knowledge h2c
// like the API gateway's port. Requests on it arrive with X-Forwarded-Proto:
// https, so links services render (GCS session URIs, selfLink, mediaLink)
// use the scheme the client used (#440). The hand-off is in process: no
// listener exists that a client could reach to claim the marker, and a
// client-sent X-Forwarded-Proto on the API gateway's port is left as it
// was.
//
// ServeFrontend returns once the API gateway has closed c, with
// net.ErrClosed after Shutdown, or with ctx's error if ctx ends before the
// API gateway accepts c.
func (g *Gateway) ServeFrontend(ctx context.Context, c net.Conn) error {
	fc := &frontConn{Conn: c, done: make(chan struct{})}
	select {
	case g.front.conns <- fc:
	case <-g.front.closed:
		return net.ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
	<-fc.done
	return nil
}

// frontKey marks the context of a connection handed over by ServeFrontend.
type frontKey struct{}

// connContext marks connections from the Google front end.
func connContext(ctx context.Context, c net.Conn) context.Context {
	if _, ok := c.(*frontConn); ok {
		return context.WithValue(ctx, frontKey{}, true)
	}
	return ctx
}

// markFrontend sets X-Forwarded-Proto: https on requests that came through
// the Google front end, replacing any value the client sent.
func markFrontend(r *http.Request) {
	if via, _ := r.Context().Value(frontKey{}).(bool); via {
		r.Header.Set("X-Forwarded-Proto", "https")
	}
}

// frontConn is a connection handed over by ServeFrontend; done is closed
// when the API gateway closes it.
type frontConn struct {
	net.Conn
	once sync.Once
	done chan struct{}
}

func (c *frontConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.done) })
	return err
}

// handoff is the in-process listener ServeFrontend feeds.
type handoff struct {
	conns  chan net.Conn
	once   sync.Once
	closed chan struct{}
}

func newHandoff() *handoff {
	return &handoff{conns: make(chan net.Conn), closed: make(chan struct{})}
}

func (h *handoff) Accept() (net.Conn, error) {
	select {
	case c := <-h.conns:
		return c, nil
	case <-h.closed:
		return nil, net.ErrClosed
	}
}

func (h *handoff) Close() error {
	h.once.Do(func() { close(h.closed) })
	return nil
}

func (h *handoff) Addr() net.Addr { return frontAddr{} }

type frontAddr struct{}

func (frontAddr) Network() string { return "frontend" }
func (frontAddr) String() string  { return "frontend" }
