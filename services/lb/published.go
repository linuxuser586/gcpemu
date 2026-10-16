package lb

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"time"

	"github.com/linuxuser586/gcpemu/internal/loopback"
	"github.com/linuxuser586/gcpemu/internal/published"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// Published ports (ADR 0012, #289). In a Containerized Instance a
// forwarding rule's loopback listener is the container's, and `-p`
// forwards to the container's network interface, never to its loopback,
// so the host reaches no listener above. Each rule is therefore also
// served on a port its container publishes, on the wildcard address of
// the container, beside its loopback listener (which clients inside the
// container and the emulated DNS keep using), much as --lb-bind serves it
// on more addresses. Its endpoint "lb:<name>" is the host side of that
// binding instead of the loopback listener.
//
// The rule's own port comes first, when the container publishes it at any
// host port (GCPEMU_LB_<NAME> carries the port), then a free port of
// --port-range from a hash of the rule onwards (published.AnyHostPort).
// GKE LoadBalancer Services take ports from the same range: whoever binds
// a port first wins. A rule with none free has no endpoint, logs what to
// publish, and is retried each second and whenever the rules change.
//
// Without the runtime socket the Instance cannot read its published
// ports. A rule then listens on its own port and reports 127.0.0.1 there,
// which is right when the port is published at the same host port; the
// first rule on each port wins.
//
// The loopback and published listeners share their port on Linux only
// when both set SO_REUSEPORT (published.ListenShared), which would also
// let two rules share a published port, so the data plane keeps track of
// the ports its rules hold. An Instance on the host is unchanged.

// publishedHost is the address published ports are served on in the
// container; "" is the wildcard. Tests change it.
var publishedHost = ""

// publishing is how a forwarding rule reaches the Instance's host.
type publishing struct {
	// on: a Containerized Instance, which serves rules on Published ports.
	on bool
	// noSocket: the Instance has no runtime socket to read its published
	// ports with.
	noSocket bool
	// ports is the container's published ports; nil until read, or
	// without the runtime socket.
	ports *published.Ports
}

// pubRetryAfter is how long the data plane waits before asking the
// runtime again after it failed to read the container's published ports.
const pubRetryAfter = 30 * time.Second

// publishing returns how rules reach the host, learning it on first use.
// The container's published ports are fixed for its life. Callers hold
// d.mu, so after a failed read the runtime is not asked again for
// pubRetryAfter.
func (d *dataplane) publishing() publishing {
	if d.pubKnown {
		return d.pub
	}
	// A Containerized Instance whose ports are unknown for now: nothing
	// is served on them until a later try reads them.
	unknown := publishing{on: true}
	if time.Now().Before(d.pubRetry) {
		return unknown
	}
	p, err := d.selfPublishing(d.ctx)
	if err != nil {
		d.pubRetry = time.Now().Add(pubRetryAfter)
		d.s.env.Log.Warn("lb: cannot read the container's published ports; forwarding rules are not served on them until a later try", "err", err)
		return unknown
	}
	d.pub, d.pubKnown = p, true
	if p.on {
		d.s.env.Log.Info("lb: Containerized Instance: forwarding rules are also served on published ports", "published", p.ports.Len(), "runtimeSocket", !p.noSocket)
	}
	return p
}

// selfPublishing asks the runtime about the Instance's own container.
func (d *dataplane) selfPublishing(ctx context.Context) (publishing, error) {
	if !runtime.InContainer() {
		return publishing{}, nil
	}
	if !d.edge.available(ctx) {
		return publishing{on: true, noSocket: true}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rt, err := d.s.env.Containers.Runtime(ctx)
	if err != nil {
		return publishing{}, err
	}
	if rt.Self() == "" {
		// On the host network: as on the host.
		return publishing{}, nil
	}
	ports, err := rt.SelfPorts(ctx)
	if err != nil {
		return publishing{}, err
	}
	return publishing{on: true, ports: published.NewPorts(ports, d.s.env.Config)}, nil
}

// listenLoop opens a forwarding rule's loopback listener, sharing its port
// with a published listener in a Containerized Instance. Callers hold
// d.mu.
func (d *dataplane) listenLoop(addr string) (net.Listener, error) {
	if d.publishing().on && !d.single {
		return published.ListenShared(addr)
	}
	return net.Listen("tcp", addr)
}

// publish serves l on a Published port when the Instance is a
// Containerized Instance and l has none yet, and records the binding's
// host side as the rule's endpoint. With warn, a rule finding none free
// logs what to publish. Callers hold d.mu.
func (d *dataplane) publish(l *listener, fe *frontend, warn bool) {
	pub := d.publishing()
	if !pub.on || l.pub != nil {
		return
	}
	held := map[int]bool{}
	for _, o := range d.listeners {
		if o.pub != nil {
			held[o.pubPort] = true
		}
	}
	ports, cands := pub.ports, []int(nil)
	if pub.noSocket {
		ports = published.NewPorts(map[int]string{fe.port: net.JoinHostPort(loopback.Host, strconv.Itoa(fe.port))}, d.s.env.Config)
		cands = []int{fe.port}
		l.pubHint = fmt.Sprintf("publish the forwarding rule's port at the same host port (-p 127.0.0.1:%d:%d); without the container runtime socket only the first forwarding rule on a port is served", fe.port, fe.port)
	} else {
		if ports == nil {
			// Not read yet: none to serve on.
			ports = published.NewPorts(nil, d.s.env.Config)
		}
		cands = ports.Candidates(fe.frPath, fe.port, published.AnyHostPort)
		l.pubHint = ports.Hint(fe.port, "the forwarding rule")
	}
	listen := func(port int) (net.Listener, error) {
		return published.ListenShared(net.JoinHostPort(publishedHost, strconv.Itoa(port)))
	}
	if b := ports.Bind(cands, listen, func(port int) bool { return held[port] }); b != nil {
		l.pub, l.pubPort, l.pubHost, l.pubHint = b.Listener, b.Port, b.Host, ""
		go l.serve(b.Listener)
		d.s.env.Endpoints.Set("lb:"+fe.fr.Name, l.pubHost)
		d.s.env.Log.Info("lb: forwarding rule on a published port", "forwardingRule", fe.frPath, "port", fe.port, "containerPort", b.Port, "host", l.pubHost)
		return
	}
	if warn {
		d.s.env.Log.Warn("lb: no free published port for a forwarding rule; the host cannot reach it until one frees",
			"forwardingRule", fe.frPath, "port", fe.port, "fix", l.pubHint)
	}
}

// retryPublished serves the rules that have no Published port yet, in
// key order, on ports that have freed. Callers hold d.mu.
func (d *dataplane) retryPublished() {
	if !d.publishing().on {
		return
	}
	cfg := d.cfg.Load()
	keys := make([]string, 0, len(d.listeners))
	for k, l := range d.listeners {
		if l.pub == nil && cfg.fronts[k] != nil {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		d.publish(d.listeners[k], cfg.fronts[k], false)
	}
}

// publishLoop retries unserved rules while ctx lives: a port may free
// outside the data plane (a GKE LoadBalancer Service going).
func (d *dataplane) publishLoop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		d.mu.Lock()
		if !d.closed {
			d.retryPublished()
		}
		d.mu.Unlock()
	}
}
