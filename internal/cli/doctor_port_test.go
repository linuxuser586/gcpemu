package cli

import (
	"errors"
	"net"
	"runtime"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/loopback"
)

// checkPort reports a named port free on both families, fails one held on
// either, and warns when another process answers on it even though the
// binds succeed: it holds the port on all interfaces, as AirPlay Receiver
// does for 5000 and 7000 on macOS (#641). A port in LocalStack's range is
// named as such (#645).
func TestCheckPort(t *testing.T) {
	inUse := errors.New("bind: address already in use")
	for _, tc := range []struct {
		name      string
		goos      string
		port      int
		probe     portProbe
		fail      bool
		want, not []string
	}{
		{name: "free", goos: "darwin", port: 4561, probe: portProbe{addr: "127.0.0.1:4561", v6: true},
			want: []string{"[ok  ] port 127.0.0.1:4561 (ar) free", "[::1]:4561"}, not: []string{"WARN"}},
		{name: "no IPv6 loopback", goos: "linux", port: 4561, probe: portProbe{addr: "127.0.0.1:4561"},
			want: []string{"[ok  ] port 127.0.0.1:4561 (ar) free"}, not: []string{"[::1]", "WARN"}},
		{name: "IPv4 held", goos: "linux", port: 4561, probe: portProbe{addr: "127.0.0.1:4561", err: inUse}, fail: true,
			want: []string{"[FAIL] port 127.0.0.1:4561 (ar) free", "--port ar="}},
		{name: "IPv6 held", goos: "linux", port: 4561, probe: portProbe{addr: "127.0.0.1:4561", v6: true, v6Err: inUse}, fail: true,
			want: []string{"[FAIL] port [::1]:4561 (ar) free", "IPv6", "--port ar="}},
		{name: "wildcard on macOS 5000", goos: "darwin", port: 5000, probe: portProbe{addr: "127.0.0.1:5000", v6: true, answered: true},
			want: []string{"[WARN] port 5000 (ar)", "all interfaces", "take precedence", "AirPlay Receiver", "AirDrop & Handoff"}},
		{name: "wildcard on macOS 7000", goos: "darwin", port: 7000, probe: portProbe{addr: "127.0.0.1:7000", v6: true, answered: true},
			want: []string{"[WARN] port 7000 (ar)", "AirPlay Receiver"}},
		{name: "wildcard on macOS elsewhere", goos: "darwin", port: 4561, probe: portProbe{addr: "127.0.0.1:4561", v6: true, answered: true},
			want: []string{"[WARN] port 4561 (ar)"}, not: []string{"AirPlay"}},
		{name: "IPv4 held in LocalStack's range", goos: "darwin", port: 4510, probe: portProbe{addr: "127.0.0.1:4510", err: inUse}, fail: true,
			want: []string{"[FAIL] port 127.0.0.1:4510 (ar) free", "LocalStack", "--port ar="}},
		{name: "IPv6 held on LocalStack's edge port", goos: "linux", port: 4566, probe: portProbe{addr: "127.0.0.1:4566", v6: true, v6Err: inUse}, fail: true,
			want: []string{"[FAIL] port [::1]:4566 (ar) free on IPv6", "LocalStack", "--port ar="}},
		{name: "wildcard in LocalStack's range", goos: "linux", port: 4559, probe: portProbe{addr: "127.0.0.1:4559", v6: true, answered: true},
			want: []string{"[WARN] port 4559 (ar)", "LocalStack", "--port ar="}, not: []string{"AirPlay"}},
		{name: "just past LocalStack's range", goos: "darwin", port: 4560, probe: portProbe{addr: "127.0.0.1:4560", err: inUse}, fail: true,
			want: []string{"[FAIL] port 127.0.0.1:4560 (ar) free"}, not: []string{"LocalStack"}},
		{name: "held outside LocalStack's range", goos: "darwin", port: 4443, probe: portProbe{addr: "127.0.0.1:4443", err: inUse}, fail: true,
			want: []string{"[FAIL] port 127.0.0.1:4443 (ar) free"}, not: []string{"LocalStack"}},
		{name: "wildcard outside LocalStack's range", goos: "darwin", port: 4443, probe: portProbe{addr: "127.0.0.1:4443", v6: true, answered: true},
			want: []string{"[WARN] port 4443 (ar)"}, not: []string{"LocalStack"}},
		{name: "wildcard on Windows 5000", goos: "windows", port: 5000, probe: portProbe{addr: "127.0.0.1:5000", v6: true, answered: true},
			want: []string{"[WARN] port 5000 (ar)"}, not: []string{"AirPlay"}},
	} {
		var out strings.Builder
		failed := false
		report := func(ok bool, what, fix string) {
			mark := "ok  "
			if !ok {
				mark, failed = "FAIL", true
			}
			out.WriteString("[" + mark + "] " + what + "\n")
			if !ok {
				out.WriteString("       fix: " + fix + "\n")
			}
		}
		checkPort(&out, report, tc.goos, "ar", tc.port, tc.probe)
		if failed != tc.fail {
			t.Errorf("%s: failed = %v, want %v:\n%s", tc.name, failed, tc.fail, out.String())
		}
		for _, w := range tc.want {
			if !strings.Contains(out.String(), w) {
				t.Errorf("%s: output lacks %q:\n%s", tc.name, w, out.String())
			}
		}
		for _, n := range tc.not {
			if strings.Contains(out.String(), n) {
				t.Errorf("%s: output has %q:\n%s", tc.name, n, out.String())
			}
		}
	}
}

// probePort beside another process's listener on all interfaces, as each
// OS treats it (#641): Linux refuses the specific bind, so doctor fails the
// port; macOS allows it, so doctor sees the port answer and warns. Windows
// is to be confirmed, so its outcome is only logged, but it must be one of
// the two.
func TestProbePortWildcard(t *testing.T) {
	free := probePort("127.0.0.1", freePort(t))
	if free.err != nil || free.v6Err != nil || free.answered {
		t.Fatalf("free port: %+v, want free and silent", free)
	}
	w, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	go func() {
		for {
			c, err := w.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	p := probePort("127.0.0.1", w.Addr().(*net.TCPAddr).Port)
	switch runtime.GOOS {
	case "linux":
		if !loopback.AddrInUse(p.err) {
			t.Errorf("Linux: bind beside a wildcard listener = %v, want address in use", p.err)
		}
	case "darwin":
		if p.err != nil || p.v6Err != nil || !p.answered {
			t.Errorf("macOS: %+v, want both binds to succeed and the port to answer", p)
		}
	default:
		t.Logf("%s: bind beside a wildcard listener: %+v", runtime.GOOS, p)
		if p.err == nil && p.v6Err == nil && !p.answered {
			t.Errorf("%s: binds succeeded but the port did not answer: %+v", runtime.GOOS, p)
		}
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
