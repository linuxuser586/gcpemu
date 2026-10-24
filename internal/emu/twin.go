package emu

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"strconv"
	"sync"

	"github.com/linuxuser586/gcpemu/internal/loopback"
)

// The [::1] twin (#641). macOS lets one process listen on 127.0.0.1:PORT
// while another holds *:PORT, as AirPlay Receiver does for 5000 and 7000.
// A client resolving localhost to ::1, such as Docker Desktop, then reaches
// the other process. A listener on a loopback --bind therefore also listens
// on [::1] at the same port. The Instance still reports the IPv4 address.

// listenTCP is net.Listen; tests replace it.
var listenTCP = net.Listen

// twinRetries caps the fresh picks of a port 0 whose IPv4 port is taken
// on [::1].
const twinRetries = 8

// Twinned reports whether a listener on bind gets an [::1] twin.
func Twinned(bind string) bool { return bind == "127.0.0.1" || bind == "localhost" }

// ListenBind listens on Config.Bind:port, with an [::1] twin on a loopback
// --bind. Port 0 picks a free port, from --port-range when useRange is set
// (FR-CORE-042); a candidate taken on [::1] moves both families on to the
// next one. Addr is the IPv4 address, and Close closes both.
func (e *Env) ListenBind(port int, useRange bool) (net.Listener, error) {
	host := e.Config.Bind
	if !Twinned(host) {
		if port == 0 && !useRange {
			return net.Listen("tcp", net.JoinHostPort(host, "0"))
		}
		return e.ListenTCP(host, port)
	}
	if port != 0 {
		return e.listenTwin(port)
	}
	if lo, hi, ok, err := e.Config.ParsePortRange(); useRange && ok && err == nil {
		n := hi - lo + 1
		off := rand.IntN(n)
		for i := range n {
			if l, err := e.listenTwin(lo + (off+i)%n); err == nil {
				return l, nil
			}
		}
		return nil, fmt.Errorf("no port in range %s free on both 127.0.0.1 and [::1]", e.Config.PortRange)
	}
	for i := 1; ; i++ {
		l, err := e.listenTwin(0)
		if err == nil || !loopback.AddrInUse(err) || i == twinRetries {
			return l, err
		}
	}
}

// listenTwin listens on 127.0.0.1:port and [::1] at the port IPv4 got.
// Without IPv6 loopback on the host the listener is IPv4-only.
func (e *Env) listenTwin(port int) (net.Listener, error) {
	v4, err := listenTCP("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	p := strconv.Itoa(v4.Addr().(*net.TCPAddr).Port)
	v6, err := listenTCP("tcp", net.JoinHostPort("::1", p))
	switch {
	case err == nil:
		return newTwin(v4, v6), nil
	case loopback.AddrInUse(err):
		v4.Close()
		return nil, fmt.Errorf("port %s is in use on [::1] (IPv6) by another process: %w", p, err)
	case !ipv6Loopback():
		if e.Log != nil {
			e.Log.Debug("no IPv6 loopback; listening on IPv4 only", "address", v4.Addr().String(), "err", err)
		}
		return v4, nil
	}
	v4.Close()
	return nil, fmt.Errorf("listen [::1]:%s (IPv6): %w", p, err)
}

// ipv6Loopback reports whether the host can listen on [::1].
func ipv6Loopback() bool {
	l, err := listenTCP("tcp", "[::1]:0")
	if err == nil {
		l.Close()
	}
	return err == nil
}

// twinListener accepts on an IPv4 and an IPv6 listener at once.
type twinListener struct {
	v4, v6   net.Listener
	accepted chan accepted
	done     chan struct{}
	close    sync.Once
}

type accepted struct {
	c   net.Conn
	err error
}

func newTwin(v4, v6 net.Listener) *twinListener {
	t := &twinListener{v4: v4, v6: v6, accepted: make(chan accepted), done: make(chan struct{})}
	go t.pump(v4)
	go t.pump(v6)
	return t
}

// pump hands l's connections and errors to Accept until l is closed.
func (t *twinListener) pump(l net.Listener) {
	for {
		c, err := l.Accept()
		if errors.Is(err, net.ErrClosed) {
			return
		}
		select {
		case t.accepted <- accepted{c, err}:
		case <-t.done:
			if c != nil {
				c.Close()
			}
			return
		}
	}
}

func (t *twinListener) Accept() (net.Conn, error) {
	select {
	case a := <-t.accepted:
		return a.c, a.err
	case <-t.done:
		return nil, &net.OpError{Op: "accept", Net: "tcp", Addr: t.Addr(), Err: net.ErrClosed}
	}
}

func (t *twinListener) Close() error {
	err := net.ErrClosed
	t.close.Do(func() {
		close(t.done)
		err = errors.Join(t.v4.Close(), t.v6.Close())
	})
	return err
}

// Addr is the IPv4 address.
func (t *twinListener) Addr() net.Addr { return t.v4.Addr() }
