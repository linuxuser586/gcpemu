package proxy

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func startupMsg(params ...string) []byte {
	body := []byte{0, 3, 0, 0}
	for _, p := range params {
		body = append(body, p...)
		body = append(body, 0)
	}
	body = append(body, 0)
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, uint32(4+len(body)))
	return append(out, body...)
}

func TestReadStartup(t *testing.T) {
	r := bytes.NewReader(append(append([]byte{}, SSLRequest...), startupMsg("user", "bob", "database", "app", "application_name", "x")...))
	m, err := readStartup(r)
	if err != nil || m.code != sslRequestCode || m.params != nil {
		t.Fatalf("SSLRequest: %+v %v", m, err)
	}
	m, err = readStartup(r)
	if err != nil || m.params["user"] != "bob" || m.params["database"] != "app" || m.params["application_name"] != "x" {
		t.Fatalf("startup: %+v %v", m, err)
	}
	if _, err := readStartup(bytes.NewReader([]byte{0, 0, 0, 2, 0, 0, 0, 0})); err == nil {
		t.Error("invalid length accepted")
	}
}

func TestMessages(t *testing.T) {
	e := errorResponse("FATAL", "28000", "nope")
	if e[0] != 'E' || int(binary.BigEndian.Uint32(e[1:5])) != len(e)-1 || !bytes.Contains(e, []byte("C28000\x00Mnope\x00")) || e[len(e)-1] != 0 {
		t.Fatalf("errorResponse = %q", e)
	}
	if a := authCleartext(); !bytes.Equal(a, []byte{'R', 0, 0, 0, 8, 0, 0, 0, 3}) {
		t.Fatalf("authCleartext = %v", a)
	}
	pw := append([]byte{'p', 0, 0, 0, 9}, "tok1\x00"...)
	if got, err := readPassword(bytes.NewReader(pw)); err != nil || got != "tok1" {
		t.Fatalf("readPassword = %q %v", got, err)
	}
	if _, err := readPassword(bytes.NewReader([]byte{'Q', 0, 0, 0, 4})); err == nil {
		t.Error("non-password message accepted")
	}
}
