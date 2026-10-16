// Package published chooses the Published ports (ADR 0012) a Containerized
// Instance serves host clients on: the ports its own container publishes
// to the host. GKE LoadBalancer Services (on a VM runtime) and forwarding
// rules (on any runtime) share them, and whoever binds a port first wins.
package published

import (
	"fmt"
	"hash/fnv"
	"net"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/netplane"
)

// Ports is what a Containerized Instance knows of its container's
// Published ports: the ports it publishes, and --port-range.
type Ports struct {
	// published maps each published container port to its host address
	// (runtime.Client.SelfPorts).
	published map[int]string
	// portRange is --port-range ("" for none); lo-hi when ranged.
	portRange string
	lo, hi    int
	ranged    bool
}

// NewPorts returns the Ports of a container that publishes published
// (runtime.Client.SelfPorts) under the Instance's config c.
func NewPorts(published map[int]string, c *config.Config) *Ports {
	lo, hi, ranged, _ := c.ParsePortRange()
	return &Ports{published: published, portRange: c.PortRange, lo: lo, hi: hi, ranged: ranged}
}

// Len is the number of ports the container publishes; 0 for nil.
func (p *Ports) Len() int {
	if p == nil {
		return 0
	}
	return len(p.published)
}

// OwnPortPolicy says when an Own port comes first among its candidates.
type OwnPortPolicy int

const (
	// SameHostPort: when the container publishes it at the same host
	// port (GKE LoadBalancer Services).
	SameHostPort OwnPortPolicy = iota
	// AnyHostPort: when the container publishes it at any host port
	// (forwarding rules).
	AnyHostPort
)

// Candidates lists the container ports to try serving port own on, in
// order: own itself when the container publishes it as policy says, then
// the published ports of --port-range, from a hash of name onwards, so
// that name keeps its port across syncs and restarts where it can.
func (p *Ports) Candidates(name string, own int, policy OwnPortPolicy) []int {
	var out []int
	if host, ok := p.published[own]; ok {
		if _, hp := netplane.HostPort(host); policy == AnyHostPort || hp == own {
			out = append(out, own)
		}
	}
	if !p.ranged {
		return out
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	n := p.hi - p.lo + 1
	start := int(h.Sum32() % uint32(n))
	for i := range n {
		c := p.lo + (start+i)%n
		if _, ok := p.published[c]; ok && (len(out) == 0 || out[0] != c) {
			out = append(out, c)
		}
	}
	return out
}

// Hint says what to publish so that port own of what ("the Service", "the
// forwarding rule") can be served.
func (p *Ports) Hint(own int, what string) string {
	flag := fmt.Sprintf("-p 127.0.0.1:%d:%d", own, own)
	if p.portRange == "" {
		return "start the emulator with --port-range LO-HI and publish that range (-p 127.0.0.1:LO-HI:LO-HI), or publish " + what + "'s port (" + flag + ")"
	}
	return "publish more of --port-range " + p.portRange + " (-p 127.0.0.1:" + p.portRange + ":" + p.portRange + "), or " + what + "'s port (" + flag + ")"
}

// Binding is a listener on a Published port.
type Binding struct {
	Listener net.Listener
	// Port is the container port, and Host its host address.
	Port int
	Host string
}

// Bind listens on the first of cands that skip (when set) does not report
// and listen binds, and returns that binding; nil when none binds.
func (p *Ports) Bind(cands []int, listen func(port int) (net.Listener, error), skip func(port int) bool) *Binding {
	for _, c := range cands {
		if skip != nil && skip(c) {
			continue
		}
		if ln, err := listen(c); err == nil {
			return &Binding{Listener: ln, Port: c, Host: p.published[c]}
		}
	}
	return nil
}
