package loopback

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
	if err := wrap(windows.WSAEADDRINUSE); !AddrInUse(err) {
		t.Errorf("AddrInUse(%v) = false, want true", err)
	}
	if err := wrap(windows.WSAEACCES); !BindDenied(err) {
		t.Errorf("BindDenied(%v) = false, want true", err)
	}
}
