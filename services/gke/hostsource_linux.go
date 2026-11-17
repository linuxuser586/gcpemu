package gke

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// The node side of ADR 0087: the ClusterIP relay connects to a pod on this
// node from the Host address, which the node does not hold, so that the
// pod sees the host. The socket is transparent (IP_TRANSPARENT) and marked
// (srcSocketMark); mangle OUTPUT copies that mark onto the connection as
// srcConnMark, mangle PREROUTING copies it back onto the pod's replies, and
// a policy rule routes packets carrying it to a table where every address
// is local, so the replies reach the socket. Every other packet to the
// Host address, a pod reaching the host's Offered ports above all, is
// routed as before.

const (
	// srcSocketMark marks the relay's transparent sockets, and
	// srcConnMark their connections and the replies on them. They are
	// distinct so that the relay's own packets are not routed locally,
	// and clear of kube-proxy's (0x4000, 0x8000) and kube-router's
	// (0x10000, 0x20000) marks.
	srcSocketMark = 0x1000000
	srcConnMark   = 0x2000000
	// srcTable is the routing table, and srcRulePref the policy rule's
	// priority, of the replies.
	srcTable    = 15090
	srcRulePref = 15090
	// podBridge is the bridge flannel puts the node's pods on.
	podBridge = "cni0"
)

// sourceRelay makes r connect to pods from a source, or records why the
// node cannot.
func sourceRelay(r *clusterIPRelay) {
	r.localPod = localPod
	if err := sourceRouting(); err != nil {
		r.unpreserved = err.Error()
		return
	}
	r.dialFrom = dialFrom
}

// run runs a command of the node's network setup.
func run(name string, args ...string) error {
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// sourceRouting installs the marks and the policy rule of the replies,
// with an empty table that localSource fills.
func sourceRouting() error {
	const ipt = "/bin/aux/iptables"
	mask := func(m int) string { return fmt.Sprintf("0x%x/0x%x", m, m) }
	hex := func(m int) string { return fmt.Sprintf("0x%x", m) }
	for _, c := range []struct{ hook, chain string }{{"OUTPUT", "GCPEMU-SRC-OUT"}, {"PREROUTING", "GCPEMU-SRC-PRE"}} {
		_ = exec.Command(ipt, "-t", "mangle", "-N", c.chain).Run()
		if err := run(ipt, "-t", "mangle", "-F", c.chain); err != nil {
			return err
		}
		if exec.Command(ipt, "-t", "mangle", "-C", c.hook, "-j", c.chain).Run() != nil {
			if err := run(ipt, "-t", "mangle", "-I", c.hook, "-j", c.chain); err != nil {
				return err
			}
		}
	}
	if err := run(ipt, "-t", "mangle", "-A", "GCPEMU-SRC-OUT", "-m", "mark", "--mark", mask(srcSocketMark), "-j", "CONNMARK", "--or-mark", hex(srcConnMark)); err != nil {
		return err
	}
	if err := run(ipt, "-t", "mangle", "-A", "GCPEMU-SRC-PRE", "-m", "connmark", "--mark", mask(srcConnMark), "-j", "MARK", "--or-mark", hex(srcConnMark)); err != nil {
		return err
	}
	pref, table := strconv.Itoa(srcRulePref), strconv.Itoa(srcTable)
	for exec.Command("ip", "rule", "del", "pref", pref).Run() == nil { // every copy a restarted agent left
	}
	_ = exec.Command("ip", "route", "flush", "table", table).Run()
	return run("ip", "rule", "add", "pref", pref, "fwmark", mask(srcConnMark), "lookup", table)
}

// localSources are the sources localSource has made local.
var localSources sync.Map

// localSource makes src local for the replies on marked connections.
func localSource(src string) error {
	if _, ok := localSources.Load(src); ok {
		return nil
	}
	if err := run("ip", "route", "replace", "local", src+"/32", "dev", "lo", "table", strconv.Itoa(srcTable)); err != nil {
		return err
	}
	localSources.Store(src, true)
	return nil
}

// localPod reports whether ip is on the node's pod bridge, other than the
// bridge's own address.
func localPod(ip net.IP) bool {
	ifc, err := net.InterfaceByName(podBridge)
	if err != nil {
		return false
	}
	addrs, err := ifc.Addrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.Contains(ip) && !n.IP.Equal(ip) {
			return true
		}
	}
	return false
}

// dialFrom connects to target from src through a transparent, marked
// socket.
func dialFrom(ctx context.Context, src, target string) (net.Conn, error) {
	ip := net.ParseIP(src).To4()
	if ip == nil {
		return nil, fmt.Errorf("bad source %q", src)
	}
	if err := localSource(ip.String()); err != nil {
		return nil, err
	}
	d := net.Dialer{
		LocalAddr: &net.TCPAddr{IP: ip},
		Control: func(_, _ string, rc syscall.RawConn) error {
			var serr error
			if err := rc.Control(func(fd uintptr) {
				if serr = syscall.SetsockoptInt(int(fd), syscall.SOL_IP, syscall.IP_TRANSPARENT, 1); serr != nil {
					serr = fmt.Errorf("IP_TRANSPARENT: %w", serr)
					return
				}
				if serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_MARK, srcSocketMark); serr != nil {
					serr = fmt.Errorf("SO_MARK: %w", serr)
				}
			}); err != nil {
				return err
			}
			return serr
		},
	}
	return d.DialContext(ctx, "tcp4", target)
}
