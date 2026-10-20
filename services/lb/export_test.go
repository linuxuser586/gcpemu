package lb

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/linuxuser586/gcpemu/internal/edgeagent"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/published"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/services/gke"
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
	EdgeControlPort = edgeagent.ControlPort
	EdgeRelayPort   = edgeagent.RelayPort
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

// SetBeforeCertWrite makes f run before each managed-certificate pass
// writes a certificate's new status (exported to the external tests).
func SetBeforeCertWrite(svc emu.Service, f func(ctx context.Context, path string)) {
	svc.(*Service).beforeCertWrite = f
}

// SetPodRoutes makes f the edge's source of pod routes in place of GKE
// (exported to the external tests).
func SetPodRoutes(svc emu.Service, f func(ctx context.Context) []gke.PodRoute) {
	svc.(*Service).podRoutes = f
}

// SetSelfRuntime makes the data plane run as in a container, learning
// its Published ports anew from rt (emu.Containers.Runtime) and
// selfPorts (the Manager's Self, SelfFound and SelfPorts), served on host instead
// of the wildcard (call it before creating forwarding rules).
func SetSelfRuntime(svc emu.Service, rt func(ctx context.Context) (*runtime.Manager, error), selfPorts func(ctx context.Context) (string, bool, map[int]string, error), host string) {
	d := svc.(*Service).dp
	d.mu.Lock()
	defer d.mu.Unlock()
	d.self = &selfProbe{
		inContainer: func() bool { return true },
		runtime:     rt,
		selfPorts: func(ctx context.Context, _ *runtime.Manager) (string, bool, map[int]string, error) {
			return selfPorts(ctx)
		},
	}
	d.pub, d.pubKnown, d.pubRetry = publishing{}, false, time.Time{}
	d.edge.mu.Lock()
	d.edge.rt, d.edge.rtRetry = rtUnknown, time.Time{}
	d.edge.mu.Unlock()
	publishedHost = host
}

// SetContainerized sets --containerized (call it before creating
// forwarding rules).
func SetContainerized(svc emu.Service, setting string) {
	svc.(*Service).env.Config.Containerized = setting
}

// Publishing reports whether the data plane serves forwarding rules on
// Published ports.
func Publishing(svc emu.Service) bool {
	d := svc.(*Service).dp
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pubKnown && d.pub.on
}

// RetryPublishedNow ends the data plane's backoff after it failed to
// read the Published ports, as if it had elapsed. The edge's own backoff
// is left running.
func RetryPublishedNow(svc emu.Service) {
	d := svc.(*Service).dp
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pubRetry = time.Time{}
}

// RuntimeAvailable reports whether the edge finds a container runtime,
// as the listener mode choice asks.
func RuntimeAvailable(svc emu.Service) bool {
	d := svc.(*Service).dp
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.edge.available(d.ctx)
}
