package gke

import (
	"bufio"
	"context"
	"io"
	"net"
	"strings"
	"time"

	"github.com/linuxuser586/gcpemu/internal/netplane"
)

// The ClusterIP relay (ADR 0014) runs in the node agent on clusterIPRelayPort. It
// connects the emulator to a Service's ClusterIP, which only the cluster's
// nodes route (kube-proxy), so that host clients reach the Service at its
// Loopback alias (hostsvc.go). The emulator sends "SECRET ip:port\n", the
// cluster's secret and a ClusterIP in the Service range, and gets "OK\n"
// before the stream is spliced, or "ERR reason\n".
//
// So that pods see host clients come from the Host address (ADR 0088), the
// emulator can instead send "SECRET ip:port SOURCE\n", for a pod on this
// node and the Host address to connect from. The relay connects from
// SOURCE and answers "OK\n", or, when the node cannot, connects from its
// own address and answers "OK unpreserved REASON\n". An agent that
// predates the source answers ERR, since "ip:port SOURCE" is no address.

// clusterIPRelayPort is the node's ClusterIP relay port; tests change it.
var clusterIPRelayPort = 15090

// unpreservedPrefix starts the OK of a connection made from the node's own
// address instead of the source asked for.
const unpreservedPrefix = "OK unpreserved "

// clusterIPRelay is the node's ClusterIP relay.
type clusterIPRelay struct {
	secret string
	// services is the cluster's Service range.
	services *net.IPNet
	// localPod reports whether ip is a pod on this node, which alone can
	// be reached from a source: replies from a pod elsewhere would go to
	// the real Host address.
	localPod func(ip net.IP) bool
	// dialFrom connects to target from src, an address the node does not
	// hold, and returns replies to the connection; nil when unpreserved.
	dialFrom func(ctx context.Context, src, target string) (net.Conn, error)
	// unpreserved is why the node cannot connect from a source, if it
	// cannot.
	unpreserved string
}

// serve serves the relay on ln until it closes.
func (r *clusterIPRelay) serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go r.handle(c)
	}
}

// handle answers one request on c and splices it to its target.
func (r *clusterIPRelay) handle(c net.Conn) {
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil {
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != r.secret {
		_, _ = io.WriteString(c, "ERR forbidden\n")
		return
	}
	target, src := fields[1], ""
	if len(fields) == 3 {
		src = fields[2]
	} else if len(fields) > 3 {
		_, _ = io.WriteString(c, "ERR bad request\n")
		return
	}
	host, _, err := net.SplitHostPort(target)
	ip := net.ParseIP(host)
	if err != nil || ip == nil {
		_, _ = io.WriteString(c, "ERR "+target+" is no address\n")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var u net.Conn
	ok := "OK\n"
	switch {
	case src == "":
		if r.services == nil || !r.services.Contains(ip) {
			_, _ = io.WriteString(c, "ERR "+target+" is outside the Service range\n")
			return
		}
		u, err = (&net.Dialer{}).DialContext(ctx, "tcp", target)
	case net.ParseIP(src) == nil:
		_, _ = io.WriteString(c, "ERR bad source "+src+"\n")
		return
	case r.localPod == nil || !r.localPod(ip):
		_, _ = io.WriteString(c, "ERR "+target+" is not a pod on this node\n")
		return
	default:
		why := r.unpreserved
		if r.dialFrom != nil {
			if u, err = r.dialFrom(ctx, src, target); err != nil {
				why = err.Error()
			}
		} else if why == "" {
			why = "the node cannot connect from another address"
		}
		if u == nil {
			ok = unpreservedPrefix + strings.ReplaceAll(why, "\n", " ") + "\n"
			u, err = (&net.Dialer{}).DialContext(ctx, "tcp", target)
		}
	}
	if err != nil {
		_, _ = io.WriteString(c, "ERR "+strings.ReplaceAll(err.Error(), "\n", " ")+"\n")
		return
	}
	defer u.Close()
	if _, err := io.WriteString(c, ok); err != nil {
		return
	}
	if n := br.Buffered(); n > 0 {
		b, _ := br.Peek(n)
		if _, err := u.Write(b); err != nil {
			return
		}
	}
	netplane.Splice(c, u)
}

// relayRefused is the ERR a ClusterIP relay answered.
type relayRefused string

func (e relayRefused) Error() string { return "ClusterIP relay: " + string(e) }

// predatesSource reports whether the relay refused a source because its
// agent predates them: it takes "ip:port SOURCE" for a target outside the
// Service range, which a current agent never answers to a source.
func (e relayRefused) predatesSource() bool {
	return strings.HasSuffix(string(e), "is outside the Service range")
}

// dialClusterIPRelay connects to target through the ClusterIP relay at
// relay, which dial reaches: to a ClusterIP and port, or, from src, to a
// pod on the relay's node. unpreserved is why the relay connected from its
// node's address instead of src.
func dialClusterIPRelay(ctx context.Context, dial func(ctx context.Context, network, addr string) (net.Conn, error), relay, secret, target, src string) (c net.Conn, unpreserved string, err error) {
	c, err = dial(ctx, "tcp", relay)
	if err != nil {
		return nil, "", err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	req := secret + " " + target
	if src != "" {
		req += " " + src
	}
	if _, err := io.WriteString(c, req+"\n"); err != nil {
		c.Close()
		return nil, "", err
	}
	// Read the status line byte by byte so no Service data is consumed.
	var line []byte
	b := make([]byte, 1)
	for len(line) < 512 {
		if _, err := c.Read(b); err != nil {
			c.Close()
			return nil, "", err
		}
		if b[0] == '\n' {
			break
		}
		line = append(line, b[0])
	}
	_ = c.SetDeadline(time.Time{})
	s := string(line)
	if why, ok := strings.CutPrefix(s, unpreservedPrefix); ok {
		return c, why, nil
	}
	if s != "OK" {
		c.Close()
		return nil, "", relayRefused(strings.TrimPrefix(s, "ERR "))
	}
	return c, "", nil
}
