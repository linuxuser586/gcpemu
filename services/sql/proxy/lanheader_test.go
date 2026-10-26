package proxy

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/edgeagent"
)

// directAgent serves the direct listener of an agent whose emulator
// refuses every login and records it.
func directAgent(t *testing.T, key string) (string, <-chan LoginRequest) {
	t.Helper()
	logins := make(chan LoginRequest, 4)
	ctl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req LoginRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		logins <- req
		_ = json.NewEncoder(w).Encode(LoginResponse{Action: ActionDeny, Message: "no"})
	}))
	t.Cleanup(ctl.Close)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	a := &agent{control: ctl.URL, key: key, http: ctl.Client()}
	go a.serve(l, ListenerDirect)
	return l.Addr().String(), logins
}

// dialDirect sends header (if any) and a startup message, and returns the
// agent's first reply byte, or an error when it closes the connection.
func dialDirect(t *testing.T, addr string, header func(io.Writer) error) (byte, error) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if header != nil {
		if err := header(c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Write(startupMsg("user", "postgres", "database", "postgres")); err != nil {
		return 0, err
	}
	var b [1]byte
	_, err = io.ReadFull(c, b[:])
	return b[0], err
}

func TestDirectListenerLANHeader(t *testing.T) {
	addr, logins := directAgent(t, "agent-key")
	src := &net.TCPAddr{IP: net.ParseIP("192.168.1.7"), Port: 51234}
	dst := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 5432}

	// A header signed with the instance's key names the LAN caller.
	if b, err := dialDirect(t, addr, func(w io.Writer) error { return WriteLANHeader(w, "agent-key", src, dst) }); err != nil || b != 'E' {
		t.Fatalf("signed header: reply %q, %v", b, err)
	}
	if req := <-logins; req.LANClient != "192.168.1.7" || req.Remote != "127.0.0.1" {
		t.Errorf("signed header: login from LAN client %q, remote %q", req.LANClient, req.Remote)
	}

	// A wrong or missing signature closes the connection unasked.
	for name, header := range map[string]func(io.Writer) error{
		"wrong key": func(w io.Writer) error { return WriteLANHeader(w, "other-key", src, dst) },
		"no HMAC":   func(w io.Writer) error { return edgeagent.WriteProxyV2(w, src, dst) },
	} {
		if b, err := dialDirect(t, addr, header); err == nil {
			t.Errorf("%s: agent replied %q", name, b)
		}
	}

	// Without a header, as before.
	if b, err := dialDirect(t, addr, nil); err != nil || b != 'E' {
		t.Fatalf("no header: reply %q, %v", b, err)
	}
	if req := <-logins; req.LANClient != "" || req.Remote != "127.0.0.1" {
		t.Errorf("no header: login from LAN client %q, remote %q", req.LANClient, req.Remote)
	}
	select {
	case req := <-logins:
		t.Errorf("a refused header logged in: %+v", req)
	default:
	}
}
