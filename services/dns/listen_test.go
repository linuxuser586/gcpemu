package dns

import (
	"context"
	"net"
	"testing"
)

// Start binds TCP and UDP on one dynamic port, even when ephemeral port
// picks run through a range the other protocol has reserved, as on
// Windows (#206).
func TestStartDynamicPort(t *testing.T) {
	ctx := context.Background()
	for range 30 {
		s := newTestService(t, true)
		s.env.Config.Bind = "127.0.0.1"
		s.env.Config.Ports = map[string]int{"dns": 0}
		if err := s.Start(ctx); err != nil {
			t.Fatal(err)
		}
		tp := s.tcp.Listener.Addr().(*net.TCPAddr).Port
		up := s.udp.PacketConn.LocalAddr().(*net.UDPAddr).Port
		if tp != up || s.env.Endpoints.Get("dns") != s.addr {
			t.Errorf("tcp port %d, udp port %d, endpoint %q, addr %q", tp, up, s.env.Endpoints.Get("dns"), s.addr)
		}
		if err := s.Stop(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

// listenPair leaves nothing bound when the port is taken.
func TestListenPairTaken(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	s := newTestService(t, true)
	s.env.Config.Bind = "127.0.0.1"
	port := l.Addr().(*net.TCPAddr).Port
	if _, _, err := s.listenPair(context.Background(), port); err == nil {
		t.Fatal("listenPair on a taken port succeeded")
	}
}
