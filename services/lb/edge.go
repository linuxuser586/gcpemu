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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linuxuser586/gcpemu/internal/agent"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/services/gke"
)

// The lb-edge container (FR-LB-002, FR-INT-001): started on demand when a
// forwarding rule needs a privileged port the emulator cannot bind, or when
// a backend's endpoints are GKE pod IPs the host cannot route to. It joins
// a labelled "lb" network (forwarding rule addresses), the services network
// (to reach the emulator's edge ingress) and every VPC subnetwork with pod
// routes. The in-process proxy stays the only L7 implementation: the edge
// only relays TCP (edgeagent.go).

// Images lists the container images the lb service launches (pinned by
// tag; digests are pinned at release).
var Images = map[string]string{
	"lb-edge": edgeImage,
}

const edgeImage = "rancher/k3s:v1.36.5-k3s1"

type edge struct {
	d *dataplane

	mu      sync.Mutex
	rtTried bool
	rtOK    bool
	id      string
	ip      string // container address on the lb network
	subnet  *net.IPNet
	netName string
	token   string
	ingress net.Listener
	inAddr  string            // ingress address on the services network
	alias   map[string]string // frontend key → alias IP
	byAlias map[string]string // alias ip:port → frontend key
	ports   map[string]int    // frontend key → port
	routes  []edgeRoute
	pods    []*net.IPNet
	nets    map[string]bool
	pushed  string
	stopped bool
}

func newEdge(d *dataplane) *edge {
	e := &edge{d: d, alias: map[string]string{}, byAlias: map[string]string{}, ports: map[string]int{}, nets: map[string]bool{}}
	go e.routeLoop(d.ctx)
	return e
}

func (e *edge) env() *emu.Env { return e.d.s.env }

// available reports whether a container runtime can run the edge.
func (e *edge) available(ctx context.Context) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.d.s.opts.Edge {
		return true
	}
	if !e.rtTried {
		e.rtTried = true
		if e.env().Containers != nil {
			c, cancel := context.WithTimeout(ctx, 5*time.Second)
			_, err := e.env().Containers.Runtime(c)
			cancel()
			e.rtOK = err == nil
		}
	}
	return e.rtOK
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
		ip = e.allocAliasLocked(fe.ip)
		e.alias[fe.key] = ip
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
	delete(e.alias, key)
	delete(e.ports, key)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = e.pushLocked(ctx)
}

// allocAliasLocked picks an address for a forwarding rule IP: the same
// alias for every port of one IP, allocated from the top of the subnet.
func (e *edge) allocAliasLocked(frIP string) string {
	for key, ip := range e.alias {
		if h, _, _ := net.SplitHostPort(key); h == frIP {
			return ip
		}
	}
	used := map[string]bool{e.ip: true}
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
	dl := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	c, err := dl.DialContext(ctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(edgeRelayPort)))
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
		e.id, e.pushed = "", ""
		e.nets = map[string]bool{}
	}
	if e.ingress == nil {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		e.ingress = ln
		e.env().Endpoints.Set("lb-edge-in", ln.Addr().String())
		go e.serveIngress(ln)
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
	if err := rt.EnsureImage(ctx, edgeImage, e.env().Config.Offline); err != nil {
		return fmt.Errorf("lb-edge image %s: %w", edgeImage, err)
	}
	bin, err := agent.Binary()
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
		Image:      edgeImage,
		Entrypoint: []string{agent.ContainerPath},
		Cmd:        []string{},
		Env: []string{
			agent.EnvVar + "=" + edgeAgentName,
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
		return fmt.Errorf("lb-edge: copy agent: %w", err)
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
		req, _ := http.NewRequestWithContext(wctx, http.MethodGet, "http://"+net.JoinHostPort(e.ip, strconv.Itoa(edgeControlPort))+"/healthz", nil)
		resp, err := http.DefaultClient.Do(req)
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

// pushLocked sends the desired listeners and routes to the agent.
func (e *edge) pushLocked(ctx context.Context) error {
	if e.id == "" {
		return nil
	}
	cfg := edgeConfig{Ingress: e.inAddr, Routes: e.routes}
	for key, ip := range e.alias {
		cfg.Listeners = append(cfg.Listeners, edgeListener{IP: ip, Port: e.ports[key]})
	}
	sort.Slice(cfg.Listeners, func(i, j int) bool {
		a, b := cfg.Listeners[i], cfg.Listeners[j]
		return a.IP < b.IP || a.IP == b.IP && a.Port < b.Port
	})
	body, _ := json.Marshal(cfg)
	if string(body) == e.pushed {
		return nil
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+net.JoinHostPort(e.ip, strconv.Itoa(edgeControlPort))+"/config", bytes.NewReader(body))
	req.Header.Set(edgeTokenHeader, e.token)
	resp, err := http.DefaultClient.Do(req)
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
			pc, err := acceptProxied(c)
			if err != nil {
				c.Close()
				return
			}
			e.mu.Lock()
			key := e.byAlias[pc.LocalAddr().String()]
			e.mu.Unlock()
			e.d.mu.Lock()
			l := e.d.listeners[key]
			e.d.mu.Unlock()
			if l == nil || l.virt == nil || !l.virt.deliver(pc) {
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
	if e.d.s.opts.DirectDial {
		return
	}
	svc, ok := e.env().Lookup("gke")
	if !ok {
		return
	}
	g, ok := svc.(*gke.Service)
	if !ok {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	prs := g.PodRoutes(cctx)
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(prs) == 0 && e.id == "" {
		return
	}
	if len(prs) > 0 && !e.needed() {
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
	var routes []edgeRoute
	var pods []*net.IPNet
	for _, pr := range prs {
		sn, err := vpc.SubnetNetwork(cctx, pr.Subnetwork)
		if err != nil {
			continue
		}
		if !e.nets[sn.Name] {
			if err := rt.Connect(cctx, sn.Name, e.id, ""); err != nil && !strings.Contains(err.Error(), "already exists") {
				e.env().Log.Warn("lb: attaching edge to subnetwork", "network", sn.Name, "err", err)
				continue
			}
			e.nets[sn.Name] = true
		}
		if _, n, err := net.ParseCIDR(pr.CIDR); err == nil && pr.Via != "" {
			routes = append(routes, edgeRoute{CIDR: pr.CIDR, Via: pr.Via})
			pods = append(pods, n)
		}
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].CIDR < routes[j].CIDR })
	e.routes, e.pods = routes, pods
	if err := e.pushLocked(cctx); err != nil {
		e.env().Log.Warn("lb: programming edge routes", "err", err)
	}
}

// needed reports whether any backend endpoint is a pod IP (callers hold e.mu).
func (e *edge) needed() bool {
	if e.id != "" {
		return true
	}
	return e.d.hasNEGEndpoints()
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
