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

func TestProxyProtocolTLVs(t *testing.T) {
	for _, c := range []struct{ src, dst string }{
		{"192.168.1.7:51234", "127.0.0.1:5432"},
		{"[2001:db8::7]:51234", "[2001:db8::1]:5432"},
	} {
		src, _ := net.ResolveTCPAddr("tcp", c.src)
		dst, _ := net.ResolveTCPAddr("tcp", c.dst)
		var buf bytes.Buffer
		tlv := TLV{Type: 0xE0, Value: []byte("signature")}
		if err := WriteProxyV2(&buf, src, dst, tlv); err != nil {
			t.Fatal(err)
		}
		buf.WriteString("rest")
		r := bufio.NewReader(&buf)
		s, d, tlvs, err := ReadProxyV2TLVs(r)
		if err != nil || s.String() != c.src || d.String() != c.dst {
			t.Fatalf("%s: parsed %v %v %v", c.src, s, d, err)
		}
		if len(tlvs) != 1 || tlvs[0].Type != 0xE0 || string(tlvs[0].Value) != "signature" {
			t.Errorf("%s: TLVs %+v", c.src, tlvs)
		}
		if rest, _ := r.ReadString(0); rest != "rest" {
			t.Errorf("%s: after the header %q", c.src, rest)
		}
	}
}
