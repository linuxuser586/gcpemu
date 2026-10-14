package gke

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/loopback"
	"github.com/linuxuser586/gcpemu/internal/netplane"
)

// LoadBalancer Services on a VM runtime (ADR 0005) or a Translated network
// (ADR 0004). k3s's servicelb gives a LoadBalancer Service its nodes'
// addresses, which the host cannot reach there, so clusters run without it
// (clusterInternal.HostLB) and this controller takes its place.
// Each Service gets host listeners by the rules LB forwarding rules follow
// (#172, loopback.Listen):
//
//   - with loopback aliases, a 127.0.0.x of its own, allocated from a
//     hash of the cluster and Service and skipping the addresses that
//     forwarding rules and Cloud SQL instances hold, on each Service port;
//   - without them, 127.0.0.1 on each Service port, or a free port when
//     another socket holds it;
//   - on macOS, a privileged port on the wildcard address, serving only
//     loopback clients, or a free port when another socket holds that.
//
// Each listener pipes its connections to the Service's node port on a
// node's container address, through the VM relay on a VM runtime. The address is reported as the Service's
// load balancer ingress, and each port as endpoint
// "gke:<cluster>/<namespace>/<service>/<port>", which `gcpemu env` prints
// and the Web console shows. Only TCP ports with a node port are served.
//
// A Containerized Instance (ADR 0003) has no host loopback to listen on:
// its own is the container's (#276). On a Translated network it reports
// its Ready nodes' Fallback-range addresses as the ingress instead, which
// a Linux host routes to, and opens no listener: kube-proxy on every node
// serves a LoadBalancer Service's ingress IPs on the Service's ports.
// The endpoints then hold the first of them.
//
// On a VM runtime (ADR 0012) the host routes to no container address, so
// a Containerized Instance serves each Service port on the wildcard
// address of its container, on a port the container publishes
// (publishedCandidates), and reports the host side of that binding: its
// host IP as the ingress, and its host address as the endpoint. A Service
// port with no free published port is not served, and is retried while
// its Service stays; a Service with none served has no ingress.

const lbEndpointPrefix = "gke:"

// lbMode is how the host reaches a cluster's LoadBalancer Services.
type lbMode int

const (
	// lbLoopback: host listeners, for an Instance on the host.
	lbLoopback lbMode = iota
	// lbNodes: the Ready nodes' addresses, for a Containerized Instance
	// off a VM runtime.
	lbNodes
	// lbPublished: the container's published ports, for a Containerized
	// Instance on a VM runtime.
	lbPublished
)

// publishedHost is the address published ports are served on in the
// emulator's container; "" is the wildcard.
var publishedHost = ""

// lbService is the host side of one LoadBalancer Service.
type lbService struct {
	sig       string // the ports it serves, to notice changes
	ip        string // its loopback address; "" when the nodes serve it
	ingress   []string
	endpoints []string
	ex        []*netplane.Exposed
	// served holds the Service ports served on published ports
	// (lbPublished); unserved is set while some wait for a free one.
	served   map[int]bool
	unserved bool
}

// lbPort is a Service port served on the host.
type lbPort struct {
	port, nodePort int
}

// lbServicesLoop serves the cluster's LoadBalancer Services while ctx
// lives, when the cluster runs without servicelb.
func (s *Service) lbServicesLoop(ctx context.Context, key string) {
	if s.env.Containers == nil {
		return
	}
	if rec, err := s.loadKey(key); err != nil || !rec.Int.HostLB {
		return
	}
	np, err := s.env.Containers.Netplane(ctx)
	if err != nil {
		return
	}
	// Inside a container the VM relay is off, so HostLB means a
	// Translated network, or a VM runtime.
	rt := np.Runtime()
	mode := lbLoopback
	if rt.Self() != "" {
		mode = lbNodes
		if rt.VM.On {
			mode = lbPublished
		}
	}
	// The container's published ports, fixed for its life.
	var published map[int]string
	lbs := map[string]*lbService{}
	defer func() {
		for id, l := range lbs {
			s.closeLB(key, id, l)
		}
	}()
	t := time.NewTicker(negInterval)
	defer t.Stop()
	for {
		err := error(nil)
		if mode == lbPublished && published == nil {
			published, err = rt.SelfPorts(ctx)
		}
		if err == nil {
			err = s.syncLBServices(ctx, np, key, mode, published, lbs)
		}
		if err != nil && ctx.Err() == nil {
			s.env.Log.Debug("gke: LoadBalancer Service sync", "cluster", key, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// syncLBServices reconciles the host side of a cluster's LoadBalancer
// Services, keyed by namespace/name in lbs, as mode says. published is the
// container's published ports, for lbPublished.
func (s *Service) syncLBServices(ctx context.Context, np *netplane.Plane, key string, mode lbMode, published map[int]string, lbs map[string]*lbService) error {
	kc, err := s.kube(key)
	if err != nil {
		return err
	}
	var svcs kubeList[kubeService]
	if err := kc.get(ctx, "/api/v1/services", &svcs); err != nil {
		return err
	}
	var nodeIPs []string
	if mode == lbNodes {
		rec, err := s.loadKey(key)
		if err != nil {
			return err
		}
		var nodes kubeList[kubeNode]
		if err := kc.get(ctx, "/api/v1/nodes", &nodes); err != nil {
			return err
		}
		ready := map[string]bool{}
		for _, n := range nodes.Items {
			ready[n.Metadata.Name] = n.ready()
		}
		// Until a node is Ready, keep what is reported.
		if nodeIPs = nodeIngress(rec.Int.Nodes, ready); len(nodeIPs) == 0 {
			return nil
		}
	}
	// Close the Services that went first, freeing their ports.
	seen := map[string]bool{}
	for _, svc := range svcs.Items {
		if svc.Spec.Type == "LoadBalancer" {
			seen[svc.Metadata.Namespace+"/"+svc.Metadata.Name] = true
		}
	}
	for id, l := range lbs {
		if !seen[id] {
			s.closeLB(key, id, l)
			delete(lbs, id)
		}
	}
	var errs []error
	for _, svc := range svcs.Items {
		if svc.Spec.Type != "LoadBalancer" {
			continue
		}
		id := svc.Metadata.Namespace + "/" + svc.Metadata.Name
		var ports []lbPort
		for _, p := range svc.Spec.Ports {
			if (p.Protocol == "" || p.Protocol == "TCP") && p.NodePort != 0 {
				ports = append(ports, lbPort{p.Port, p.NodePort})
			}
		}
		sig := fmt.Sprint(ports, nodeIPs)
		l := lbs[id]
		if l == nil || l.sig != sig {
			prev := ""
			if l != nil {
				prev = l.ip
				s.closeLB(key, id, l)
				delete(lbs, id)
			}
			if len(ports) == 0 {
				continue
			}
			switch mode {
			case lbNodes:
				l = s.reportLB(key, id, ports, nodeIPs)
				s.env.Log.Info("gke: LoadBalancer Service at its nodes", "cluster", key, "service", id, "ips", nodeIPs)
			case lbPublished:
				l = &lbService{served: map[int]bool{}}
				s.servePublished(np, key, id, ports, published, l, true)
			default:
				if l, err = s.serveLB(np, key, id, ports, prev); err != nil {
					errs = append(errs, fmt.Errorf("%s: %w", id, err))
					continue
				}
				s.env.Log.Info("gke: LoadBalancer Service listening", "cluster", key, "service", id, "ip", l.ip)
			}
			l.sig = sig
			lbs[id] = l
		} else if l.unserved {
			s.servePublished(np, key, id, ports, published, l, false)
		}
		ing := svc.Status.LoadBalancer.Ingress
		same := len(ing) == len(l.ingress)
		for i := 0; same && i < len(ing); i++ {
			same = ing[i].IP == l.ingress[i] && ing[i].Hostname == ""
		}
		if !same {
			want := make([]map[string]string, len(l.ingress))
			for i, ip := range l.ingress {
				want[i] = map[string]string{"ip": ip}
			}
			patch := map[string]any{"status": map[string]any{"loadBalancer": map[string]any{"ingress": want}}}
			if err := kc.mergePatch(ctx, "/api/v1/namespaces/"+svc.Metadata.Namespace+"/services/"+svc.Metadata.Name+"/status", patch); err != nil {
				errs = append(errs, fmt.Errorf("%s: status: %w", id, err))
			}
		}
	}
	return errors.Join(errs...)
}

// serveLB binds a Service's ports on its loopback address, trying prev
// first, and pipes them to its node ports.
func (s *Service) serveLB(np *netplane.Plane, key, id string, ports []lbPort, prev string) (*lbService, error) {
	name := key + "/" + id
	cands := []string{loopback.Host}
	aliases := loopback.Aliases()
	if aliases {
		cands = s.loopCandidates(name, prev)
	}
	var errs []error
	for _, ip := range cands {
		l := &lbService{ip: ip, ingress: []string{ip}}
		var err error
		for _, p := range ports {
			var ln net.Listener
			var addr string
			// With aliases a port held on this address moves the
			// Service to another one, so that all its ports share one.
			if ln, addr, err = loopback.Listen(ip, p.port, !aliases); err != nil {
				break
			}
			l.ex = append(l.ex, np.Serve(ln, s.nodePorts(key, p.nodePort)))
			ep := lbEndpoint(key, id, p.port)
			s.env.Endpoints.Set(ep, addr)
			l.endpoints = append(l.endpoints, ep)
		}
		if err == nil {
			s.loopMu.Lock()
			s.loops[name] = ip
			s.loopMu.Unlock()
			return l, nil
		}
		s.closeLB(key, id, l)
		errs = append(errs, err)
	}
	if !aliases {
		return nil, fmt.Errorf("serving it on %s: %w", loopback.Host, errors.Join(errs...))
	}
	return nil, fmt.Errorf("no free loopback address: %w", errors.Join(errs...))
}

// servePublished serves the ports of a Service that l does not serve yet
// on published ports (publishedCandidates), and records the host side in
// l. A port with none free stays missing; with warn, it is logged with
// what to publish.
func (s *Service) servePublished(np *netplane.Plane, key, id string, ports []lbPort, published map[int]string, l *lbService, warn bool) {
	lo, hi, ranged, _ := s.env.Config.ParsePortRange()
	l.unserved = false
	var missing []int
	for _, p := range ports {
		if l.served[p.port] {
			continue
		}
		ep := lbEndpoint(key, id, p.port)
		var ln net.Listener
		var cport int
		for _, c := range publishedCandidates(key+"/"+id+"/"+strconv.Itoa(p.port), p.port, published, lo, hi, ranged) {
			var err error
			if ln, err = net.Listen("tcp", net.JoinHostPort(publishedHost, strconv.Itoa(c))); err == nil {
				cport = c
				break
			}
		}
		if ln == nil {
			l.unserved = true
			missing = append(missing, p.port)
			continue
		}
		host := published[cport]
		l.ex = append(l.ex, np.Serve(ln, s.nodePorts(key, p.nodePort)))
		s.env.Endpoints.Set(ep, host)
		l.endpoints = append(l.endpoints, ep)
		l.served[p.port] = true
		if ip, _ := netplane.HostPort(host); !slices.Contains(l.ingress, ip) {
			l.ingress = append(l.ingress, ip)
		}
		s.env.Log.Info("gke: LoadBalancer Service on a published port", "cluster", key, "service", id, "port", p.port, "containerPort", cport, "host", host)
	}
	if warn && len(missing) > 0 {
		s.env.Log.Warn("gke: no free published port for a LoadBalancer Service; the host cannot reach it until one frees",
			"cluster", key, "service", id, "ports", missing, "fix", publishHint(s.env.Config.PortRange, missing[0]))
	}
}

// publishHint says what to publish so that a Service port can be served
// on a VM runtime.
func publishHint(portRange string, port int) string {
	own := fmt.Sprintf("-p 127.0.0.1:%d:%d", port, port)
	if portRange == "" {
		return "start the emulator with --port-range LO-HI and publish that range (-p 127.0.0.1:LO-HI:LO-HI), or publish the Service's port (" + own + ")"
	}
	return "publish more of --port-range " + portRange + " (-p 127.0.0.1:" + portRange + ":" + portRange + "), or the Service's port (" + own + ")"
}

// reportLB records a Service that its nodes serve at ips: no listener,
// endpoints at the first of them.
func (s *Service) reportLB(key, id string, ports []lbPort, ips []string) *lbService {
	l := &lbService{ingress: ips}
	for _, p := range ports {
		ep := lbEndpoint(key, id, p.port)
		s.env.Endpoints.Set(ep, net.JoinHostPort(ips[0], strconv.Itoa(p.port)))
		l.endpoints = append(l.endpoints, ep)
	}
	return l
}

// lbEndpoint names the endpoint of a Service port of the cluster key.
func lbEndpoint(key, id string, port int) string {
	return lbEndpointPrefix + key[strings.LastIndex(key, "/")+1:] + "/" + id + "/" + strconv.Itoa(port)
}

// nodeIngress returns the sorted Fallback-range addresses of the Ready
// nodes, at which a Containerized Instance's host reaches them.
func nodeIngress(nodes []nodeRecord, ready map[string]bool) []string {
	var out []string
	for _, n := range nodes {
		if ip := n.dialIP(); ready[n.Name] && ip != "" {
			out = append(out, ip)
		}
	}
	sort.Strings(out)
	return out
}

// closeLB stops serving a Service on the host.
func (s *Service) closeLB(key, id string, l *lbService) {
	for _, e := range l.ex {
		_ = e.Close()
	}
	for _, ep := range l.endpoints {
		s.env.Endpoints.Delete(ep)
	}
	s.loopMu.Lock()
	if s.loops[key+"/"+id] == l.ip {
		delete(s.loops, key+"/"+id)
	}
	s.loopMu.Unlock()
}

// nodePorts returns the addresses of a node port on the cluster's nodes,
// read for each connection so that it follows node changes.
func (s *Service) nodePorts(key string, port int) func() []string {
	return func() []string {
		rec, err := s.loadKey(key)
		if err != nil {
			return nil
		}
		out := make([]string, 0, len(rec.Int.Nodes))
		for _, n := range rec.Int.Nodes {
			if ip := n.dialIP(); ip != "" {
				out = append(out, net.JoinHostPort(ip, strconv.Itoa(port)))
			}
		}
		return out
	}
}

// loopCandidates lists the 127.0.0.x addresses to try for a Service, in
// order: prev, then x = hash of its name onwards (x in 2-254), skipping
// those that other Services, forwarding rules and Cloud SQL instances
// hold.
func (s *Service) loopCandidates(name, prev string) []string {
	used := map[string]bool{}
	for _, peer := range []string{"lb", "sql"} {
		if svc, ok := s.env.Lookup(peer); ok {
			if h, ok := svc.(emu.LoopbackHolder); ok {
				for _, a := range h.LoopbackAddrs() {
					used[a] = true
				}
			}
		}
	}
	s.loopMu.Lock()
	for n, a := range s.loops {
		if n != name {
			used[a] = true
		}
	}
	s.loopMu.Unlock()
	var out []string
	if prev != "" && prev != loopback.Host && !used[prev] {
		out = append(out, prev)
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	start := int(h.Sum32() % 253)
	for i := range 253 {
		a := "127.0.0." + strconv.Itoa((start+i)%253+2)
		if !used[a] && a != prev {
			out = append(out, a)
		}
	}
	return out
}

// LoopbackAddrs implements emu.LoopbackHolder: the 127.0.0.x addresses
// LoadBalancer Services hold. It takes only loopMu, which is never held
// while calling another Service, so peers may call it holding their own
// locks.
func (s *Service) LoopbackAddrs() []string {
	s.loopMu.Lock()
	defer s.loopMu.Unlock()
	out := make([]string, 0, len(s.loops))
	for _, a := range s.loops {
		if a != loopback.Host {
			out = append(out, a)
		}
	}
	sort.Strings(out)
	return out
}

// lbEnvVars returns GCPEMU_GKE_<CLUSTER>_<NAMESPACE>_<SERVICE>_<PORT> for
// each LoadBalancer Service port served on the host.
func lbEnvVars(endpoints map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range endpoints {
		if name, ok := strings.CutPrefix(k, lbEndpointPrefix); ok {
			out["GCPEMU_GKE_"+envName(name)] = v
		}
	}
	return out
}

// envName upper-cases a name for an env var, replacing other characters
// with '_'.
func envName(s string) string {
	var b strings.Builder
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z':
			b.WriteRune(c - 32)
		case c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b.WriteRune(c)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// publishedCandidates lists the container ports to try serving a Service
// port on, in order, on a VM runtime (ADR 0012): port itself when the
// emulator's container publishes it at the same host port, then the
// published ports of --port-range lo-hi (when ranged), from a hash of
// name onwards, so that a Service keeps its port across syncs and
// restarts where it can.
func publishedCandidates(name string, port int, published map[int]string, lo, hi int, ranged bool) []int {
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
