// Package httpserver is the HTTP server the API gateway and the Services'
// own ports serve with: HTTP/1.1 and h2c, and a Shutdown that stops even
// when its ctx has already ended (#1034, #1117).
package httpserver

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// Server is an http.Server speaking HTTP/1.1 and h2c. Callers may set its
// fields, such as ConnState, ConnContext, HTTP2 or TLSConfig, before
// serving, but not BaseContext, which Shutdown uses to end every request.
type Server struct {
	*http.Server
	name string
	log  *slog.Logger
	// end ends every request's context, so connections handed over by
	// the server, such as hijacked or upgraded ones, close too.
	end context.CancelFunc

	mu  sync.Mutex
	lns []*closeErrListener // what Serve and ServeTLS serve on
}

// New returns a Server for h. name names it in the line Shutdown logs
// when it closes connections by force, such as "API gateway".
func New(name string, h http.Handler, log *slog.Logger) *Server {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
	srv.Protocols = new(http.Protocols)
	srv.Protocols.SetHTTP1(true)
	srv.Protocols.SetHTTP2(true)
	srv.Protocols.SetUnencryptedHTTP2(true)
	base, end := context.WithCancel(context.Background())
	srv.BaseContext = func(net.Listener) context.Context { return base }
	return &Server{Server: srv, name: name, log: log, end: end}
}

// Serve serves on l until Shutdown or Close.
func (s *Server) Serve(l net.Listener) error { return s.Server.Serve(s.track(l)) }

// ServeTLS serves TLS on l until Shutdown or Close.
func (s *Server) ServeTLS(l net.Listener, certFile, keyFile string) error {
	return s.Server.ServeTLS(s.track(l), certFile, keyFile)
}

func (s *Server) track(l net.Listener) net.Listener {
	ln := &closeErrListener{Listener: l}
	s.mu.Lock()
	s.lns = append(s.lns, ln)
	s.mu.Unlock()
	return ln
}

// Shutdown stops the Server. It drains within ctx; if ctx ends first, the
// connections still open are closed by force, which is the normal end of
// a shutdown rather than an error, and one Info line says so. Then it
// ends every request's context, so connections the server handed over
// close as their handlers return. It reports only a real failure to
// stop, such as a listener that failed to close.
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.Server.Shutdown(ctx)
	defer s.end()
	if drainCut := ctx.Err() != nil && errors.Is(err, ctx.Err()); !drainCut {
		return err
	}
	// Shutdown returned ctx's error in place of the listeners', and Close
	// no longer sees the listeners, so they report it.
	err = errors.Join(s.Server.Close(), s.closeErr())
	if err == nil {
		s.log.Info(s.name + " closed connections still open at shutdown")
	}
	return err
}

func (s *Server) closeErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	for _, l := range s.lns {
		errs = append(errs, l.closeErr())
	}
	return errors.Join(errs...)
}

// closeErrListener remembers the error from closing it, which
// http.Server.Shutdown does not report once its ctx has ended.
type closeErrListener struct {
	net.Listener
	mu  sync.Mutex
	err error
}

func (l *closeErrListener) Close() error {
	err := l.Listener.Close()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err == nil {
		l.err = err
	}
	return err
}

func (l *closeErrListener) closeErr() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}
