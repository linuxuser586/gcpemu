package lb

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linuxuser586/gcpemu/internal/agent"
	"github.com/linuxuser586/gcpemu/internal/edgeagent"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/services/gke"
)

// The lb-edge container (FR-LB-002, FR-INT-001): started on demand when a
// forwarding rule needs a privileged port the emulator cannot bind, when
// a backend's endpoints are GKE pod IPs the host cannot route to, or when
// GKE clusters run beside external forwarding rules. It joins a labelled
// "lb" network (forwarding rule addresses), the services network (to reach
// the emulator's edge ingress) and every VPC subnetwork with pod routes.
// The in-process proxy stays the only L7 implementation: the edge only
// relays TCP (internal/edgeagent). The edge needs a container runtime: without
// one for good (emu.ErrNoRuntime) its callers fall back for the
// Instance's life; after a transient failure they fall back for now, and
// the runtime is asked again after rtRetryAfter (#313).
//
// VPC workloads reach external forwarding rules by their own address, as
// on GCP (#187). The emulated DNS answers them with it (dnsmap.go), and
// the edge joins the external network, holds every external forwarding
// rule address and relays its ports like the lb-network aliases. Services
// whose containers egress over the external network (GKE nodes with
// external IPs, the NAT egress proxies) route those addresses via the
// edge (emu.PublicRouter), so the client the backends see is the node or
// the NAT, as on GCP.

// Images lists the container images the lb service launches (pinned by
// tag; digests are pinned at release).
var Images = map[string]string{
	"lb-edge": edgeagent.Image,
}

type edge struct {
	d *dataplane

	// syncMu serialises route passes with leaveSubnetwork, so that a pass
	// holding pod routes read before a subnetwork's last ones went does not
	// join it again after compute asked the Edge relay to leave it.
	syncMu sync.Mutex
	mu     sync.Mutex
	// rt is what the runtime check found; after a transient failure it
	// stays rtUnknown and is not tried again before rtRetry.
	rt      rtState
	rtRetry time.Time
	id      string
	ip      string // container address on the lb network
	subnet  *net.IPNet
	netName string
	token   string
	ingress net.Listener
	inAddr  string            // ingress address on the services network
	alias   map[string]string // frontend key → Edge alias
	// resMu guards reserved, the distinct Edge aliases (ReservedAddrs).
	resMu    sync.Mutex
	reserved []string
	byAlias  map[string]string // alias ip:port → frontend key
	ports    map[string]int    // frontend key → port
	routes   []edgeagent.Route
	pods     []*net.IPNet
	nets     map[string]bool
	// subnets holds the subnetwork networks the edge joined, by network
	// name; on those not Translated it holds an address compute allocated
	// for it.
	subnets map[string]emu.SubnetNet
	// public are the external forwarding rule addresses and ports the
	// edge serves on the external network, where it holds extIP; routed
	// is the last route set the PublicRouters accepted ("" for none).
	public  []edgeagent.Listener
	extIP   string
	routed  string
	pushed  string
	stopped bool
	// idleSince is when the edge last became unused (no forwarding rule
	// mapped, no pod endpoints); it is removed after edgeIdleGrace.
	idleSince time.Time

	ctlOnce sync.Once
	ctl     *http.Client
}

// edgeIdleGrace is how long an unused edge container is kept (so that a
// forwarding rule being replaced does not restart it).
const edgeIdleGrace = 5 * time.Second

func newEdge(d *dataplane) *edge {
	return &edge{d: d, alias: map[string]string{}, byAlias: map[string]string{}, ports: map[string]int{}, nets: map[string]bool{}, subnets: map[string]emu.SubnetNet{}}
}

func (e *edge) env() *emu.Env { return e.d.s.env }

// control returns the client for the edge's control port, which it
// reaches through the ContainerDialer.
func (e *edge) control() *http.Client {
	e.ctlOnce.Do(func() {
		e.ctl = &http.Client{Transport: &http.Transport{DialContext: e.env().Containers.DialContext, MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second}}
	})
	return e.ctl
}

// rtState is what the edge knows of the container runtime.
type rtState int

const (
	// rtUnknown: not asked yet, or the last try failed transiently (the
	// socket not ready yet, a timeout); asked again after rtRetryAfter.
	rtUnknown rtState = iota
	// rtUp: the runtime answered.
	rtUp
	// rtNone: no runtime at all (emu.ErrNoRuntime), for good.
	rtNone
)

// rtRetryAfter is how long the edge waits before asking the runtime again
// after a transient failure. Callers of runtimeState hold the data plane's
// mutex and each try can block for 5s, so it is not asked on every call.
const rtRetryAfter = 30 * time.Second

// runtimeState reports what the edge knows of the container runtime,
// asking it when unknown and not backing off, or with retry whatever the
// backoff (a caller with its own backoff). Only an answer or a permanent
// failure is kept. Callers hold d.mu.
func (e *edge) runtimeState(ctx context.Context, retry bool) rtState {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.rt != rtUnknown || (!retry && time.Now().Before(e.rtRetry)) {
		return e.rt
	}
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	_, err := e.d.containerRuntime(c)
	cancel()
	switch {
	case err == nil:
		e.rt = rtUp
	case errors.Is(err, emu.ErrNoRuntime):
		e.rt = rtNone
	default:
		e.rtRetry = time.Now().Add(rtRetryAfter)
		e.env().Log.Debug("lb: container runtime not reachable; trying again later", "err", err, "retryAfter", rtRetryAfter)
	}
	return e.rt
}

// available reports whether a container runtime can run the edge now.
// After a transient failure it is false until a later try succeeds.
// Callers hold d.mu.
func (e *edge) available(ctx context.Context) bool {
	return e.runtimeState(ctx, false) == rtUp
}

// add maps a frontend onto an edge address and returns "alias:port".
func (e *edge) add(ctx context.Context, fe *frontend) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.ensureLocked(ctx); err != nil {
		return "", err
	}
	ip := e.alias[fe.key]
	if ip == "" {
		// An Edge alias skips the addresses containers hold on the lb
		// network, the VM relay's above all (#900). Without them it could
		// take one, so it falls back to a high port instead.
		rt, err := e.env().Containers.Runtime(ctx)
		if err != nil {
			return "", err
		}
		held, err := rt.NetworkAddrs(ctx, e.netName)
		if err != nil {
			return "", fmt.Errorf("addresses on %s: %w", e.netName, err)
		}
		ip = e.allocAliasLocked(fe.ip, held)
		e.setAliasLocked(fe.key, ip)
	}
	addr := net.JoinHostPort(ip, strconv.Itoa(fe.port))
	e.byAlias[addr] = fe.key
	e.ports[fe.key] = fe.port
	if err := e.pushLocked(ctx); err != nil {
		return "", err
	}
	return addr, nil
}

// remove unmaps a frontend.
func (e *edge) remove(key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ip := e.alias[key]
	if ip == "" {
		return
	}
	delete(e.byAlias, net.JoinHostPort(ip, strconv.Itoa(e.ports[key])))
	e.dropAliasLocked(key)
	delete(e.ports, key)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = e.pushLocked(ctx)
}

// setAliasLocked maps frontend key to Edge alias ip, and dropAliasLocked
// unmaps it; both keep reserved, the Edge aliases ReservedAddrs returns,
// in step.
func (e *edge) setAliasLocked(key, ip string) {
	e.alias[key] = ip
	e.reserveLocked()
}

func (e *edge) dropAliasLocked(key string) {
	delete(e.alias, key)
	e.reserveLocked()
}

func (e *edge) reserveLocked() {
	seen := map[string]bool{}
	var ips []string
	for _, ip := range e.alias {
		if !seen[ip] {
			seen[ip] = true
			ips = append(ips, ip)
		}
	}
	e.resMu.Lock()
	e.reserved = ips
	e.resMu.Unlock()
}

var _ emu.AddressReserver = (*Service)(nil)

// ReservedAddrs implements emu.AddressReserver: the live Edge aliases, so
// that a container joining the lb network after them, such as a VM relay
// recreated (ADR 0050), does not take one (#900). The runtime asks while
// the Edge relay holds e.mu (a join), so this reads only resMu.
func (s *Service) ReservedAddrs() []string {
	e := s.dp.edge
	e.resMu.Lock()
	defer e.resMu.Unlock()
	return slices.Clone(e.reserved)
}

// allocAliasLocked picks the Edge alias for a forwarding rule IP: the same
// one for every port of the IP, allocated from the top of the subnet and
// skipping the addresses containers hold on the lb network (held).
func (e *edge) allocAliasLocked(frIP string, held map[string]bool) string {
	for key, ip := range e.alias {
		if h, _, _ := net.SplitHostPort(key); h == frIP {
			return ip
		}
	}
	used := map[string]bool{e.ip: true}
	for ip := range held {
		used[ip] = true
	}
	for _, ip := range e.alias {
		used[ip] = true
	}
	base := binary.BigEndian.Uint32(e.subnet.IP.To4())
	ones, bits := e.subnet.Mask.Size()
	last := base + uint32(1)<<(bits-ones) - 2
	for v := last; v > base+2; v-- {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], v)
		ip := net.IP(b[:]).String()
		if !used[ip] {
			return ip
		}
	}
	return ""
}

// covers reports whether ip must be reached through the edge relay.
func (e *edge) covers(ip string) bool {
	p := net.ParseIP(ip)
	if p == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.id == "" {
		return false
	}
	for _, n := range e.pods {
		if n.Contains(p) {
			return true
		}
	}
	return false
}

// dial opens a relayed connection to addr through the edge container.
func (e *edge) dial(ctx context.Context, addr string) (net.Conn, error) {
	e.mu.Lock()
	ip, token := e.ip, e.token
	e.mu.Unlock()
	if ip == "" {
		return nil, errors.New("lb-edge relay is not running")
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	c, err := e.env().Containers.DialContext(dctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(edgeagent.RelayPort)))
	cancel()
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	if _, err := io.WriteString(c, token+" "+addr+"\n"); err != nil {
		c.Close()
		return nil, err
	}
	// Read the status line byte by byte so no backend data is consumed.
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
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("relay: " + strings.TrimPrefix(s, "ERR "))}
	}
	return c, nil
}

// ensureLocked starts the edge container and the ingress if needed.
func (e *edge) ensureLocked(ctx context.Context) error {
	if e.stopped {
		return errors.New("lb is stopping")
	}
	rt, err := e.env().Containers.Runtime(ctx)
	if err != nil {
		return err
	}
	if e.id != "" {
		if c, err := rt.InspectContainer(ctx, e.id); err == nil && c.Running {
			return nil
		}
		e.id, e.pushed, e.extIP = "", "", ""
		e.nets = map[string]bool{}
	}
	if _, err := e.ingressLocked(); err != nil {
		return err
	}
	plane, err := e.env().Containers.Netplane(ctx)
	if err != nil {
		return err
	}
	svc, err := plane.Services(ctx)
	if err != nil {
		return err
	}
	if e.inAddr, err = plane.Addr(ctx, "lb-edge-in"); err != nil {
		return err
	}
	e.netName = rt.Name("lb")
	n, err := rt.InspectNetwork(ctx, e.netName)
	if errors.Is(err, runtime.ErrNotFound) {
		if _, err = rt.CreateNetwork(ctx, runtime.NetworkSpec{Name: e.netName, Internal: true, Labels: rt.Labels("lb", "edge", "network")}); err == nil {
			n, err = rt.InspectNetwork(ctx, e.netName)
		}
	}
	if err != nil {
		return fmt.Errorf("lb network: %w", err)
	}
	_, e.subnet, err = net.ParseCIDR(n.Subnet)
	if err != nil {
		return fmt.Errorf("lb network subnet %q: %w", n.Subnet, err)
	}
	if err := rt.EnsureImage(ctx, edgeagent.Image, e.env().Config.Offline); err != nil {
		return fmt.Errorf("lb-edge image %s: %w", edgeagent.Image, err)
	}
	bin, err := rt.AgentBinary(ctx)
	if err != nil {
		return err
	}
	if e.token == "" {
		var b [16]byte
		_, _ = rand.Read(b[:])
		e.token = hex.EncodeToString(b[:])
	}
	name := rt.Name("lb-edge")
	_ = rt.RemoveContainer(ctx, name, true)
	id, err := rt.CreateContainer(ctx, runtime.ContainerSpec{
		Name:       name,
		Image:      edgeagent.Image,
		Entrypoint: []string{agent.ContainerPath},
		Cmd:        []string{},
		Env: []string{
			agent.EnvVar + "=" + edgeagent.EdgeAgentName,
			"GCPEMU_LB_TOKEN=" + e.token,
			"GCPEMU_LB_SUBNET=" + n.Subnet,
		},
		Labels:   rt.Labels("lb", "edge", "edge"),
		Hostname: "lb-edge",
		CapAdd:   []string{"NET_ADMIN", "NET_BIND_SERVICE"},
		Sysctls:  map[string]string{"net.ipv4.ip_nonlocal_bind": "1"},
		Networks: []runtime.Attachment{{Network: e.netName}, {Network: svc.Name}},
	})
	if err != nil {
		return fmt.Errorf("lb-edge: %w", err)
	}
	if err := rt.CopyTo(ctx, id, "/", []runtime.File{{Name: strings.TrimPrefix(agent.ContainerPath, "/"), Mode: 0o755, Path: bin}}); err != nil {
		_ = rt.RemoveContainer(ctx, id, true)
		return fmt.Errorf("lb-edge: copy Sidecar agent: %w", err)
	}
	if err := rt.StartContainer(ctx, id); err != nil {
		_ = rt.RemoveContainer(ctx, id, true)
		return fmt.Errorf("lb-edge: start: %w", err)
	}
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := rt.WaitRunning(wctx, id); err != nil {
		_ = rt.RemoveContainer(ctx, id, true)
		return fmt.Errorf("lb-edge: %w", err)
	}
	c, err := rt.InspectContainer(ctx, id)
	if err != nil {
		return err
	}
	e.id, e.ip = id, c.IPs[e.netName]
	e.nets = map[string]bool{e.netName: true, svc.Name: true}
	// Wait for the control port.
	for {
		req, _ := http.NewRequestWithContext(wctx, http.MethodGet, "http://"+net.JoinHostPort(e.ip, strconv.Itoa(edgeagent.ControlPort))+"/healthz", nil)
		resp, err := e.control().Do(req)
		if err == nil {
			resp.Body.Close()
			break
		}
		select {
		case <-wctx.Done():
			return fmt.Errorf("lb-edge: control port not ready: %w", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	e.env().Log.Info("lb: edge container started", "container", name, "ip", e.ip)
	return nil
}

// ingressLocked starts the ingress that edge-relayed connections arrive on
// (PROXY v2) if needed and returns its host address.
func (e *edge) ingressLocked() (string, error) {
	if e.ingress == nil {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return "", err
		}
		e.ingress = ln
		e.env().Endpoints.Set("lb-edge-in", ln.Addr().String())
		go e.serveIngress(ln)
	}
	return e.ingress.Addr().String(), nil
}

// ingressAddr is ingressLocked for the --lb-bind relay (lan.go).
func (e *edge) ingressAddr() (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopped {
		return "", errors.New("lb is stopping")
	}
	return e.ingressLocked()
}

// pushLocked sends the desired listeners and routes to the agent.
func (e *edge) pushLocked(ctx context.Context) error {
	if e.id == "" {
		return nil
	}
	cfg := edgeagent.Config{Ingress: e.inAddr, Routes: e.routes, Public: e.public}
	for key, ip := range e.alias {
		cfg.Listeners = append(cfg.Listeners, edgeagent.Listener{IP: ip, Port: e.ports[key]})
	}
	sort.Slice(cfg.Listeners, func(i, j int) bool {
		a, b := cfg.Listeners[i], cfg.Listeners[j]
		return a.IP < b.IP || a.IP == b.IP && a.Port < b.Port
	})
	body, _ := json.Marshal(cfg)
	if string(body) == e.pushed {
		return nil
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+net.JoinHostPort(e.ip, strconv.Itoa(edgeagent.ControlPort))+"/config", bytes.NewReader(body))
	req.Header.Set(edgeagent.TokenHeader, e.token)
	resp, err := e.control().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("lb-edge config: %s", strings.TrimSpace(string(msg)))
	}
	e.pushed = string(body)
	return nil
}

// serveIngress accepts edge-relayed connections and hands them to the
// frontend's server.
func (e *edge) serveIngress(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			pc, err := edgeagent.AcceptProxied(c)
			if err != nil {
				c.Close()
				return
			}
			dst := pc.LocalAddr().String()
			e.mu.Lock()
			key := e.byAlias[dst]
			e.mu.Unlock()
			e.d.mu.Lock()
			var virt *chanListener
			if b := e.d.lan[dst]; key == "" && b != nil && b.relay {
				virt = b.l.relayed()
			} else {
				if key == "" {
					// An external forwarding rule's own address (#187).
					key = dst
				}
				if l := e.d.listeners[key]; l != nil {
					virt = l.relayed()
				}
			}
			e.d.mu.Unlock()
			if virt == nil || !virt.deliver(pc) {
				c.Close()
			}
		}()
	}
}

// routeLoop keeps the edge's pod routes in sync with GKE (FR-INT-001).
func (e *edge) routeLoop(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		e.syncRoutes(ctx)
	}
}

func (e *edge) syncRoutes(ctx context.Context) {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	e.retireIdle(ctx, time.Now())
	if e.d.s.options().DirectDial {
		return
	}
	podRoutes := e.d.s.podRoutes
	if podRoutes == nil {
		svc, ok := e.env().Lookup("gke")
		if !ok {
			return
		}
		g, ok := svc.(*gke.Service)
		if !ok {
			return
		}
		podRoutes = g.PodRoutes
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	prs := podRoutes(cctx)
	if ctx.Err() != nil {
		return // stopped, or the routes were read before a Reset
	}
	var pub []edgeagent.Listener
	if len(prs) > 0 {
		// Only GKE nodes are VPC workloads that call load balancers.
		pub = e.d.publicFrontends()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.id == "" && (len(prs) == 0 || len(pub) == 0 && !e.d.hasNEGEndpoints()) {
		e.routePublicLocked(cctx, nil)
		return
	}
	if err := e.ensureLocked(cctx); err != nil {
		e.env().Log.Warn("lb: edge relay unavailable", "err", err)
		return
	}
	vpcSvc, _ := e.env().Lookup("compute")
	vpc, _ := vpcSvc.(emu.VPC)
	rt, err := e.env().Containers.Runtime(cctx)
	if err != nil || vpc == nil {
		return
	}
	var routes []edgeagent.Route
	var pods []*net.IPNet
	used := map[string]bool{}
	for _, pr := range prs {
		used[pr.Subnetwork] = true
		sn, err := vpc.SubnetNetwork(cctx, pr.Subnetwork)
		if err != nil {
			continue
		}
		if err := e.joinLocked(cctx, rt, vpc, sn); err != nil {
			e.env().Log.Warn("lb: attaching edge to subnetwork", "network", sn.Name, "err", err)
			continue
		}
		if _, n, err := net.ParseCIDR(pr.CIDR); err == nil && pr.Via != "" {
			routes = append(routes, edgeagent.Route{CIDR: pr.CIDR, Via: pr.Via})
			pods = append(pods, n)
		}
	}
	for _, sn := range e.subnets {
		if !used[sn.SubnetworkName] {
			if err := e.leaveLocked(cctx, rt, vpc, sn); err != nil {
				e.env().Log.Warn("lb: detaching edge from subnetwork", "network", sn.Name, "err", err)
			}
		}
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].CIDR < routes[j].CIDR })
	e.routes, e.pods = routes, pods
	if len(pub) > 0 {
		if err := e.joinExternalLocked(cctx, rt); err != nil {
			e.env().Log.Warn("lb: attaching edge to the external network", "err", err)
			pub = nil
		}
	}
	e.public = pub
	if err := e.pushLocked(cctx); err != nil {
		e.env().Log.Warn("lb: programming edge routes", "err", err)
		return
	}
	e.routePublicLocked(cctx, pub)
}

// publicFrontends returns the listening external IPv4 forwarding rules.
func (d *dataplane) publicFrontends() []edgeagent.Listener {
	cfg := d.cfg.Load()
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []edgeagent.Listener
	for key, fe := range cfg.fronts {
		if cfg.public[fe.ip] && d.listeners[key] != nil && net.ParseIP(fe.ip).To4() != nil {
			out = append(out, edgeagent.Listener{IP: fe.ip, Port: fe.port})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		return a.IP < b.IP || a.IP == b.IP && a.Port < b.Port
	})
	return out
}

// joinExternalLocked attaches the edge to the external network, where it
// serves external forwarding rule addresses to VPC workloads.
func (e *edge) joinExternalLocked(ctx context.Context, rt *runtime.Manager) error {
	plane, err := e.env().Containers.Netplane(ctx)
	if err != nil {
		return err
	}
	ext, err := plane.External(ctx)
	if err != nil {
		return err
	}
	if e.nets[ext.Name] && e.extIP != "" {
		return nil
	}
	if err := rt.Connect(ctx, ext.Name, e.id, ""); err != nil && !strings.Contains(err.Error(), "already exists") {
		return err
	}
	c, err := rt.InspectContainer(ctx, e.id)
	if err != nil {
		return err
	}
	if c.IPs[ext.Name] == "" {
		return fmt.Errorf("no address on %s", ext.Name)
	}
	e.nets[ext.Name], e.extIP = true, c.IPs[ext.Name]
	return nil
}

// routePublicLocked asks the PublicRouters to route the addresses of pub
// via the edge, or to remove the routes when pub is empty.
func (e *edge) routePublicLocked(ctx context.Context, pub []edgeagent.Listener) {
	via, ips := "", []string(nil)
	if len(pub) > 0 {
		via = e.extIP
		for _, l := range pub {
			if len(ips) == 0 || ips[len(ips)-1] != l.IP {
				ips = append(ips, l.IP)
			}
		}
	}
	want := ""
	if via != "" {
		want = via + " " + strings.Join(ips, " ")
	}
	if want == e.routed {
		return
	}
	ok := true
	for _, name := range []string{"gke", "compute"} {
		svc, found := e.env().Lookup(name)
		if !found {
			continue
		}
		if r, isRouter := svc.(emu.PublicRouter); isRouter {
			if err := r.RoutePublic(ctx, via, ips); err != nil {
				e.env().Log.Warn("lb: routing forwarding rule addresses via the edge", "service", name, "err", err)
				ok = false
			}
		}
	}
	if ok {
		e.routed = want
	}
}

// edgeIPOwner owns the edge's addresses in compute's allocations.
const edgeIPOwner = "lb-edge"

// joinLocked attaches the edge to subnetwork network sn. On a network that
// is not Translated the container network is on the Declared range, so the
// edge joins with an address compute allocated for it (#148): compute then
// neither hands it to another resource nor lets an internal address be
// reserved on it. On a Translated network the runtime picks a Fallback-range
// address, which compute never allocates from; pod routes go via the nodes'
// Fallback-range addresses there.
func (e *edge) joinLocked(ctx context.Context, rt *runtime.Manager, vpc emu.VPC, sn emu.SubnetNet) error {
	if e.nets[sn.Name] {
		return nil
	}
	ip := ""
	if !sn.Translated {
		var err error
		if ip, err = vpc.AllocateIP(ctx, sn, edgeIPOwner); err != nil {
			return err
		}
		e.subnets[sn.Name] = sn
	}
	if err := rt.Connect(ctx, sn.Name, e.id, ip); err != nil && !strings.Contains(err.Error(), "already exists") {
		if ip != "" {
			// Another container may hold it: allocate afresh next time,
			// skipping the addresses held on the network.
			_ = vpc.ReleaseIP(ctx, sn, edgeIPOwner)
			delete(e.subnets, sn.Name)
		}
		return err
	}
	e.nets[sn.Name] = true
	e.subnets[sn.Name] = sn
	return nil
}

// leaveLocked detaches the Edge relay from subnetwork network sn and frees its
// address there, once no pod route needs it: the subnetwork can then be
// deleted while load balancers remain (#683).
func (e *edge) leaveLocked(ctx context.Context, rt *runtime.Manager, vpc emu.VPC, sn emu.SubnetNet) error {
	if e.nets[sn.Name] && e.id != "" {
		if err := rt.Disconnect(ctx, sn.Name, e.id); err != nil && !errors.Is(err, runtime.ErrNotFound) && !strings.Contains(err.Error(), "not connected") {
			return err
		}
	}
	delete(e.nets, sn.Name)
	if !sn.Translated {
		if err := vpc.ReleaseIP(ctx, sn, edgeIPOwner); err != nil {
			return err
		}
	}
	delete(e.subnets, sn.Name)
	return nil
}

// leaveSubnetwork is compute's SubnetReleaser for the Edge relay's addresses:
// compute calls it when they are all that keeps subnetwork from being
// deleted, which may be before the next route pass sees its pod routes
// gone.
func (e *edge) leaveSubnetwork(ctx context.Context, subnetwork string) error {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, sn := range e.subnets {
		if sn.SubnetworkName != subnetwork {
			continue
		}
		vpcSvc, _ := e.env().Lookup("compute")
		vpc, ok := vpcSvc.(emu.VPC)
		if !ok {
			return errors.New("no compute service")
		}
		var rt *runtime.Manager
		if e.nets[sn.Name] && e.id != "" {
			var err error
			if rt, err = e.env().Containers.Runtime(ctx); err != nil {
				return err
			}
		}
		return e.leaveLocked(ctx, rt, vpc, sn)
	}
	return nil
}

// releaseLocked frees the edge's addresses in compute's allocations.
func (e *edge) releaseLocked(ctx context.Context) {
	if len(e.subnets) == 0 {
		return
	}
	vpcSvc, _ := e.env().Lookup("compute")
	if vpc, ok := vpcSvc.(emu.VPC); ok {
		for name, sn := range e.subnets {
			if !sn.Translated {
				if err := vpc.ReleaseIP(ctx, sn, edgeIPOwner); err != nil {
					e.env().Log.Warn("lb: releasing edge address", "network", name, "err", err)
					continue
				}
			}
			delete(e.subnets, name)
		}
	}
}

// retireIdle removes the edge container and its network once no
// forwarding rule is mapped to it and no backend has pod endpoints for
// edgeIdleGrace, so deleting the last load balancer leaves no containers
// behind (SRS 11.2 step 10). The next add or pod route starts it again.
func (e *edge) retireIdle(ctx context.Context, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.id == "" || e.stopped || len(e.alias) > 0 || len(e.public) > 0 || e.d.hasNEGEndpoints() {
		e.idleSince = time.Time{}
		return
	}
	if e.idleSince.IsZero() {
		e.idleSince = now
		return
	}
	if now.Sub(e.idleSince) < edgeIdleGrace {
		return
	}
	rt, err := e.env().Containers.Runtime(ctx)
	if err != nil {
		return
	}
	if err := rt.RemoveContainer(ctx, e.id, true); err != nil {
		e.env().Log.Warn("lb: removing idle edge container", "err", err)
		return
	}
	if e.netName != "" {
		_ = rt.RemoveNetwork(ctx, e.netName)
	}
	e.releaseLocked(ctx)
	e.env().Log.Info("lb: idle edge container removed")
	e.id, e.ip, e.pushed, e.extIP = "", "", "", ""
	e.nets = map[string]bool{}
	e.routes, e.pods = nil, nil
	e.idleSince = time.Time{}
}

func (e *edge) close(ctx context.Context) {
	e.mu.Lock()
	e.stopped = true
	id := e.id
	e.id = ""
	ln := e.ingress
	e.ingress = nil
	e.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
	if id == "" {
		return
	}
	if rt, err := e.env().Containers.Runtime(ctx); err == nil {
		_ = rt.RemoveContainer(ctx, id, true)
	}
}

// hasNEGEndpoints reports whether any backend service has NEG endpoints.
func (d *dataplane) hasNEGEndpoints() bool {
	for _, b := range d.cfg.Load().svcs {
		if len(*b.eps.Load()) > 0 {
			return true
		}
	}
	return false
}
