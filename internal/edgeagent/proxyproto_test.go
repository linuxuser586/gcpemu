package edgeagent

import (
	"bufio"
	"bytes"
	"net"
	"testing"
)

func TestProxyProtocol(t *testing.T) {
	var buf bytes.Buffer
	src := &net.TCPAddr{IP: net.ParseIP("203.0.113.7"), Port: 51234}
	dst := &net.TCPAddr{IP: net.ParseIP("172.30.0.250"), Port: 443}
	if err := WriteProxyV2(&buf, src, dst); err != nil {
		t.Fatal(err)
	}
	buf.WriteString("GET / HTTP/1.1\r\n")
	s, d, err := ReadProxyV2(bufio.NewReader(&buf))
	if err != nil || s.String() != src.String() || d.String() != dst.String() {
		t.Fatalf("parsed %v %v %v", s, d, err)
	}
	if _, _, err := ReadProxyV2(bufio.NewReader(bytes.NewBufferString("GET / HTTP/1.1\r\n\r\n"))); err == nil {
		t.Error("plain HTTP accepted as PROXY header")
	}
}
