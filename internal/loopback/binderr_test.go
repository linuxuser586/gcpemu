package loopback

import (
	"errors"
	"net"
	"os"
	"testing"
)

func TestAddrInUse(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	ln, err := net.Listen("tcp", held.Addr().String())
	if err == nil {
		ln.Close()
		t.Fatalf("second bind of %s succeeded", held.Addr())
	}
	if !AddrInUse(err) {
		t.Errorf("AddrInUse(%v) = false, want true", err)
	}
	if BindDenied(err) {
		t.Errorf("BindDenied(%v) = true, want false", err)
	}
}

func TestBindErrnos(t *testing.T) {
	wrap := func(errno error) error {
		return &net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", errno)}
	}
	for _, errno := range addrInUseErrnos {
		if err := wrap(errno); !AddrInUse(err) || BindDenied(err) {
			t.Errorf("%v: AddrInUse = %v, BindDenied = %v; want true, false", err, AddrInUse(err), BindDenied(err))
		}
	}
	for _, errno := range bindDeniedErrnos {
		if err := wrap(errno); !BindDenied(err) || AddrInUse(err) {
			t.Errorf("%v: BindDenied = %v, AddrInUse = %v; want true, false", err, BindDenied(err), AddrInUse(err))
		}
	}
	for _, err := range []error{nil, errors.New("bind: address already in use"), wrap(errors.New("other"))} {
		if AddrInUse(err) || BindDenied(err) {
			t.Errorf("%v: AddrInUse = %v, BindDenied = %v; want false, false", err, AddrInUse(err), BindDenied(err))
		}
	}
}
