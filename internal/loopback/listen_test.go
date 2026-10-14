package loopback

import (
	"net"
	"strconv"
	"testing"
)

func TestListen(t *testing.T) {
	held, err := net.Listen("tcp", Host+":0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	_, port, _ := net.SplitHostPort(held.Addr().String())
	p, _ := strconv.Atoi(port)

	if _, _, err := Listen(Host, p, false); !AddrInUse(err) {
		t.Errorf("held port without inUse: err = %v, want address in use", err)
	}
	ln, addr, err := Listen(Host, p, true)
	if err != nil {
		t.Fatalf("held port with inUse: %v", err)
	}
	ln.Close()
	if h, got, _ := net.SplitHostPort(addr); h != Host || got == port || got == "0" {
		t.Errorf("held port with inUse: addr = %s, want another port on %s", addr, Host)
	}

	held.Close()
	ln, addr, err = Listen(Host, p, false)
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	ln.Close()
	if addr != held.Addr().String() {
		t.Errorf("free port: addr = %s, want %s", addr, held.Addr())
	}
}

// A privileged port is served somewhere for host clients, as the address
// Listen reports.
func TestListenPrivileged(t *testing.T) {
	ln, addr, err := Listen(Host, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if _, ok := ln.(*LocalFilter); ok && addr != Host+":1" {
		t.Errorf("wildcard listener reported as %s, want %s:1", addr, Host)
	}
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.Close()
		}
	}()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	c.Close()
}
