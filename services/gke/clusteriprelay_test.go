package gke

import (
	"context"
	"errors"
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

// relayFixture serves r on 127.0.0.1 and returns its address.
func relayFixture(t *testing.T, r *clusterIPRelay) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go r.serve(ln)
	return ln.Addr().String()
}

// pingPong checks that c echoes, and closes it.
func pingPong(t *testing.T, c net.Conn) {
	t.Helper()
	defer c.Close()
	if _, err := io.WriteString(c, "ping"); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 4)
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "ping" {
		t.Errorf("echo = %q, %v", b, err)
	}
}

// The node's ClusterIP relay (ADR 0014) connects the emulator to a ClusterIP
// in the Service range it was given, and only with the cluster's secret.
func TestClusterIPRelay(t *testing.T) {
	target := echoServer(t)
	_, cidr, _ := net.ParseCIDR("127.0.0.0/8")
	relay := relayFixture(t, &clusterIPRelay{secret: "secret", services: cidr})
	var d net.Dialer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, unpreserved, err := dialClusterIPRelay(ctx, d.DialContext, relay, "secret", target, "")
	if err != nil {
		t.Fatal(err)
	}
	if unpreserved != "" {
		t.Errorf("no source asked, yet unpreserved: %q", unpreserved)
	}
	pingPong(t, c)

	if _, _, err := dialClusterIPRelay(ctx, d.DialContext, relay, "wrong", target, ""); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("wrong secret: %v", err)
	}
	if _, _, err := dialClusterIPRelay(ctx, d.DialContext, relay, "secret", "10.0.0.1:80", ""); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Errorf("outside the Service range: %v", err)
	}
}

// Asked for a source, the relay connects to a pod on its node from that
// address (ADR 0088). A node that cannot, from the start or on the dial,
// connects from its own address and says why.
func TestClusterIPRelaySource(t *testing.T) {
	target := echoServer(t)
	_, services, _ := net.ParseCIDR("10.8.0.0/16")
	local := func(ip net.IP) bool { return ip.IsLoopback() }
	var d net.Dialer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var from, to string
	transparent := &clusterIPRelay{secret: "secret", services: services, localPod: local,
		dialFrom: func(ctx context.Context, src, target string) (net.Conn, error) {
			from, to = src, target
			return d.DialContext(ctx, "tcp", target)
		}}
	relay := relayFixture(t, transparent)
	c, unpreserved, err := dialClusterIPRelay(ctx, d.DialContext, relay, "secret", target, "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if unpreserved != "" || from != "10.0.0.1" || to != target {
		t.Errorf("dialled %s from %s, unpreserved %q; want %s from 10.0.0.1", to, from, unpreserved, target)
	}
	pingPong(t, c)

	// Only a pod on this node: elsewhere, replies to the source would not
	// come back.
	if _, _, err := dialClusterIPRelay(ctx, d.DialContext, relay, "secret", "10.4.2.5:80", "10.0.0.1"); err == nil || !strings.Contains(err.Error(), "not a pod on this node") {
		t.Errorf("a pod elsewhere: %v", err)
	}
	if _, _, err := dialClusterIPRelay(ctx, d.DialContext, relay, "secret", target, "not-an-ip"); err == nil || !strings.Contains(err.Error(), "source") {
		t.Errorf("a bad source: %v", err)
	}

	unable := relayFixture(t, &clusterIPRelay{secret: "secret", services: services, localPod: local, unpreserved: "no xt_connmark"})
	c, unpreserved, err = dialClusterIPRelay(ctx, d.DialContext, unable, "secret", target, "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if unpreserved != "no xt_connmark" {
		t.Errorf("unpreserved = %q", unpreserved)
	}
	pingPong(t, c)

	failing := relayFixture(t, &clusterIPRelay{secret: "secret", services: services, localPod: local,
		dialFrom: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("bind: cannot assign requested address")
		}})
	c, unpreserved, err = dialClusterIPRelay(ctx, d.DialContext, failing, "secret", target, "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unpreserved, "cannot assign requested address") {
		t.Errorf("unpreserved = %q", unpreserved)
	}
	pingPong(t, c)
}
