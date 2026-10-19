package loopback

import (
	"net"
	goruntime "runtime"
	"strconv"
)

// Host listener rules (#172), shared by LB forwarding rules and GKE
// LoadBalancer Services on a VM runtime. A host client's address is a
// 127.0.0.x of its own when the host has aliases, and 127.0.0.1 otherwise.

// Host is the one loopback address every host has.
const Host = "127.0.0.1"

// DarwinPorts is set on macOS, which lets an unprivileged process bind a
// port below 1024 only on the wildcard address.
const DarwinPorts = goruntime.GOOS == "darwin"

// Listen binds ip:port for host clients. When the port is in a Windows
// excluded range, it fails. When the host refuses the port as privileged,
// it binds the wildcard address on the port on macOS,
// serving only connections to a loopback address, and otherwise (or when
// another socket holds the wildcard) a free port on ip. When another
// socket holds ip:port, it binds a free port on ip if inUse is set, and
// fails otherwise. addr is the address clients use: ip and the port
// served.
func Listen(ip string, port int, inUse bool) (ln net.Listener, addr string, err error) {
	ln, err = net.Listen("tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
	switch refused := Refused(err); {
	case err == nil:
		return ln, ln.Addr().String(), nil
	case refused == Excluded:
		return nil, "", ExcludedError(err, "tcp")
	case refused == PrivilegedWildcard:
		if wln, werr := net.Listen("tcp", net.JoinHostPort("0.0.0.0", strconv.Itoa(port))); werr == nil {
			return &LocalFilter{Listener: wln, Allow: IsLoopback}, net.JoinHostPort(ip, strconv.Itoa(port)), nil
		}
	case refused == Privileged:
	case !AddrInUse(err) || !inUse:
		return nil, "", err
	}
	if ln, err = net.Listen("tcp", net.JoinHostPort(ip, "0")); err != nil {
		return nil, "", err
	}
	return ln, ln.Addr().String(), nil
}

// IsLoopback reports whether a is a loopback TCP address.
func IsLoopback(a net.Addr) bool {
	ta, ok := a.(*net.TCPAddr)
	return ok && ta.IP.IsLoopback()
}

// LocalFilter serves only the connections whose local address Allow
// accepts, and closes the others unserved. It keeps a wildcard listener
// from serving clients on other hosts.
type LocalFilter struct {
	net.Listener
	Allow func(net.Addr) bool
}

func (f *LocalFilter) Accept() (net.Conn, error) {
	for {
		c, err := f.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if f.Allow(c.LocalAddr()) {
			return c, nil
		}
		c.Close()
	}
}
