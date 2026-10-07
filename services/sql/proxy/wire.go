package proxy

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// PostgreSQL frontend/backend protocol constants (protocol 3.x).
const (
	sslRequestCode    = 80877103
	gssEncRequestCode = 80877104
	cancelRequestCode = 80877102
	maxStartupLen     = 10000
)

// SSLRequest is the 8-byte SSLRequest packet (used by readiness probes).
var SSLRequest = []byte{0, 0, 0, 8, 0x04, 0xd2, 0x16, 0x2f}

// startup is a parsed untyped startup-phase packet.
type startup struct {
	raw    []byte // the full packet including the length prefix
	code   uint32 // protocol version or request code
	params map[string]string
}

// readStartup reads one startup-phase packet (StartupMessage, SSLRequest,
// GSSENCRequest or CancelRequest).
func readStartup(r io.Reader) (*startup, error) {
	var hdr [8]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:4])
	if n < 8 || n > maxStartupLen {
		return nil, fmt.Errorf("invalid startup packet length %d", n)
	}
	raw := make([]byte, n)
	copy(raw, hdr[:])
	if _, err := io.ReadFull(r, raw[8:]); err != nil {
		return nil, err
	}
	st := &startup{raw: raw, code: binary.BigEndian.Uint32(hdr[4:])}
	if st.code>>16 == 3 { // protocol 3.x StartupMessage
		st.params = map[string]string{}
		fields := bytes.Split(raw[8:], []byte{0})
		for i := 0; i+1 < len(fields); i += 2 {
			if len(fields[i]) == 0 {
				break
			}
			st.params[string(fields[i])] = string(fields[i+1])
		}
	}
	return st, nil
}

// errorResponse builds a backend ErrorResponse message.
func errorResponse(severity, code, msg string) []byte {
	var b bytes.Buffer
	for _, f := range []struct {
		t byte
		v string
	}{{'S', severity}, {'V', severity}, {'C', code}, {'M', msg}} {
		b.WriteByte(f.t)
		b.WriteString(f.v)
		b.WriteByte(0)
	}
	b.WriteByte(0)
	return message('E', b.Bytes())
}

// message frames a typed backend message.
func message(t byte, body []byte) []byte {
	out := make([]byte, 5+len(body))
	out[0] = t
	binary.BigEndian.PutUint32(out[1:5], uint32(4+len(body)))
	copy(out[5:], body)
	return out
}

// authCleartext is AuthenticationCleartextPassword.
func authCleartext() []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], 3)
	return message('R', b[:])
}

// readPassword reads a PasswordMessage ('p').
func readPassword(r io.Reader) (string, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return "", err
	}
	if hdr[0] != 'p' {
		return "", fmt.Errorf("expected password message, got %q", hdr[0])
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n < 4 || n > 1<<20 {
		return "", errors.New("invalid password message length")
	}
	body := make([]byte, n-4)
	if _, err := io.ReadFull(r, body); err != nil {
		return "", err
	}
	return string(bytes.TrimRight(body, "\x00")), nil
}
