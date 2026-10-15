package gke

import (
	"bufio"
	"context"
	"errors"
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

// clusterIPRelayPort is the node's ClusterIP relay port; tests change it.
var clusterIPRelayPort = 15090

// serveClusterIPRelay serves the relay on ln until it closes, for targets in
// cidr.
func serveClusterIPRelay(ln net.Listener, secret string, cidr *net.IPNet) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
			br := bufio.NewReader(c)
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			_ = c.SetReadDeadline(time.Time{})
			tok, target, _ := strings.Cut(strings.TrimSpace(line), " ")
			if tok != secret {
				_, _ = io.WriteString(c, "ERR forbidden\n")
				return
			}
			host, _, err := net.SplitHostPort(target)
			if ip := net.ParseIP(host); err != nil || ip == nil || cidr == nil || !cidr.Contains(ip) {
				_, _ = io.WriteString(c, "ERR "+target+" is outside the Service range\n")
				return
			}
			u, err := net.DialTimeout("tcp", target, 10*time.Second)
			if err != nil {
				_, _ = io.WriteString(c, "ERR "+err.Error()+"\n")
				return
			}
			defer u.Close()
			if _, err := io.WriteString(c, "OK\n"); err != nil {
				return
			}
			if n := br.Buffered(); n > 0 {
				b, _ := br.Peek(n)
				if _, err := u.Write(b); err != nil {
					return
				}
			}
			netplane.Splice(c, u)
		}()
	}
}

// dialClusterIPRelay connects to target, a ClusterIP and port, through the
// ClusterIP relay at relay, which dial reaches.
func dialClusterIPRelay(ctx context.Context, dial func(ctx context.Context, network, addr string) (net.Conn, error), relay, secret, target string) (net.Conn, error) {
	c, err := dial(ctx, "tcp", relay)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	if _, err := io.WriteString(c, secret+" "+target+"\n"); err != nil {
		c.Close()
		return nil, err
	}
	// Read the status line byte by byte so no Service data is consumed.
	var line []byte
	b := make([]byte, 1)
	for len(line) < 512 {
		if _, err := c.Read(b); err != nil {
			c.Close()
			return nil, err
		}
		if b[0] == '\n' {
			break
		}
		line = append(line, b[0])
	}
	_ = c.SetDeadline(time.Time{})
	if s := string(line); s != "OK" {
		c.Close()
		return nil, errors.New("ClusterIP relay: " + strings.TrimPrefix(s, "ERR "))
	}
	return c, nil
}
