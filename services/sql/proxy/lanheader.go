package proxy

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"net"

	"github.com/linuxuser586/gcpemu/internal/edgeagent"
)

// The --sql-bind Postgres relay writes a PROXY v2 header ahead of a LAN
// caller's bytes, naming the caller's address and the address it dialled,
// with an HMAC-SHA256 of both keyed by the instance's agent key (EnvKey)
// in a TLV of type lanMACType. The direct listener reports a verified
// header's source as LoginRequest.LANCaller and closes a connection whose
// header fails verification (ADR 0030).

// lanMACType is the TLV type of the header's HMAC, in the range PROXY v2
// leaves to applications.
const lanMACType = 0xE0

// WriteLANHeader writes the signed PROXY v2 header for a LAN caller src
// that dialled dst.
func WriteLANHeader(w io.Writer, key string, src, dst *net.TCPAddr) error {
	return edgeagent.WriteProxyV2(w, src, dst, edgeagent.TLV{Type: lanMACType, Value: lanMAC(key, src, dst)})
}

// lanMAC is the HMAC of src and dst, each as its 16-byte IP and port.
func lanMAC(key string, src, dst *net.TCPAddr) []byte {
	m := hmac.New(sha256.New, []byte(key))
	for _, a := range []*net.TCPAddr{src, dst} {
		m.Write(a.IP.To16())
		m.Write(binary.BigEndian.AppendUint16(nil, uint16(a.Port)))
	}
	return m.Sum(nil)
}

// proxyV2First is the first byte of a PROXY v2 header; a Postgres startup
// message starts with its length's high byte, 0.
const proxyV2First = 0x0D

// readLANHeader reads c's signed header, if it starts with one, and
// returns the connection to read the client from after it and the LAN
// caller's address ("" without a header).
func readLANHeader(c net.Conn, key string) (net.Conn, string, error) {
	r := bufio.NewReader(c)
	b, err := r.Peek(1)
	if err != nil {
		return nil, "", err
	}
	bc := &bufferedConn{Conn: c, r: r}
	if b[0] != proxyV2First {
		return bc, "", nil
	}
	src, dst, tlvs, err := edgeagent.ReadProxyV2TLVs(r)
	if err != nil {
		return nil, "", err
	}
	if key != "" {
		for _, t := range tlvs {
			if t.Type == lanMACType && hmac.Equal(t.Value, lanMAC(key, src, dst)) {
				return bc, hostOf(src), nil
			}
		}
	}
	return nil, "", fmt.Errorf("PROXY header from %s fails verification", hostOf(c.RemoteAddr()))
}

// bufferedConn is c read through r, which may hold bytes already read.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// CloseWrite half-closes the connection, as splice expects.
func (c *bufferedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Conn.Close()
}
