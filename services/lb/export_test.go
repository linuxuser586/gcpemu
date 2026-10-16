package lb

import (
	"context"
	"errors"
	"net"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/published"
)

// JoinSubnetwork starts the edge if needed and attaches it to subnetwork's
// container network as a pod route would, returning the edge's address on
// that network (exported to the external tests).
func JoinSubnetwork(ctx context.Context, svc emu.Service, subnetwork string) (string, error) {
	e := svc.(*Service).dp.edge
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.ensureLocked(ctx); err != nil {
		return "", err
	}
	vpcSvc, _ := e.env().Lookup("compute")
	vpc, ok := vpcSvc.(emu.VPC)
	if !ok {
		return "", errors.New("no compute service")
	}
	sn, err := vpc.SubnetNetwork(ctx, subnetwork)
	if err != nil {
		return "", err
	}
	rt, err := e.env().Containers.Runtime(ctx)
	if err != nil {
		return "", err
	}
	if err := e.joinLocked(ctx, rt, vpc, sn); err != nil {
		return "", err
	}
	c, err := rt.InspectContainer(ctx, e.id)
	if err != nil {
		return "", err
	}
	return c.IPs[sn.Name], nil
}

// The edge container's ports, which the VM relay's stream counts name.
const (
	EdgeControlPort = edgeControlPort
	EdgeRelayPort   = edgeRelayPort
)

// SetSingleLoopback switches the data plane in or out of single-loopback
// mode, which macOS without loopback aliases uses (call it before
// creating forwarding rules).
func SetSingleLoopback(svc emu.Service, on bool) {
	d := svc.(*Service).dp
	d.mu.Lock()
	defer d.mu.Unlock()
	d.single = on
}

// SingleLoopback reports whether the data plane is in single-loopback mode.
func SingleLoopback(svc emu.Service) bool {
	d := svc.(*Service).dp
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.single
}

// MappedIP is the listener IP emulated DNS answers for a forwarding rule IP.
func MappedIP(svc emu.Service, ip string) string {
	if m := svc.(*Service).dp.mappedIP(net.ParseIP(ip)); m != nil {
		return m.String()
	}
	return ""
}

// SetPublished makes the data plane serve forwarding rules on Published
// ports as a Containerized Instance does, with ports the container's
// published ports (nil: no runtime socket), served on host instead of the
// wildcard (call it before creating forwarding rules).
func SetPublished(svc emu.Service, ports map[int]string, host string) {
	d := svc.(*Service).dp
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pub = publishing{on: true, noSocket: ports == nil}
	if ports != nil {
		d.pub.ports = published.NewPorts(ports, d.s.env.Config)
	}
	d.pubKnown = true
	publishedHost = host
}

// ListenerAddr returns the loopback listener IP of forwarding rule name.
func ListenerAddr(svc emu.Service, name string) string {
	d := svc.(*Service).dp
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, l := range d.listeners {
		if l.name == name {
			h, _, _ := net.SplitHostPort(l.addr)
			return h
		}
	}
	return ""
}
