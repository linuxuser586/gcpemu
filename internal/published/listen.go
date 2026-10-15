package published

import (
	"context"
	"net"
)

// ListenShared listens on addr like net.Listen, but lets a listener on the
// wildcard address and one on a specific address share a port (Linux
// refuses that otherwise), when both are opened with it. A forwarding
// rule's loopback listener and a Published port listener on the rule's
// own port need that in a Containerized Instance. Sharing also lets two
// wildcard listeners share a port, so callers must keep track of the
// ports they hold themselves; a plain listener (another process, or a
// GKE LoadBalancer Service) still refuses it.
func ListenShared(addr string) (net.Listener, error) {
	lc := net.ListenConfig{Control: reusePort}
	return lc.Listen(context.Background(), "tcp", addr)
}
