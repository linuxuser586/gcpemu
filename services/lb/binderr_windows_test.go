package lb

import (
	"net"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWinsockBindErrnos(t *testing.T) {
	wrap := func(errno error) error {
		return &net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", errno)}
	}
	if err := wrap(windows.WSAEADDRINUSE); !addrInUse(err) {
		t.Errorf("addrInUse(%v) = false, want true", err)
	}
	if err := wrap(windows.WSAEACCES); !bindDenied(err) {
		t.Errorf("bindDenied(%v) = false, want true", err)
	}
}
