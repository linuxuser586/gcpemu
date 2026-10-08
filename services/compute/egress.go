package compute

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/agent"
	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Cloud NAT data plane (FR-NAT-002..005). Each VPC network with realised
// subnetworks gets one egress gateway container, attached to the external
// network (internet egress), the services network (emulator endpoints)
// and every realised subnetwork / PSA network of the VPC at the range's
// reserved second-to-last address. Workloads use that address as their
// default route. The gateway:
//
//   - routes between the VPC's networks (source-NATed so replies return
//     through it), giving the VPC its implied any-to-any routes;
//   - forwards and masquerades traffic to the internet only from source
//     ranges a Cloud NAT on a router in the subnetwork's region and
//     network covers (sourceSubnetworkIpRangesToNat), dropping the rest
//     (GCP-like timeout) or rejecting it with natReject;
//   - in --offline mode redirects NAT-allowed TCP egress to a sink in the
//     gateway that records the attempt and answers with natSinkResponse;
//   - streams conntrack events for NAT translation logs, and NFLOGs the
//     first packet of every refused egress connection for drop logs
//     (FR-NAT-004).
//
// The data plane is re-programmed after every router, NAT, subnetwork or
// connection change.

// natImage runs the egress gateway (it has iptables, ip and conntrack).
const natImage = "rancher/k3s:v1.36.5-k3s1"

// Images lists the container images the compute service launches, by role
// (pinned by tag; digests are pinned at release).
var Images = map[string]string{
	"nat-gateway": natImage,
}

// natSinkPort is the sink's port inside the gateway.
const natSinkPort = 15001

// natAgentName is the in-container agent run by the gateway.
const natAgentName = "nat-gateway"

type gateway struct {
	s       *Service
	network string // VPC network path

	mu    sync.Mutex
	id    string
	extIP string
	extGW string
	svcIP string
	// rules is the last programmed rule set (to skip no-op updates).
	rules string

	kick   chan struct{}
	cancel context.CancelFunc
	done   chan struct{}
}

// gatewayFor returns the running gateway of a VPC network, if any.
func (s *Service) gatewayFor(np string) *gateway {
	s.egMu.Lock()
	defer s.egMu.Unlock()
	return s.egress[np]
}

// EgressGateway implements emu.VPC.
func (s *Service) EgressGateway(ctx context.Context, subnetwork string) (string, error) {
	net, err := s.SubnetNetwork(ctx, subnetwork)
	if err != nil {
		return "", err
	}
	g, err := s.ensureGateway(ctx, net.NetworkName)
	if err != nil {
		return "", err
	}
	if err := g.sync(ctx); err != nil {
		return "", err
	}
	c, _ := parseCIDR(net.Subnet, false)
	return ipString(c.reservedTail()), nil
}

// ensureGateway returns the network's gateway, starting it if needed.
func (s *Service) ensureGateway(ctx context.Context, np string) (*gateway, error) {
	s.egMu.Lock()
	g := s.egress[np]
	if g == nil {
		bg, cancel := context.WithCancel(context.Background())
		g = &gateway{s: s, network: np, kick: make(chan struct{}, 1), cancel: cancel, done: make(chan struct{})}
		s.egress[np] = g
		go g.loop(bg)
	}
	s.egMu.Unlock()
	if err := g.ensureRunning(ctx); err != nil {
		return nil, err
	}
	return g, nil
}

// removeGateway stops and removes a VPC network's gateway container.
func (s *Service) removeGateway(ctx context.Context, np string) {
	s.egMu.Lock()
	g := s.egress[np]
	delete(s.egress, np)
	s.egMu.Unlock()
	if g == nil {
		return
	}
	g.stop()
	g.mu.Lock()
	id := g.id
	g.id = ""
	g.mu.Unlock()
	if id == "" {
		return
	}
	if rt, err := s.runtime(ctx); err == nil {
		_ = rt.RemoveContainer(ctx, id, true)
	}
}

// networkChanged schedules re-programming of a VPC network's gateway.
func (s *Service) networkChanged(np string) {
	if g := s.gatewayFor(np); g != nil {
		select {
		case g.kick <- struct{}{}:
		default:
		}
	}
}

// attachIfGateway synchronously re-syncs a running gateway (so newly
// realised networks are attached before callers use them).
func (s *Service) attachIfGateway(ctx context.Context, np string) {
	if g := s.gatewayFor(np); g != nil {
		if err := g.sync(ctx); err != nil {
			s.env.Log.Warn("compute: syncing NAT gateway", "network", np, "err", err)
		}
	}
}

func (g *gateway) stop() {
	g.cancel()
	<-g.done
}

// loop re-programs the gateway when kicked.
func (g *gateway) loop(ctx context.Context) {
	defer close(g.done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-g.kick:
		}
		sctx, cancel := context.WithTimeout(ctx, time.Minute)
		if err := g.sync(sctx); err != nil && ctx.Err() == nil {
			g.s.env.Log.Warn("compute: re-programming NAT gateway", "network", g.network, "err", err)
		}
		cancel()
	}
}

// ensureRunning creates and starts the gateway container if it is absent
// or has died.
func (g *gateway) ensureRunning(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	rt, err := g.s.runtime(ctx)
	if err != nil {
		return err
	}
	if g.id != "" {
		if c, err := rt.InspectContainer(ctx, g.id); err == nil && c.Running {
			return nil
		}
	}
	g.rules = ""
	return g.startLocked(ctx, rt)
}

func (g *gateway) startLocked(ctx context.Context, rt *runtime.Manager) error {
	plane, err := g.s.env.Containers.Netplane(ctx)
	if err != nil {
		return apierr.FailedPrecondition("NAT gateway: %v", err)
	}
	ext, err := plane.External(ctx)
	if err != nil {
		return apierr.Internal("NAT gateway: external network: %v", err)
	}
	svc, err := plane.Services(ctx)
	if err != nil {
		return apierr.Internal("NAT gateway: services network: %v", err)
	}
	if err := rt.EnsureImage(ctx, natImage, g.s.env.Config.Offline); err != nil {
		return apierr.FailedPrecondition("NAT gateway image %s: %v", natImage, err)
	}
	bin, err := agent.Binary()
	if err != nil {
		return apierr.FailedPrecondition("NAT gateway agent: %v", err)
	}
	report, err := g.s.sinkReportURL(ctx)
	if err != nil {
		g.s.env.Log.Warn("compute: NAT event reporting unavailable", "err", err)
	}
	name := rt.Name("nat", lastSeg(g.network), shortHash(g.network))
	_ = rt.RemoveContainer(ctx, name, true)
	id, err := rt.CreateContainer(ctx, runtime.ContainerSpec{
		Name:       name,
		Image:      natImage,
		Entrypoint: []string{agent.ContainerPath},
		Cmd:        []string{},
		Env: []string{
			agent.EnvVar + "=" + natAgentName,
			"GCPEMU_NAT_REPORT=" + report,
			"GCPEMU_NAT_TOKEN=" + g.s.sinkToken(),
			"GCPEMU_NAT_NETWORK=" + g.network,
			fmt.Sprintf("GCPEMU_NAT_SINK_PORT=%d", natSinkPort),
			"GCPEMU_NAT_SINK_RESPONSE=" + g.s.env.Config.NATSinkResponse,
		},
		Labels:   rt.Labels("compute", g.network, "nat"),
		Hostname: "nat-gateway",
		CapAdd:   []string{"NET_ADMIN", "NET_RAW"},
		Sysctls: map[string]string{
			"net.ipv4.ip_forward":             "1",
			"net.ipv4.conf.all.rp_filter":     "0",
			"net.ipv4.conf.default.rp_filter": "0",
		},
		Networks: []runtime.Attachment{{Network: ext.Name}, {Network: svc.Name}},
	})
	if err != nil {
		return apierr.Internal("NAT gateway: %v", err)
	}
	if err := rt.CopyTo(ctx, id, "/", []runtime.File{{Name: strings.TrimPrefix(agent.ContainerPath, "/"), Mode: 0o755, Path: bin}}); err != nil {
		_ = rt.RemoveContainer(ctx, id, true)
		return apierr.Internal("NAT gateway: copy agent: %v", err)
	}
	if err := rt.StartContainer(ctx, id); err != nil {
		_ = rt.RemoveContainer(ctx, id, true)
		return apierr.Internal("NAT gateway: start: %v", err)
	}
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := rt.WaitRunning(wctx, id); err != nil {
		_ = rt.RemoveContainer(ctx, id, true)
		return apierr.Internal("NAT gateway: %v", err)
	}
	c, err := rt.InspectContainer(ctx, id)
	if err != nil {
		return err
	}
	g.id, g.extIP, g.extGW, g.svcIP = id, c.IPs[ext.Name], ext.Gateway, c.IPs[svc.Name]
	g.s.env.Log.Info("compute: NAT egress gateway started", "network", g.network, "container", name)
	return nil
}

// detach disconnects the gateway from a container network being removed.
func (g *gateway) detach(ctx context.Context, netName string) {
	g.mu.Lock()
	id := g.id
	g.rules = ""
	g.mu.Unlock()
	if id == "" {
		return
	}
	if rt, err := g.s.runtime(ctx); err == nil {
		_ = rt.Disconnect(ctx, netName, id)
	}
}

// sync attaches the gateway to every realised network of the VPC and
// programs the NAT rules.
func (g *gateway) sync(ctx context.Context) error {
	if err := g.ensureRunning(ctx); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	rt, err := g.s.runtime(ctx)
	if err != nil {
		return err
	}
	c, err := rt.InspectContainer(ctx, g.id)
	if err != nil {
		return err
	}
	nets := g.s.netsOf(g.network)
	for _, st := range nets {
		if _, ok := c.IPs[st.Name]; ok {
			continue
		}
		cc, _ := parseCIDR(st.Subnet, false)
		if err := rt.Connect(ctx, st.Name, g.id, ipString(cc.reservedTail())); err != nil && !strings.Contains(err.Error(), "already exists") {
			return apierr.Internal("NAT gateway: attach to %s: %v", st.Name, err)
		}
	}
	rules, err := g.s.natRules(g, nets)
	if err != nil {
		return err
	}
	if rules == g.rules {
		return nil
	}
	res, err := rt.Exec(ctx, g.id, []string{"sh", "-c", rules}, nil)
	if err != nil {
		return apierr.Internal("NAT gateway: program rules: %v", err)
	}
	if res.ExitCode != 0 {
		return apierr.Internal("NAT gateway: program rules failed (%d): %s%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	g.rules = rules
	return nil
}

// coverage is a source range a NAT covers.
type coverage struct {
	cidr          string
	router, nat   string
	region        string
	subnet        string
	log, logTrans bool
	logErrors     bool
}

// natCoverage computes the source ranges of the VPC's realised
// subnetworks that a Cloud NAT covers (FR-NAT-002).
func (s *Service) natCoverage(np string, nets []*vpcNet) []coverage {
	var out []coverage
	_ = s.env.Store.View(func(tx store.Tx) error {
		p, _, _ := pathParts(np)
		routers := list[computev1.Router](tx, nsRouters, "projects/"+p+"/regions/")
		for _, st := range nets {
			if st.Subnetwork == "" {
				continue
			}
			sn, ok := get[computev1.Subnetwork](tx, nsSubnets, st.Subnetwork)
			if !ok {
				continue
			}
			for _, rt := range routers {
				_, reg, _ := pathParts(relPath(rt.SelfLink))
				if relPath(rt.Network) != np || reg != st.Region {
					continue
				}
				for _, nat := range rt.Nats {
					for _, c := range natRanges(nat, sn, st.Subnet) {
						cv := coverage{cidr: c, router: relPath(rt.SelfLink), nat: nat.Name, region: reg, subnet: st.Subnetwork}
						if nat.LogConfig != nil && nat.LogConfig.Enable {
							cv.log = true
							cv.logTrans = nat.LogConfig.Filter != "ERRORS_ONLY"
							cv.logErrors = nat.LogConfig.Filter != "TRANSLATIONS_ONLY"
						}
						out = append(out, cv)
					}
				}
			}
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].cidr < out[j].cidr })
	return out
}

// natRules renders the shell script that programs the gateway. Interfaces
// are resolved inside the container from the addresses they carry.
func (s *Service) natRules(g *gateway, nets []*vpcNet) (string, error) {
	cov := s.natCoverage(g.network, nets)
	var b strings.Builder
	b.WriteString("set -e\n")
	fmt.Fprintf(&b, "ifc() { ip -o -4 addr show | awk -v a=\"$1\" '{split($4,x,\"/\"); if (x[1]==a) print $2}'; }\n")
	fmt.Fprintf(&b, "EXT=$(ifc %s)\n[ -n \"$EXT\" ]\n", g.extIP)
	fmt.Fprintf(&b, "ip route replace default via %s dev \"$EXT\"\n", g.extGW)
	b.WriteString("iptables -N GCPEMU-FWD 2>/dev/null || true\n")
	b.WriteString("iptables -t nat -N GCPEMU-POST 2>/dev/null || true\n")
	b.WriteString("iptables -t nat -N GCPEMU-PRE 2>/dev/null || true\n")
	b.WriteString("iptables -C FORWARD -j GCPEMU-FWD 2>/dev/null || iptables -I FORWARD -j GCPEMU-FWD\n")
	b.WriteString("iptables -t nat -C POSTROUTING -j GCPEMU-POST 2>/dev/null || iptables -t nat -I POSTROUTING -j GCPEMU-POST\n")
	b.WriteString("iptables -t nat -C PREROUTING -j GCPEMU-PRE 2>/dev/null || iptables -t nat -I PREROUTING -j GCPEMU-PRE\n")
	b.WriteString("iptables-restore --noflush <<EOF\n*filter\n:GCPEMU-FWD - [0:0]\n:GCPEMU-DROP - [0:0]\n-F GCPEMU-FWD\n-F GCPEMU-DROP\n")
	b.WriteString("-A GCPEMU-FWD -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT\n")
	b.WriteString("-A GCPEMU-FWD ! -o $EXT -j ACCEPT\n")
	for _, c := range cov {
		fmt.Fprintf(&b, "-A GCPEMU-FWD -s %s -o $EXT -j ACCEPT\n", c.cidr)
	}
	b.WriteString("-A GCPEMU-FWD -o $EXT -j GCPEMU-DROP\n")
	if s.env.Config.NATReject {
		b.WriteString("-A GCPEMU-DROP -p tcp -j REJECT --reject-with tcp-reset\n")
		b.WriteString("-A GCPEMU-DROP -j REJECT --reject-with icmp-net-unreachable\n")
	} else {
		b.WriteString("-A GCPEMU-DROP -j DROP\n")
	}
	b.WriteString("COMMIT\n*nat\n:GCPEMU-POST - [0:0]\n:GCPEMU-PRE - [0:0]\n-F GCPEMU-POST\n-F GCPEMU-PRE\n")
	b.WriteString("-A GCPEMU-POST -o $EXT -j MASQUERADE\n")
	// Intra-VPC traffic is source-NATed to the gateway so replies return
	// through it regardless of the destination's default route.
	for _, st := range nets {
		fmt.Fprintf(&b, "-A GCPEMU-POST -d %s ! -s %s -j MASQUERADE\n", st.Subnet, st.Subnet)
	}
	if s.env.Config.Offline {
		var internal []string
		for _, st := range nets {
			internal = append(internal, st.Subnet)
		}
		for _, c := range cov {
			for _, in := range internal {
				fmt.Fprintf(&b, "-A GCPEMU-PRE -s %s -d %s -j RETURN\n", c.cidr, in)
			}
			fmt.Fprintf(&b, "-A GCPEMU-PRE -s %s -p tcp -j REDIRECT --to-ports %d\n", c.cidr, natSinkPort)
		}
	}
	b.WriteString("COMMIT\nEOF\n")
	// Drop logging is best effort: a kernel without NFLOG still enforces.
	fmt.Fprintf(&b, "iptables -I GCPEMU-DROP -m conntrack --ctstate NEW -j NFLOG --nflog-group %d 2>/dev/null || echo 'NFLOG unavailable: NAT drops are not logged' >&2\n", natDropGroup)
	return b.String(), nil
}
