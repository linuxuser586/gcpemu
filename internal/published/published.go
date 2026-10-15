// Package published chooses the Published ports (ADR 0012) a Containerized
// Instance serves host clients on: the ports its own container publishes
// to the host. GKE LoadBalancer Services (on a VM runtime) and forwarding
// rules (on any runtime) share them, and whoever binds a port first wins.
package published

import (
	"fmt"
	"hash/fnv"

	"github.com/linuxuser586/gcpemu/internal/netplane"
)

// Candidates lists the container ports to try serving port on, in order:
// port itself when the container publishes it at the same host port, then
// the published ports of --port-range lo-hi (when ranged), from a hash of
// name onwards, so that name keeps its port across syncs and restarts
// where it can. published maps each published container port to its host
// address (runtime.Client.SelfPorts).
func Candidates(name string, port int, published map[int]string, lo, hi int, ranged bool) []int {
	var out []int
	if _, hp := netplane.HostPort(published[port]); hp == port {
		out = append(out, port)
	}
	if !ranged {
		return out
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	n := hi - lo + 1
	start := int(h.Sum32() % uint32(n))
	for i := range n {
		p := lo + (start+i)%n
		if _, ok := published[p]; ok && (len(out) == 0 || out[0] != p) {
			out = append(out, p)
		}
	}
	return out
}

// Hint says what to publish so that port, the port of what ("the
// Service", "the forwarding rule"), can be served, given the Instance's
// --port-range ("" for none).
func Hint(portRange string, port int, what string) string {
	own := fmt.Sprintf("-p 127.0.0.1:%d:%d", port, port)
	if portRange == "" {
		return "start the emulator with --port-range LO-HI and publish that range (-p 127.0.0.1:LO-HI:LO-HI), or publish " + what + "'s port (" + own + ")"
	}
	return "publish more of --port-range " + portRange + " (-p 127.0.0.1:" + portRange + ":" + portRange + "), or " + what + "'s port (" + own + ")"
}
