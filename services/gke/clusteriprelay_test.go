package gke

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// echoServer accepts connections on 127.0.0.1 and echoes them.
func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String()
}

// The node's ClusterIP relay (ADR 0014) connects the emulator to a ClusterIP
// in the Service range it was given, and only with the cluster's secret.
func TestClusterIPRelay(t *testing.T) {
	target := echoServer(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, cidr, _ := net.ParseCIDR("127.0.0.0/8")
	go serveClusterIPRelay(ln, "secret", cidr)
	t.Cleanup(func() { ln.Close() })
	var d net.Dialer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := dialClusterIPRelay(ctx, d.DialContext, ln.Addr().String(), "secret", target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(c, "ping"); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 4)
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "ping" {
		t.Errorf("echo = %q, %v", b, err)
	}
	c.Close()

	if _, err := dialClusterIPRelay(ctx, d.DialContext, ln.Addr().String(), "wrong", target); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("wrong secret: %v", err)
	}
	if _, err := dialClusterIPRelay(ctx, d.DialContext, ln.Addr().String(), "secret", "10.0.0.1:80"); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Errorf("outside the Service range: %v", err)
	}
}
