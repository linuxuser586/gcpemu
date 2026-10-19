package gke

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	mdns "github.com/miekg/dns"

	"github.com/linuxuser586/gcpemu/internal/loopback"
	"github.com/linuxuser586/gcpemu/internal/netplane"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// The Host attachment, outward (ADR 0014): host clients reach a cluster's
// ClusterIP Services by name. The emulated DNS hands host queries for
// names under svc.cluster.local and gke.internal to answerHost, which
// answers
//
//   - <service>.<namespace>.svc.<cluster>.<location>.<project>.gke.internal
//     for the cluster's Service, and
//   - <service>.<namespace>.svc.cluster.local while exactly one running
//     cluster has that Service (as ADR 0013 does for forwarding rule env
//     vars); when a second one has it too, the short name is NXDOMAIN, a
//     warning is logged and the Admin API lists the collision,
//
// with an A record for a Loopback alias the Service gets on its first A
// query, from the pool forwarding rules, Cloud SQL instances and
// LoadBalancer Services share (loopCandidates). It keeps the alias until it
// is deleted, across restarts (nsHostSvcs), and the emulator serves each of
// its TCP ports there, connecting through a node's ClusterIP relay
// (clusteriprelay.go) to its ClusterIP, which only nodes route; the netplane
// reaches the node through the VM relay on a VM runtime. A privileged port
// the host refuses to bind is not served. Headless Services are not
// answered; ExternalName Services are answered with their CNAME. With no
// free alias, on a host without aliases, or in a Containerized Instance,
// whose loopback is its own, the answer is SERVFAIL and the reason is
// logged and listed. Each cluster's controller (hostServicesLoop) serves
// the stored aliases at start, follows port changes and releases the
// aliases of deleted Services.
//
// Admin API: GET /_emu/v1/host/services (`gcpemu status`, `gcpemu doctor`).

const (
	// nsHostSvcs holds each Service's alias: CLUSTERKEY/NAMESPACE/NAME →
	// hostSvcRecord.
	nsHostSvcs = "gke/hostsvcs"
	// hostSvcTTL is the TTL of answers, short so that a short name that
	// stops resolving does so soon.
	hostSvcTTL = 5
	// hostLoopPrefix marks a Service's alias among loopCandidates names.
	hostLoopPrefix = "svc:"
	shortSuffix    = ".svc.cluster.local."
	qualSuffix     = ".gke.internal."
)

// hostSuffixes are the names the emulated DNS hands to answerHost.
var hostSuffixes = []string{"svc.cluster.local.", "gke.internal."}

// hostAliases reports whether the host has Loopback aliases; tests replace
// it.
var hostAliases = loopback.Aliases

// errPoolExhausted is why a Service gets no alias when none is free.
var errPoolExhausted = errors.New("no free loopback alias in 127.0.0.2-127.0.0.254")

type hostSvcRecord struct {
	IP string `json:"ip"`
}

// hostSvc is a Service served at its alias.
type hostSvc struct {
	ip    string
	sig   string // what it serves, to notice changes
	ports []int
	ex    []*netplane.Exposed
}

func (h *hostSvc) close() {
	for _, e := range h.ex {
		_ = e.Close()
	}
}

// hostName is a Service a host query names; key is "" for a short name.
type hostName struct{ key, ns, name string }

// parseHostName parses a lower-case, fully qualified query name.
func parseHostName(q string) (hostName, bool) {
	if rest, ok := strings.CutSuffix(q, shortSuffix); ok {
		if l := strings.Split(rest, "."); len(l) == 2 && l[0] != "" && l[1] != "" {
			return hostName{ns: l[1], name: l[0]}, true
		}
		return hostName{}, false
	}
	if rest, ok := strings.CutSuffix(q, qualSuffix); ok {
		l := strings.Split(rest, ".")
		if len(l) == 6 && l[2] == "svc" && !slices.Contains(l, "") {
			return hostName{key: clusterKey(l[5], l[4], l[3]), ns: l[1], name: l[0]}, true
		}
	}
	return hostName{}, false
}

// qualifiedName is the gke.internal name of a Service, without the final
// dot.
func qualifiedName(id string) string {
	l := strings.Split(id, "/") // project/location/cluster/namespace/name
	return l[4] + "." + l[3] + ".svc." + l[2] + "." + l[1] + "." + l[0] + strings.TrimSuffix(qualSuffix, ".")
}

// shortName is the svc.cluster.local name of a Kubernetes Service, without
// the final dot.
func shortName(ns, name string) string {
	return name + "." + ns + strings.TrimSuffix(shortSuffix, ".")
}

// hostAddressed reports whether a host query for svc gets an address.
func hostAddressed(svc *kubeService) bool {
	return svc.Spec.Type != "ExternalName" && svc.Spec.ClusterIP != "" && svc.Spec.ClusterIP != "None"
}

// hostSig is what serving svc depends on.
func hostSig(svc *kubeService) string {
	return fmt.Sprint(svc.Spec.ClusterIP, hostPorts(svc))
}

// hostPorts are svc's TCP ports.
func hostPorts(svc *kubeService) []int {
	var out []int
	for _, p := range svc.Spec.Ports {
		if p.Protocol == "" || p.Protocol == "TCP" {
			out = append(out, p.Port)
		}
	}
	return out
}

// installHostNames hands host queries for Service names to answerHost.
func (s *Service) installHostNames() {
	svc, ok := s.env.Lookup("dns")
	if !ok {
		return
	}
	if d, ok := svc.(interface {
		SetHostNames([]string, func(context.Context, string, uint16) ([]mdns.RR, int))
	}); ok {
		d.SetHostNames(hostSuffixes, s.answerHost)
	}
}

// answerHost answers a host query for name, lower case and fully
// qualified, under one of hostSuffixes.
func (s *Service) answerHost(ctx context.Context, name string, qtype uint16) ([]mdns.RR, int) {
	h, ok := parseHostName(name)
	if !ok {
		return nil, mdns.RcodeNameError
	}
	key, svc, err := s.hostLookup(ctx, h)
	if err != nil {
		s.env.Log.Warn("gke: cannot answer a host query for a Kubernetes Service", "name", name, "err", err)
		return nil, mdns.RcodeServerFailure
	}
	if svc == nil {
		return nil, mdns.RcodeNameError
	}
	hdr := mdns.RR_Header{Name: name, Class: mdns.ClassINET, Ttl: hostSvcTTL}
	if svc.Spec.Type == "ExternalName" {
		hdr.Rrtype = mdns.TypeCNAME
		return []mdns.RR{&mdns.CNAME{Hdr: hdr, Target: mdns.Fqdn(strings.ToLower(svc.Spec.ExternalName))}}, mdns.RcodeSuccess
	}
	if !hostAddressed(svc) {
		return nil, mdns.RcodeNameError
	}
	if qtype != mdns.TypeA && qtype != mdns.TypeANY {
		return nil, mdns.RcodeSuccess
	}
	ip, err := s.hostAlias(ctx, key, svc)
	if err != nil {
		s.env.Log.Warn("gke: no Loopback alias for a Kubernetes Service; host queries get SERVFAIL", "name", name, "err", err)
		return nil, mdns.RcodeServerFailure
	}
	hdr.Rrtype = mdns.TypeA
	return []mdns.RR{&mdns.A{Hdr: hdr, A: net.ParseIP(ip)}}, mdns.RcodeSuccess
}

// hostLookup finds the Service h names: in its cluster or, for a short
// name, in the one running cluster that has it. svc is nil when none has
// it, or more than one does.
func (s *Service) hostLookup(ctx context.Context, h hostName) (key string, svc *kubeService, err error) {
	keys := []string{h.key}
	if h.key == "" {
		keys = s.runningClusters()
	} else if _, err := s.loadKey(h.key); err != nil {
		return "", nil, nil
	} else if !s.running(h.key) {
		return "", nil, fmt.Errorf("cluster %s is not running", h.key)
	}
	var found []string
	for _, k := range keys {
		kc, err := s.kube(k)
		if err != nil {
			if h.key != "" {
				return "", nil, err
			}
			continue
		}
		var v kubeService
		switch err := kc.get(ctx, "/api/v1/namespaces/"+h.ns+"/services/"+h.name, &v); {
		case isKubeNotFound(err):
			continue
		case err != nil:
			return "", nil, fmt.Errorf("cluster %s: %w", k, err)
		}
		found = append(found, k)
		key, svc = k, &v
	}
	if len(found) > 1 {
		s.env.Log.Debug("gke: short Kubernetes Service name in more than one cluster", "service", h.ns+"/"+h.name, "clusters", found)
		return "", nil, nil
	}
	return key, svc, nil
}

// runningClusters are the keys of the clusters whose controllers run,
// sorted: those whose Kubernetes Service names hostServicesLoop records.
func (s *Service) runningClusters() []string {
	s.mu.Lock()
	keys := make([]string, 0, len(s.clusters))
	for k, rt := range s.clusters {
		rt.mu.Lock()
		if rt.cancel != nil && rt.kube != nil {
			keys = append(keys, k)
		}
		rt.mu.Unlock()
	}
	s.mu.Unlock()
	sort.Strings(keys)
	return keys
}

// running reports whether the cluster's controllers run. Stopping or
// deleting a cluster cancels them before it closes or forgets its
// Kubernetes Services, so a Loopback alias served while it runs has a
// controller that closes it.
func (s *Service) running(key string) bool {
	return slices.Contains(s.runningClusters(), key)
}

// hostAlias returns svc's alias, serving it there first if it is not.
func (s *Service) hostAlias(ctx context.Context, key string, svc *kubeService) (string, error) {
	id := key + "/" + svc.Metadata.Namespace + "/" + svc.Metadata.Name
	s.hostMu.Lock()
	defer s.hostMu.Unlock()
	if h := s.hostSvcs[id]; h != nil {
		return h.ip, nil
	}
	if !s.running(key) {
		return "", fmt.Errorf("cluster %s is not running", key)
	}
	return s.serveHostLocked(ctx, key, id, svc, false)
}

// hostPlane is the netplane that serves Services to host clients, or why
// none can.
func (s *Service) hostPlane(ctx context.Context) (*netplane.Plane, error) {
	if s.env.Containers == nil {
		return nil, errors.New("no container runtime")
	}
	np, err := s.env.Containers.Netplane(ctx)
	if err != nil {
		return nil, err
	}
	if rt := np.Runtime(); rt != nil && rt.Self() != "" {
		return nil, errors.New("a Containerized Instance has no host loopback to serve Services on")
	}
	if !hostAliases() {
		return nil, fmt.Errorf("the host has no loopback aliases; run `%s`", loopback.InstallHint)
	}
	return np, nil
}

// serveHostLocked serves svc's ports at its stored Loopback alias or, but
// with stored, a free one, stores the alias and returns it. On failure the
// reason is kept for the Admin API. Callers hold hostMu.
func (s *Service) serveHostLocked(ctx context.Context, key, id string, svc *kubeService, stored bool) (string, error) {
	ip, err := s.serveHostOnAlias(ctx, key, id, svc, stored)
	if err != nil {
		s.hostFail[id] = err
		return "", err
	}
	delete(s.hostFail, id)
	return ip, nil
}

// serveHostOnAlias serves svc's ports on the first Loopback alias where
// they all bind: its stored one first, and with stored only that one, so
// that a restart never moves it.
func (s *Service) serveHostOnAlias(ctx context.Context, key, id string, svc *kubeService, stored bool) (string, error) {
	np, err := s.hostPlane(ctx)
	if err != nil {
		return "", err
	}
	prev := s.hostRecords(id)[id]
	cands := s.loopCandidates(hostLoopPrefix+id, prev)
	if stored {
		cands = []string{prev}
	}
	if len(cands) == 0 {
		return "", errPoolExhausted
	}
	ports := hostPorts(svc)
	var errs []error
	for _, ip := range cands {
		h := &hostSvc{ip: ip, sig: hostSig(svc)}
		var err error
		for _, p := range ports {
			var ln net.Listener
			if ln, err = listenAlias(ip, p); loopback.Refused(err) != loopback.NotRefused {
				s.env.Log.Warn("gke: the host refuses a Kubernetes Service port; host clients cannot reach it", "service", id, "address", ip, "port", p, "err", err)
				err = nil
				continue
			} else if err != nil {
				break
			}
			h.ex = append(h.ex, np.ServeDial(ln, s.hostDial(np, key, svc.Spec.ClusterIP, p)))
			h.ports = append(h.ports, p)
		}
		if err != nil {
			h.close()
			errs = append(errs, err)
			continue
		}
		if err := s.env.Store.Update(func(tx store.Tx) error {
			return store.PutJSON(tx, nsHostSvcs, id, hostSvcRecord{IP: ip})
		}); err != nil {
			h.close()
			return "", err
		}
		s.hostSvcs[id] = h
		s.env.Log.Info("gke: Kubernetes Service served to host clients", "service", id, "address", ip, "ports", h.ports)
		return ip, nil
	}
	if stored {
		return "", fmt.Errorf("its ports do not bind on its Loopback alias %s: %w", prev, errors.Join(errs...))
	}
	return "", fmt.Errorf("its ports bind on no free Loopback alias: %w", errors.Join(errs...))
}

// listenAlias binds ip:port. On macOS, which binds a privileged port only
// on the wildcard address, it binds that and serves only clients of ip.
func listenAlias(ip string, port int) (net.Listener, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
	if loopback.Refused(err) != loopback.PrivilegedWildcard {
		return ln, err
	}
	wln, werr := net.Listen("tcp", net.JoinHostPort("0.0.0.0", strconv.Itoa(port)))
	if werr != nil {
		return nil, err
	}
	return &loopback.LocalFilter{Listener: wln, Allow: func(a net.Addr) bool {
		ta, ok := a.(*net.TCPAddr)
		return ok && ta.IP.String() == ip
	}}, nil
}

// hostDial connects to clusterIP:port through the ClusterIP relay of the
// first of the cluster's nodes that answers.
func (s *Service) hostDial(np *netplane.Plane, key, clusterIP string, port int) func(context.Context) (net.Conn, error) {
	target := net.JoinHostPort(clusterIP, strconv.Itoa(port))
	return func(ctx context.Context) (net.Conn, error) {
		rec, err := s.loadKey(key)
		if err != nil {
			return nil, err
		}
		errs := []error{errors.New("no node")}
		for _, n := range rec.Int.Nodes {
			ip := n.dialIP()
			if ip == "" {
				continue
			}
			dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			c, err := dialClusterIPRelay(dctx, np.DialContext, net.JoinHostPort(ip, strconv.Itoa(clusterIPRelayPort)), rec.Int.Secret, target)
			cancel()
			if err == nil {
				return c, nil
			}
			errs = append(errs, fmt.Errorf("node %s: %w", n.Name, err))
		}
		return nil, errors.Join(errs[1:]...)
	}
}

// hostRecords returns the stored aliases of the Services whose IDs start
// with prefix.
func (s *Service) hostRecords(prefix string) map[string]string {
	out := map[string]string{}
	_ = s.env.Store.View(func(tx store.Tx) error {
		tx.Scan(nsHostSvcs, prefix, func(k string, b []byte) bool {
			var r hostSvcRecord
			if json.Unmarshal(b, &r) == nil && r.IP != "" {
				out[k] = r.IP
			}
			return true
		})
		return nil
	})
	return out
}

// hostServicesLoop serves the cluster's Services with stored aliases, and
// follows them, while ctx lives.
func (s *Service) hostServicesLoop(ctx context.Context, key string) {
	if s.env.Containers == nil {
		return
	}
	np, err := s.env.Containers.Netplane(ctx)
	if err != nil {
		return
	}
	defer s.closeHostServices(key)
	t := time.NewTicker(negInterval)
	defer t.Stop()
	for {
		if err := s.syncHostServices(ctx, np, key); err != nil && ctx.Err() == nil {
			s.env.Log.Debug("gke: host Kubernetes Service sync", "cluster", key, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// syncHostServices records the cluster's Service names for collisions,
// releases the aliases of its Services that went or lost their address,
// serves again those whose ports changed and serves the stored aliases
// not served yet.
func (s *Service) syncHostServices(ctx context.Context, np *netplane.Plane, key string) error {
	kc, err := s.kube(key)
	if err != nil {
		return err
	}
	var list kubeList[kubeService]
	if err := kc.get(ctx, "/api/v1/services", &list); err != nil {
		return err
	}
	names := map[string]bool{}
	svcs := map[string]*kubeService{}
	for i := range list.Items {
		svc := &list.Items[i]
		id := svc.Metadata.Namespace + "/" + svc.Metadata.Name
		if hostAddressed(svc) || svc.Spec.Type == "ExternalName" {
			names[id] = true
		}
		if hostAddressed(svc) {
			svcs[key+"/"+id] = svc
		}
	}
	stored := s.hostRecords(key + "/")
	s.hostMu.Lock()
	defer s.hostMu.Unlock()
	// Stopping or deleting the cluster cancels ctx before it takes hostMu
	// to close or forget the cluster's Kubernetes Services.
	if ctx.Err() != nil {
		return ctx.Err()
	}
	s.setHostNamesLocked(key, names)
	var errs []error
	for id := range s.hostFail {
		if strings.HasPrefix(id, key+"/") && svcs[id] == nil {
			delete(s.hostFail, id)
		}
	}
	for id, h := range s.hostSvcs {
		if !strings.HasPrefix(id, key+"/") {
			continue
		}
		switch svc := svcs[id]; {
		case svc == nil:
			s.dropHostLocked(id)
		case h.sig != hostSig(svc):
			h.close()
			delete(s.hostSvcs, id)
			if _, err := s.serveHostLocked(ctx, key, id, svc, true); err != nil {
				errs = append(errs, err)
			}
		}
	}
	for id := range stored {
		if s.hostSvcs[id] != nil {
			continue
		}
		if svc := svcs[id]; svc == nil {
			s.dropHostLocked(id)
		} else if _, err := s.serveHostLocked(ctx, key, id, svc, true); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// dropHostLocked stops serving a Service and releases its alias. Callers
// hold hostMu.
func (s *Service) dropHostLocked(id string) {
	ip := ""
	if h := s.hostSvcs[id]; h != nil {
		h.close()
		ip = h.ip
		delete(s.hostSvcs, id)
	}
	delete(s.hostFail, id)
	_ = s.env.Store.Update(func(tx store.Tx) error { return tx.Delete(nsHostSvcs, id) })
	s.env.Log.Info("gke: Kubernetes Service gone; Loopback alias released", "service", id, "address", ip)
}

// closeHostServices stops serving the cluster's Kubernetes Services,
// keeping their Loopback aliases, and drops its names, which DNS no longer
// looks up while the cluster is not running.
func (s *Service) closeHostServices(key string) {
	s.hostMu.Lock()
	defer s.hostMu.Unlock()
	s.setHostNamesLocked(key, nil)
	for id, h := range s.hostSvcs {
		if strings.HasPrefix(id, key+"/") {
			h.close()
			delete(s.hostSvcs, id)
		}
	}
}

// forgetHostCluster releases a deleted cluster's Loopback aliases and
// names.
func (s *Service) forgetHostCluster(key string) {
	s.hostMu.Lock()
	defer s.hostMu.Unlock()
	s.setHostNamesLocked(key, nil)
	for id := range s.hostRecords(key + "/") {
		s.dropHostLocked(id)
	}
	for id := range s.hostSvcs {
		if strings.HasPrefix(id, key+"/") {
			s.dropHostLocked(id)
		}
	}
	for id := range s.hostFail {
		if strings.HasPrefix(id, key+"/") {
			delete(s.hostFail, id)
		}
	}
}

// resetHost forgets every Kubernetes Service; Reset has stopped the
// controllers that served them, and the Instance resets the store.
func (s *Service) resetHost() {
	s.hostMu.Lock()
	defer s.hostMu.Unlock()
	for _, h := range s.hostSvcs {
		h.close()
	}
	s.hostSvcs = map[string]*hostSvc{}
	s.hostNames = map[string]map[string]bool{}
	s.hostWarned = map[string]bool{}
	s.hostFail = map[string]error{}
}

// setHostNamesLocked records the namespace/name of the cluster's answered
// Kubernetes Services (nil for a cluster that is not running), and logs
// each short name that starts or stops colliding. Callers hold hostMu.
func (s *Service) setHostNamesLocked(key string, names map[string]bool) {
	if names == nil {
		delete(s.hostNames, key)
	} else {
		s.hostNames[key] = names
	}
	cols := s.collisionsLocked()
	for short, keys := range cols {
		if !s.hostWarned[short] {
			s.hostWarned[short] = true
			s.env.Log.Warn("gke: a Kubernetes Service name is in more than one cluster; its short name no longer resolves for host clients, use the gke.internal names",
				"name", short, "clusters", keys)
		}
	}
	for short := range s.hostWarned {
		if cols[short] == nil {
			delete(s.hostWarned, short)
			s.env.Log.Info("gke: a short Kubernetes Service name resolves again for host clients", "name", short)
		}
	}
}

// collisionsLocked maps each short name (without the final dot) that more
// than one cluster has to those clusters' keys, sorted. Callers hold
// hostMu.
func (s *Service) collisionsLocked() map[string][]string {
	by := map[string][]string{}
	for key, names := range s.hostNames {
		for id := range names {
			ns, name, _ := strings.Cut(id, "/")
			short := shortName(ns, name)
			by[short] = append(by[short], key)
		}
	}
	for short, keys := range by {
		if len(keys) < 2 {
			delete(by, short)
			continue
		}
		sort.Strings(keys)
	}
	return by
}

// hostServicesJSON is GET /_emu/v1/host/services.
type hostServicesJSON struct {
	// Services hold aliases.
	Services   []hostServiceJSON   `json:"services"`
	Collisions []hostCollisionJSON `json:"collisions"`
	// Unserved are the Services a host query found but that have no
	// alias, and why.
	Unserved      []hostUnservedJSON `json:"unserved"`
	PoolExhausted bool               `json:"poolExhausted"`
}

type hostServiceJSON struct {
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace"`
	Service   string `json:"service"`
	Name      string `json:"name"`
	ShortName string `json:"shortName,omitempty"`
	Address   string `json:"address"`
	Ports     []int  `json:"ports"`
}

type hostCollisionJSON struct {
	Name     string   `json:"name"`
	Clusters []string `json:"clusters"`
}

type hostUnservedJSON struct {
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace"`
	Service   string `json:"service"`
	Reason    string `json:"reason"`
}

// clusterPath is the resource name of the cluster key.
func clusterPath(key string) string {
	l := strings.Split(key, "/")
	return ref{Project: l[0], Location: l[1], Cluster: l[2]}.clusterName()
}

// splitHostID splits CLUSTERKEY/NAMESPACE/NAME.
func splitHostID(id string) (key, ns, name string) {
	l := strings.Split(id, "/")
	return strings.Join(l[:3], "/"), l[3], l[4]
}

// hostServices reports the Services that hold aliases, the short names
// that collide and the Services without an alias.
func (s *Service) hostServices() hostServicesJSON {
	stored := s.hostRecords("")
	full := hostAliases() && len(s.loopCandidates("", "")) == 0
	s.hostMu.Lock()
	defer s.hostMu.Unlock()
	out := hostServicesJSON{Services: []hostServiceJSON{}, Collisions: []hostCollisionJSON{}, Unserved: []hostUnservedJSON{}, PoolExhausted: full}
	cols := s.collisionsLocked()
	for _, id := range slices.Sorted(maps.Keys(stored)) {
		key, ns, name := splitHostID(id)
		j := hostServiceJSON{Cluster: clusterPath(key), Namespace: ns, Service: name, Name: qualifiedName(id), Address: stored[id], Ports: []int{}}
		if short := shortName(ns, name); cols[short] == nil && s.hostNames[key][ns+"/"+name] {
			j.ShortName = short
		}
		if h := s.hostSvcs[id]; h != nil {
			j.Ports = slices.Clone(h.ports)
		}
		out.Services = append(out.Services, j)
	}
	for _, short := range slices.Sorted(maps.Keys(cols)) {
		c := hostCollisionJSON{Name: short}
		for _, k := range cols[short] {
			c.Clusters = append(c.Clusters, clusterPath(k))
		}
		out.Collisions = append(out.Collisions, c)
	}
	for _, id := range slices.Sorted(maps.Keys(s.hostFail)) {
		key, ns, name := splitHostID(id)
		err := s.hostFail[id]
		out.Unserved = append(out.Unserved, hostUnservedJSON{Cluster: clusterPath(key), Namespace: ns, Service: name, Reason: err.Error()})
		out.PoolExhausted = out.PoolExhausted || errors.Is(err, errPoolExhausted)
	}
	return out
}

// serveHostServices is GET /_emu/v1/host/services.
func (s *Service) serveHostServices(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.hostServices())
}
