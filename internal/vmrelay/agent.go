package vmrelay

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/linuxuser586/gcpemu/internal/agent"
)

// The "vmrelay" agent runs in the VM relay container.

const agentName = "vmrelay"

// ControlPort is the port the agent serves the emulator on; the container
// publishes it on the host's 127.0.0.1.
const ControlPort = 7171

func init() { agent.Register(agentName, runAgent) }

func runAgent(ctx context.Context, _ []string) error {
	l, err := net.Listen("tcp", ":"+strconv.Itoa(ControlPort))
	if err != nil {
		return err
	}
	a := newRelayAgent()
	go func() {
		<-ctx.Done()
		l.Close()
	}()
	a.serve(l)
	return nil
}

// relayAgent is the container side: it dials and listens on the
// container networks for the emulator.
type relayAgent struct {
	mu        sync.Mutex
	sess      *yamux.Session
	listeners map[string]*agentListener // endpoint name → its listener
}

// agentListener is what the agent listens on for one endpoint.
type agentListener struct {
	addr  string
	close func()
}

func newRelayAgent() *relayAgent { return &relayAgent{listeners: map[string]*agentListener{}} }

// serve accepts control connections until l closes. A new connection
// replaces the current one: the emulator reconnects after a drop.
func (a *relayAgent) serve(l net.Listener) {
	defer a.close()
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		s, err := yamux.Server(c, yamuxConfig())
		if err != nil {
			c.Close()
			continue
		}
		a.mu.Lock()
		old := a.sess
		a.sess = s
		a.mu.Unlock()
		if old != nil {
			old.Close()
		}
		go a.accept(s)
	}
}

func (a *relayAgent) close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sess != nil {
		a.sess.Close()
	}
	for name, l := range a.listeners {
		l.close()
		delete(a.listeners, name)
	}
}

func (a *relayAgent) accept(s *yamux.Session) {
	for {
		st, err := s.Accept()
		if err != nil {
			return
		}
		go a.handle(st)
	}
}

func (a *relayAgent) handle(st net.Conn) {
	c := newBufConn(st)
	_ = c.SetDeadline(time.Now().Add(handshakeTimeout))
	line, err := c.readLine()
	if err != nil {
		c.Close()
		return
	}
	f := strings.Fields(line)
	switch {
	case len(f) == 1 && f[0] == "ping":
		_ = writeLine(c, "OK")
		c.Close()
	case len(f) == 3 && f[0] == "dial":
		u, err := net.DialTimeout(f[1], f[2], 10*time.Second)
		if err != nil {
			_ = writeLine(c, "ERR", err.Error())
			c.Close()
			return
		}
		if err := writeLine(c, "OK"); err != nil {
			u.Close()
			c.Close()
			return
		}
		_ = c.SetDeadline(time.Time{})
		pipe(c, u)
	case len(f) == 4 && f[0] == "listen":
		addr, err := a.listen(f[1], f[2], f[3] == "udp")
		if err != nil {
			_ = writeLine(c, "ERR", err.Error())
		} else {
			_ = writeLine(c, "OK", addr)
		}
		c.Close()
	case len(f) == 4 && f[0] == "offer":
		if err := a.offer(f[1], f[2], f[3] == "udp"); err != nil {
			_ = writeLine(c, "ERR", err.Error())
		} else {
			_ = writeLine(c, "OK")
		}
		c.Close()
	case len(f) == 2 && f[0] == "withdraw":
		a.withdraw(f[1])
		_ = writeLine(c, "OK")
		c.Close()
	default:
		_ = writeLine(c, "ERR", fmt.Sprintf("unknown request %q", line))
		c.Close()
	}
}

func (a *relayAgent) current() *yamux.Session {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sess
}

// listen binds addr for the named endpoint, or another port on its host
// when that one is taken, and relays what arrives to the emulator. It is
// idempotent per name.
func (a *relayAgent) listen(name, addr string, withUDP bool) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if got, ok := a.listeners[name]; ok {
		return got.addr, nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return "", err
	}
	l, pc, err := listenPair(addr, withUDP)
	if err != nil {
		// The port, or its UDP twin, is taken: try ports the kernel picks.
		// A free TCP port's UDP twin can still be held or, on Windows, sit
		// in an excluded port range, so a few are tried.
		anyPort := net.JoinHostPort(host, "0")
		for range udpPairAttempts {
			if l, pc, err = listenPair(anyPort, withUDP); err == nil {
				break
			}
		}
		if err != nil {
			return "", err
		}
	}
	got := l.Addr().String()
	if pc != nil {
		go a.serveUDP(name, pc)
	}
	a.listeners[name] = &agentListener{addr: got, close: func() {
		l.Close()
		if pc != nil {
			pc.Close()
		}
	}}
	go a.serveTCP(name, l)
	return got, nil
}

// offer binds exactly addr for the named endpoint, over TCP or UDP, and
// relays what arrives to the emulator: an Offered port (ADR 0014), which
// must not move. It is idempotent per name.
func (a *relayAgent) offer(name, addr string, udp bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.listeners[name]; ok {
		return nil
	}
	if udp {
		pc, err := net.ListenPacket("udp", addr)
		if err != nil {
			return err
		}
		a.listeners[name] = &agentListener{addr: addr, close: func() { pc.Close() }}
		go a.serveUDP(name, pc)
		return nil
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	a.listeners[name] = &agentListener{addr: addr, close: func() { l.Close() }}
	go a.serveTCP(name, l)
	return nil
}

// withdraw stops listening for the named endpoint; connections already
// relayed run on.
func (a *relayAgent) withdraw(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if l, ok := a.listeners[name]; ok {
		l.close()
		delete(a.listeners, name)
	}
}

// udpPairAttempts bounds the kernel-picked ports listen tries for a TCP
// listener with a UDP socket on the same port.
const udpPairAttempts = 10

// listenPair listens on addr over TCP and, withUDP, over UDP on the port
// TCP got.
func listenPair(addr string, withUDP bool) (net.Listener, net.PacketConn, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	if !withUDP {
		return l, nil, nil
	}
	pc, err := net.ListenPacket("udp", l.Addr().String())
	if err != nil {
		l.Close()
		return nil, nil, fmt.Errorf("udp on %s: %w", l.Addr(), err)
	}
	return l, pc, nil
}

func (a *relayAgent) serveTCP(name string, l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			s := a.current()
			if s == nil {
				c.Close()
				return
			}
			st, err := s.Open()
			if err != nil {
				c.Close()
				return
			}
			if err := writeLine(st, "tcp", name); err != nil {
				st.Close()
				c.Close()
				return
			}
			pipe(c, st)
		}()
	}
}

// serveUDP carries each datagram on its own stream, and sends back every
// answer that comes back on it.
func (a *relayAgent) serveUDP(name string, pc net.PacketConn) {
	buf := make([]byte, 0xffff)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		q := append([]byte(nil), buf[:n]...)
		go func() {
			s := a.current()
			if s == nil {
				return
			}
			st, err := s.Open()
			if err != nil {
				return
			}
			defer st.Close()
			_ = st.SetDeadline(time.Now().Add(10 * time.Second))
			if writeLine(st, "udp", name) != nil || writeFrame(st, q) != nil {
				return
			}
			for {
				p, err := readFrame(st)
				if err != nil {
					return
				}
				_, _ = pc.WriteTo(p, from)
			}
		}()
	}
}
