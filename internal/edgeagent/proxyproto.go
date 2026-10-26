package edgeagent

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

// PROXY protocol v2 (TCP over IPv4/IPv6) between the agent and the
// emulator's ingress, so the emulator sees the real client address.

var proxyV2Sig = []byte{0x0D, 0x0A, 0x0D, 0x0A, 0x00, 0x0D, 0x0A, 0x51, 0x55, 0x49, 0x54, 0x0A}

// TLV is a PROXY v2 type-length-value field, following the addresses.
type TLV struct {
	Type  byte
	Value []byte
}

// WriteProxyV2 writes a PROXY v2 header for a TCP connection src → dst,
// with tlvs after the addresses.
func WriteProxyV2(w io.Writer, src, dst *net.TCPAddr, tlvs ...TLV) error {
	s4, d4 := src.IP.To4(), dst.IP.To4()
	var addrs []byte
	family := byte(0x21)
	if s4 != nil && d4 != nil {
		family = 0x11
		addrs = append(append(addrs, s4...), d4...)
	} else {
		addrs = append(append(addrs, src.IP.To16()...), dst.IP.To16()...)
	}
	addrs = binary.BigEndian.AppendUint16(addrs, uint16(src.Port))
	addrs = binary.BigEndian.AppendUint16(addrs, uint16(dst.Port))
	for _, t := range tlvs {
		addrs = append(addrs, t.Type)
		addrs = binary.BigEndian.AppendUint16(addrs, uint16(len(t.Value)))
		addrs = append(addrs, t.Value...)
	}
	var b []byte
	b = append(b, proxyV2Sig...)
	b = append(b, 0x21, family)
	b = binary.BigEndian.AppendUint16(b, uint16(len(addrs)))
	_, err := w.Write(append(b, addrs...))
	return err
}

// ReadProxyV2 parses a PROXY v2 header from r, ignoring its TLVs.
func ReadProxyV2(r *bufio.Reader) (src, dst *net.TCPAddr, err error) {
	src, dst, _, err = ReadProxyV2TLVs(r)
	return src, dst, err
}

// ReadProxyV2TLVs parses a PROXY v2 header from r, with the TLVs that
// follow its addresses.
func ReadProxyV2TLVs(r *bufio.Reader) (src, dst *net.TCPAddr, tlvs []TLV, err error) {
	hdr := make([]byte, 16)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, nil, nil, err
	}
	if string(hdr[:12]) != string(proxyV2Sig) || hdr[12]>>4 != 2 {
		return nil, nil, nil, errors.New("not a PROXY v2 header")
	}
	n := int(binary.BigEndian.Uint16(hdr[14:16]))
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, nil, nil, err
	}
	var rest []byte
	switch hdr[13] {
	case 0x11:
		if n < 12 {
			return nil, nil, nil, errors.New("short PROXY v2 address block")
		}
		src = &net.TCPAddr{IP: net.IP(body[0:4]), Port: int(binary.BigEndian.Uint16(body[8:10]))}
		dst = &net.TCPAddr{IP: net.IP(body[4:8]), Port: int(binary.BigEndian.Uint16(body[10:12]))}
		rest = body[12:]
	case 0x21:
		if n < 36 {
			return nil, nil, nil, errors.New("short PROXY v2 address block")
		}
		src = &net.TCPAddr{IP: net.IP(body[0:16]), Port: int(binary.BigEndian.Uint16(body[32:34]))}
		dst = &net.TCPAddr{IP: net.IP(body[16:32]), Port: int(binary.BigEndian.Uint16(body[34:36]))}
		rest = body[36:]
	default:
		return nil, nil, nil, errors.New("unsupported PROXY v2 family")
	}
	for len(rest) > 0 {
		if len(rest) < 3 {
			return nil, nil, nil, errors.New("short PROXY v2 TLV")
		}
		l := int(binary.BigEndian.Uint16(rest[1:3]))
		if len(rest) < 3+l {
			return nil, nil, nil, errors.New("short PROXY v2 TLV")
		}
		tlvs = append(tlvs, TLV{Type: rest[0], Value: rest[3 : 3+l]})
		rest = rest[3+l:]
	}
	return src, dst, tlvs, nil
}

// proxiedConn is a connection whose addresses come from a PROXY header.
type proxiedConn struct {
	net.Conn
	r           *bufio.Reader
	local, peer net.Addr
}

func (c *proxiedConn) Read(b []byte) (int, error) { return c.r.Read(b) }
func (c *proxiedConn) RemoteAddr() net.Addr       { return c.peer }
func (c *proxiedConn) LocalAddr() net.Addr        { return c.local }

// AcceptProxied reads the PROXY header of a new connection.
func AcceptProxied(c net.Conn) (*proxiedConn, error) {
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(c)
	src, dst, err := ReadProxyV2(r)
	_ = c.SetReadDeadline(time.Time{})
	if err != nil {
		return nil, err
	}
	return &proxiedConn{Conn: c, r: r, local: dst, peer: src}, nil
}

// ProxiedListener accepts from ln only connections that start with a
// PROXY v2 header, which each reads in its own goroutine, and closes every
// other; the connections it returns have the header's addresses.
func ProxiedListener(ln net.Listener) net.Listener {
	p := &proxiedListener{Listener: ln, conns: make(chan net.Conn), done: make(chan struct{})}
	go p.run()
	return p
}

type proxiedListener struct {
	net.Listener
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
	err   error // the Accept error, if it closed done
}

func (p *proxiedListener) run() {
	for {
		c, err := p.Listener.Accept()
		if err != nil {
			p.once.Do(func() { p.err = err; close(p.done) })
			return
		}
		go func() {
			pc, err := AcceptProxied(c)
			if err != nil {
				c.Close()
				return
			}
			select {
			case p.conns <- pc:
			case <-p.done:
				c.Close()
			}
		}()
	}
}

func (p *proxiedListener) Accept() (net.Conn, error) {
	select {
	case c := <-p.conns:
		return c, nil
	case <-p.done:
		if p.err != nil {
			return nil, p.err
		}
		return nil, net.ErrClosed
	}
}

func (p *proxiedListener) Close() error {
	err := p.Listener.Close()
	p.once.Do(func() { close(p.done) })
	return err
}
