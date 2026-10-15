package published

import (
	"net"
	"slices"
	"strings"
	"testing"
)

// A port is served on a port the Instance's container publishes (ADR
// 0012): its own port when it is published at the same host port, then
// the published ports of --port-range from a hash of the name onwards;
// none when nothing is published.
func TestCandidates(t *testing.T) {
	const name = "p/us-central1-a/c1/default/web/8081"
	published := map[int]string{
		8081: "127.0.0.1:8081", 9090: "127.0.0.1:19090", 4510: "127.0.0.1:4510",
		20000: "127.0.0.1:20000", 20001: "127.0.0.1:20001", 20002: "127.0.0.1:20002", 20003: "127.0.0.1:30003",
	}
	got := Candidates(name, 8081, published, 20000, 20004, true)
	if len(got) != 5 || got[0] != 8081 {
		t.Fatalf("candidates = %v, want 8081 then 20000-20003", got)
	}
	rest := slices.Clone(got[1:])
	slices.Sort(rest)
	if !slices.Equal(rest, []int{20000, 20001, 20002, 20003}) {
		t.Errorf("range candidates = %v, want the published 20000-20003", got[1:])
	}
	if again := Candidates(name, 8081, published, 20000, 20004, true); !slices.Equal(again, got) {
		t.Errorf("candidates not stable: %v then %v", got, again)
	}

	// A port published at another host port is not its own.
	if got := Candidates(name, 9090, published, 20000, 20004, true); slices.Contains(got, 9090) || len(got) != 4 {
		t.Errorf("port 9090 published as 19090: candidates = %v, want the range only", got)
	}
	// A port inside the range comes first, and once.
	if got := Candidates(name, 20002, published, 20000, 20004, true); got[0] != 20002 || len(got) != 4 {
		t.Errorf("port in range: candidates = %v", got)
	}
	// Without --port-range only the port itself.
	if got := Candidates(name, 8081, published, 0, 0, false); !slices.Equal(got, []int{8081}) {
		t.Errorf("no range: candidates = %v, want [8081]", got)
	}
	if got := Candidates(name, 8082, published, 0, 0, false); len(got) != 0 {
		t.Errorf("no range, port unpublished: candidates = %v, want none", got)
	}
	// A range that is not published gives nothing.
	if got := Candidates(name, 8082, published, 21000, 21999, true); len(got) != 0 {
		t.Errorf("range unpublished: candidates = %v, want none", got)
	}
}

// The hint names --port-range when there is none, the range when there is
// one, and the port at the same host port either way.
func TestHint(t *testing.T) {
	if h := Hint("", 8080, "the forwarding rule"); !strings.Contains(h, "--port-range LO-HI") || !strings.Contains(h, "the forwarding rule's port (-p 127.0.0.1:8080:8080)") {
		t.Errorf("no range: %q", h)
	}
	if h := Hint("20000-20019", 8443, "the Service"); !strings.Contains(h, "-p 127.0.0.1:20000-20019:20000-20019") || !strings.Contains(h, "the Service's port (-p 127.0.0.1:8443:8443)") {
		t.Errorf("range: %q", h)
	}
}

// Two shared listeners on one port, one on a loopback address and one on
// the wildcard address, both bind, and a client of the loopback address
// reaches its own listener.
func TestListenShared(t *testing.T) {
	lo, err := ListenShared("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lo.Close()
	_, port, _ := net.SplitHostPort(lo.Addr().String())
	wild, err := ListenShared(net.JoinHostPort("", port))
	if err != nil {
		t.Fatalf("wildcard beside 127.0.0.1:%s: %v", port, err)
	}
	defer wild.Close()
	go func() {
		if c, err := wild.Accept(); err == nil {
			t.Error("the wildcard listener took a loopback client")
			c.Close()
		}
	}()
	c, err := net.Dial("tcp", lo.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	got, err := lo.Accept()
	if err != nil {
		t.Fatal(err)
	}
	got.Close()
}
