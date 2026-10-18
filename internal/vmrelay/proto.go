package vmrelay

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"strings"
	"time"

	"github.com/hashicorp/yamux"
)

// The wire protocol. The emulator opens one TCP connection to the relay's
// control port and runs yamux over it; either side opens streams. Each
// stream starts with one header line naming what it carries:
//
//	emulator → relay
//	  ping                      → "OK"
//	  dial <network> <ip:port>  → "OK" or "ERR <reason>", then the connection
//	  listen <name> <ip:port> <tcp|udp>
//	                            → "OK <addr>" or "ERR <reason>"
//	  offer <name> <ip:port> <tcp|udp>
//	                            → "OK" or "ERR <reason>": listen on exactly
//	                              that address, for an Offered port
//	  withdraw <name>           → "OK": stop listening for name
//	relay → emulator
//	  tcp <name>                → "OK" or "ERR <reason>", then a connection
//	                              to the named emulator endpoint
//	  udp <name>, one query frame
//	                            → "OK" or "ERR <reason>", then the answer
//	                              frames
//
// The emulator answers a relay stream once it holds the endpoint
// connection, and only then do bytes flow. Until the answer, the relay
// tries a stream that fails again on the next session, since nothing of
// it reached the endpoint; an inward connection or datagram waits for a
// session while the emulator reconnects. After "OK" there is no retry.
//
// A datagram is framed as a 2-byte big-endian length and the payload.

// handshakeTimeout bounds a stream's header exchange.
const handshakeTimeout = 15 * time.Second

func yamuxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	c.LogOutput = nil
	c.Logger = log.New(io.Discard, "", 0)
	c.KeepAliveInterval = 10 * time.Second
	c.ConnectionWriteTimeout = 10 * time.Second
	return c
}

// bufConn is a stream whose header was read through r: later reads must
// see what r buffered.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func newBufConn(c net.Conn) *bufConn { return &bufConn{Conn: c, r: bufio.NewReader(c)} }

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// CloseWrite ends the sending side.
func (c *bufConn) CloseWrite() error {
	closeWrite(c.Conn)
	return nil
}

// readLine reads one header line.
func (c *bufConn) readLine() (string, error) {
	var b strings.Builder
	for b.Len() < 1024 {
		ch, err := c.r.ReadByte()
		if err != nil {
			return "", err
		}
		if ch == '\n' {
			return b.String(), nil
		}
		b.WriteByte(ch)
	}
	return "", errors.New("vmrelay: header line too long")
}

func writeLine(w io.Writer, fields ...string) error {
	_, err := io.WriteString(w, strings.Join(fields, " ")+"\n")
	return err
}

func writeFrame(w io.Writer, p []byte) error {
	if len(p) > 0xffff {
		return errors.New("vmrelay: datagram too large")
	}
	b := make([]byte, 2+len(p))
	binary.BigEndian.PutUint16(b, uint16(len(p)))
	copy(b[2:], p)
	_, err := w.Write(b)
	return err
}

func readFrame(r io.Reader) ([]byte, error) {
	var n [2]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return nil, err
	}
	p := make([]byte, binary.BigEndian.Uint16(n[:]))
	_, err := io.ReadFull(r, p)
	return p, err
}

// pipe copies both ways until both directions end, then closes both.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		closeWrite(dst)
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
	a.Close()
	b.Close()
}

// closeWrite half-closes c. Closing a yamux stream only ends its sending
// side; it stays readable until the peer closes too.
func closeWrite(c net.Conn) {
	switch c := c.(type) {
	case interface{ CloseWrite() error }:
		_ = c.CloseWrite()
	case *yamux.Stream:
		_ = c.Close()
	}
}
