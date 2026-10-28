package registryrelay

import (
	"io"
	"net"
	"testing"
)

// TestServePipesToTarget: each connection the relay accepts is piped both
// ways to the target, and the target's close reaches the client.
func TestServePipesToTarget(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go func() {
		for {
			c, err := backend.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				b := make([]byte, 4)
				if _, err := io.ReadFull(c, b); err == nil {
					_, _ = c.Write(append([]byte("got "), b...))
				}
			}()
		}
	}()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go serve(l, backend.Addr().String())

	for range 2 {
		c, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(c)
		c.Close()
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != "got ping" {
			t.Errorf("read %q; want %q", b, "got ping")
		}
	}
}
