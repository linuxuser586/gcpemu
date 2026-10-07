package gke

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// vpc returns compute's VPC implementation, or a minimal stand-in when the
// compute service is not running (or does not provide one): every
// subnetwork then maps to one shared gke network with egress, private
// nodes have no egress at all and NEGs are not published.
func (s *Service) vpc() emu.VPC {
	if s.testVPC != nil {
		return s.testVPC
	}
	if p, ok := s.env.Lookup("compute"); ok {
		if v, ok := p.(emu.VPC); ok {
			return v
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fallback == nil {
		s.fallback = &localVPC{env: s.env}
	}
	return s.fallback
}

// localVPC is the stand-in VPC.
type localVPC struct {
	env   *emu.Env
	mu    sync.Mutex
	net   *emu.SubnetNet
	owned map[string]string // owner → IP
}

func (l *localVPC) ResolveSubnetwork(ctx context.Context, project, region, network, subnetwork string) (string, error) {
	sub := lastSeg(subnetwork)
	if sub == "" {
		sub = lastSeg(network)
	}
	if sub == "" {
		sub = "default"
	}
	return "projects/" + project + "/regions/" + region + "/subnetworks/" + sub, nil
}

func (l *localVPC) SubnetNetwork(ctx context.Context, subnetwork string) (emu.SubnetNet, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.net != nil {
		sn := *l.net
		sn.SubnetworkName = subnetwork
		return sn, nil
	}
	rt, err := l.env.Containers.Runtime(ctx)
	if err != nil {
		return emu.SubnetNet{}, err
	}
	name := rt.Name("gke-vpc")
	n, err := rt.InspectNetwork(ctx, name)
	if errors.Is(err, runtime.ErrNotFound) {
		// Static addresses need a user-configured subnet: try candidate
		// ranges until one does not overlap an existing network.
		for i := 0; i < 16; i++ {
			subnet := fmt.Sprintf("10.250.%d.0/20", i*16)
			_, err = rt.CreateNetwork(ctx, runtime.NetworkSpec{Name: name, Subnet: subnet, Labels: rt.Labels("gke", "vpc", "network")})
			if err == nil || strings.Contains(err.Error(), "already exists") {
				break
			}
		}
		if err != nil {
			return emu.SubnetNet{}, err
		}
		n, err = rt.InspectNetwork(ctx, name)
	}
	if err != nil {
		return emu.SubnetNet{}, err
	}
	segs := strings.Split(subnetwork, "/")
	sn := emu.SubnetNet{Name: n.Name, Subnet: n.Subnet, Gateway: n.Gateway, SubnetworkName: subnetwork}
	if len(segs) == 6 {
		sn.Region = segs[3]
		sn.NetworkName = "projects/" + segs[1] + "/global/networks/default"
	}
	l.net = &sn
	return sn, nil
}

func (l *localVPC) PrivateServicesNetwork(ctx context.Context, network string) (emu.SubnetNet, error) {
	return emu.SubnetNet{}, apierr.FailedPrecondition("the compute service is not running")
}

// AllocateIP hands out the lowest address not used by a recorded GKE
// container or another owner.
func (l *localVPC) AllocateIP(ctx context.Context, sn emu.SubnetNet, owner string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.owned == nil {
		l.owned = map[string]string{}
	}
	if ip, ok := l.owned[owner]; ok {
		return ip, nil
	}
	used := map[string]bool{sn.Gateway: true}
	for _, ip := range l.owned {
		used[ip] = true
	}
	_ = l.env.Store.View(func(tx store.Tx) error {
		for _, r := range listClusters(tx, "") {
			used[r.Int.ServerIP] = true
			for _, n := range r.Int.Nodes {
				used[n.IP] = true
			}
		}
		return nil
	})
	_, ipn, err := net.ParseCIDR(sn.Subnet)
	if err != nil {
		return "", err
	}
	base := ipn.IP.To4()
	ones, bits := ipn.Mask.Size()
	for i := 2; i < (1<<(bits-ones))-1; i++ {
		ip := net.IPv4(base[0], base[1], base[2]+byte(i>>8), base[3]+byte(i&0xff)).String()
		if !used[ip] {
			l.owned[owner] = ip
			return ip, nil
		}
	}
	return "", apierr.New(8, "no free addresses in %s", sn.Subnet)
}

func (l *localVPC) ReleaseIP(ctx context.Context, sn emu.SubnetNet, owner string) error {
	l.mu.Lock()
	delete(l.owned, owner)
	l.mu.Unlock()
	return nil
}

func (l *localVPC) EgressGateway(ctx context.Context, subnetwork string) (string, error) {
	return "", apierr.FailedPrecondition("Cloud NAT needs the compute service")
}

func (l *localVPC) UpsertNEG(ctx context.Context, project, zone, name, network, subnetwork string, defaultPort int, description string) error {
	return nil
}

func (l *localVPC) SetNEGEndpoints(ctx context.Context, project, zone, name string, eps []emu.NEGEndpoint) error {
	return nil
}

func (l *localVPC) DeleteNEG(ctx context.Context, project, zone, name string) error { return nil }
