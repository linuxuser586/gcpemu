package lb

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
	if !addrInUse(err) {
		t.Errorf("addrInUse(%v) = false, want true", err)
	}
	if bindDenied(err) {
		t.Errorf("bindDenied(%v) = true, want false", err)
	}
}

func TestBindErrnos(t *testing.T) {
	wrap := func(errno error) error {
		return &net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", errno)}
	}
	for _, errno := range addrInUseErrnos {
		if err := wrap(errno); !addrInUse(err) || bindDenied(err) {
			t.Errorf("%v: addrInUse = %v, bindDenied = %v; want true, false", err, addrInUse(err), bindDenied(err))
		}
	}
	for _, errno := range bindDeniedErrnos {
		if err := wrap(errno); !bindDenied(err) || addrInUse(err) {
			t.Errorf("%v: bindDenied = %v, addrInUse = %v; want true, false", err, bindDenied(err), addrInUse(err))
		}
	}
	for _, err := range []error{nil, errors.New("bind: address already in use"), wrap(errors.New("other"))} {
		if addrInUse(err) || bindDenied(err) {
			t.Errorf("%v: addrInUse = %v, bindDenied = %v; want false, false", err, addrInUse(err), bindDenied(err))
		}
	}
}
