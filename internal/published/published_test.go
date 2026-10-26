package published

import (
	"errors"
	"net"
	"slices"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/config"
)

// A port is served on a port the Instance's container publishes (ADR
// 0012): its own port when it is published at the same host port, then
// the published ports of --port-range from a hash of the name onwards;
// none when nothing is published.
func TestCandidates(t *testing.T) {
	const name = "p/us-central1-a/c1/default/web/8081"
	published := map[int]string{
		8081: "127.0.0.1:8081", 9090: "127.0.0.1:19090", 4562: "127.0.0.1:4562",
		20000: "127.0.0.1:20000", 20001: "127.0.0.1:20001", 20002: "127.0.0.1:20002", 20003: "127.0.0.1:30003",
	}
	ports := NewPorts(published, &config.Config{PortRange: "20000-20004"})
	got := ports.Candidates(name, 8081, SameHostPort)
	if len(got) != 5 || got[0] != 8081 {
		t.Fatalf("candidates = %v, want 8081 then 20000-20003", got)
	}
	rest := slices.Clone(got[1:])
	slices.Sort(rest)
	if !slices.Equal(rest, []int{20000, 20001, 20002, 20003}) {
		t.Errorf("range candidates = %v, want the published 20000-20003", got[1:])
	}
	if again := ports.Candidates(name, 8081, SameHostPort); !slices.Equal(again, got) {
		t.Errorf("candidates not stable: %v then %v", got, again)
	}

	// A port published at another host port is not its own.
	if got := ports.Candidates(name, 9090, SameHostPort); slices.Contains(got, 9090) || len(got) != 4 {
		t.Errorf("port 9090 published as 19090: candidates = %v, want the range only", got)
	}
	// A port inside the range comes first, and once.
	if got := ports.Candidates(name, 20002, SameHostPort); got[0] != 20002 || len(got) != 4 {
		t.Errorf("port in range: candidates = %v", got)
	}
	// Any host port: a port published at another host port comes first,
	// and once.
	if got := ports.Candidates(name, 9090, AnyHostPort); len(got) != 5 || got[0] != 9090 || slices.Contains(got[1:], 9090) {
		t.Errorf("port 9090 published as 19090, any host port: candidates = %v, want 9090 then the range", got)
	}
	if got := ports.Candidates(name, 8082, AnyHostPort); slices.Contains(got, 8082) || len(got) != 4 {
		t.Errorf("port 8082 unpublished, any host port: candidates = %v, want the range only", got)
	}
	// Without --port-range only the port itself.
	none := NewPorts(published, &config.Config{})
	if got := none.Candidates(name, 8081, SameHostPort); !slices.Equal(got, []int{8081}) {
		t.Errorf("no range: candidates = %v, want [8081]", got)
	}
	if got := none.Candidates(name, 8082, SameHostPort); len(got) != 0 {
		t.Errorf("no range, port unpublished: candidates = %v, want none", got)
	}
	// A range that is not published gives nothing.
	if got := NewPorts(published, &config.Config{PortRange: "21000-21999"}).Candidates(name, 8082, SameHostPort); len(got) != 0 {
		t.Errorf("range unpublished: candidates = %v, want none", got)
	}
}

// The hint names --port-range when there is none, the range when there is
// one, and the port at the same host port either way.
func TestHint(t *testing.T) {
	if h := NewPorts(nil, &config.Config{}).Hint(8080, "the forwarding rule"); !strings.Contains(h, "--port-range LO-HI") || !strings.Contains(h, "the forwarding rule's port (-p 127.0.0.1:8080:8080)") {
		t.Errorf("no range: %q", h)
	}
	if h := NewPorts(nil, &config.Config{PortRange: "20000-20019"}).Hint(8443, "the Service"); !strings.Contains(h, "-p 127.0.0.1:20000-20019:20000-20019") || !strings.Contains(h, "the Service's port (-p 127.0.0.1:8443:8443)") {
		t.Errorf("range: %q", h)
	}
}

// Bind tries the candidates in order, passing over those skip reports and
// those listen fails on, and reports the first that binds with its host
// side; nil when none does.
func TestBind(t *testing.T) {
	ports := NewPorts(map[int]string{1: "127.0.0.1:10001", 2: "127.0.0.1:10002", 3: "127.0.0.1:10003", 4: "127.0.0.1:10004"}, &config.Config{})
	var tried []int
	listen := func(port int) (net.Listener, error) {
		tried = append(tried, port)
		if port == 2 {
			return nil, errors.New("in use")
		}
		return net.Listen("tcp", "127.0.0.1:0")
	}
	b := ports.Bind([]int{1, 2, 3, 4}, listen, func(port int) bool { return port == 1 })
	if b == nil {
		t.Fatal("nothing bound")
	}
	defer b.Listener.Close()
	if b.Port != 3 || b.Host != "127.0.0.1:10003" {
		t.Errorf("bound %d at %q, want 3 at 127.0.0.1:10003", b.Port, b.Host)
	}
	if !slices.Equal(tried, []int{2, 3}) {
		t.Errorf("tried %v, want [2 3]: 1 skipped, 2 failed, 3 bound", tried)
	}

	// Without skip every candidate is tried.
	tried = nil
	if b := ports.Bind([]int{2, 4}, listen, nil); b == nil || b.Port != 4 || b.Host != "127.0.0.1:10004" {
		t.Errorf("no skip: bound %+v, want 4", b)
	} else {
		b.Listener.Close()
	}
	if b := ports.Bind([]int{2}, listen, nil); b != nil {
		t.Errorf("all fail: bound %d, want nil", b.Port)
	}
	if b := ports.Bind(nil, listen, nil); b != nil {
		t.Errorf("no candidates: bound %d, want nil", b.Port)
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
