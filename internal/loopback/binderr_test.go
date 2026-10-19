package loopback

import (
	"errors"
	"net"
	"os"
	"strings"
	"testing"
)

func wrapBind(errno error) error {
	return &net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", errno)}
}

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
	if r := Refused(err); r != NotRefused {
		t.Errorf("Refused(%v) = %v, want NotRefused", err, r)
	}
}

func TestBindErrnos(t *testing.T) {
	for _, errno := range addrInUseErrnos {
		if err := wrapBind(errno); !AddrInUse(err) || Refused(err) != NotRefused {
			t.Errorf("%v: AddrInUse = %v, Refused = %v; want true, NotRefused", err, AddrInUse(err), Refused(err))
		}
	}
	for _, errno := range bindDeniedErrnos {
		if err := wrapBind(errno); Refused(err) == NotRefused || AddrInUse(err) {
			t.Errorf("%v: Refused = %v, AddrInUse = %v; want refused, false", err, Refused(err), AddrInUse(err))
		}
	}
	for _, err := range []error{nil, errors.New("bind: address already in use"), wrapBind(errors.New("other"))} {
		if AddrInUse(err) || Refused(err) != NotRefused {
			t.Errorf("%v: AddrInUse = %v, Refused = %v; want false, NotRefused", err, AddrInUse(err), Refused(err))
		}
	}
}

// TestRefusedOn: a bind refused for permission is a privileged port on
// Linux, one macOS binds on the wildcard address, and on Windows, which
// has no privileged ports, a port in an excluded range (ADR 0015).
func TestRefusedOn(t *testing.T) {
	denied := wrapBind(bindDeniedErrnos[0])
	for goos, want := range map[string]Refusal{
		"linux":   Privileged,
		"freebsd": Privileged,
		"darwin":  PrivilegedWildcard,
		"windows": Excluded,
	} {
		if got := RefusedOn(goos, denied); got != want {
			t.Errorf("RefusedOn(%s, %v) = %v, want %v", goos, denied, got, want)
		}
		if got := RefusedOn(goos, wrapBind(addrInUseErrnos[0])); got != NotRefused {
			t.Errorf("RefusedOn(%s, address in use) = %v, want NotRefused", goos, got)
		}
	}
}

func TestExcludedError(t *testing.T) {
	err := ExcludedError(wrapBind(bindDeniedErrnos[0]), "udp")
	if RefusedOn("windows", err) != Excluded {
		t.Errorf("ExcludedError does not wrap the bind error: %v", err)
	}
	const want = "netsh int ipv4 show excludedportrange protocol=udp"
	if got := err.Error(); !strings.Contains(got, want) {
		t.Errorf("ExcludedError = %q, want it to name %q", got, want)
	}
}
