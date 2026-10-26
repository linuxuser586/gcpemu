package gke

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/netplane"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Container-native load balancing (FR-GKE-008, FR-INT-001): Services
// annotated cloud.google.com/neg: {"exposed_ports": {"80": {"name": "x"}}}
// get standalone zonal GCE_VM_IP_PORT NEGs (one per cluster zone) whose
// endpoints are the Service's ready pod IP:targetPort pairs. The controller
// polls the cluster every second, so endpoint changes reach the NEG well
// within 5 s, and it reports the NEGs in the cloud.google.com/neg-status
// annotation like GKE's NEG controller.

const (
	annoNEG       = "cloud.google.com/neg"
	annoNEGStatus = "cloud.google.com/neg-status"
	negInterval   = time.Second
)

// negRecord is a NEG the controller owns.
type negRecord struct {
	Project   string `json:"project"`
	Zone      string `json:"zone"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Service   string `json:"service"`
	Port      int    `json:"port"`
	// Hash of the last endpoint set written.
	Hash string `json:"hash"`
}

func negKey(cluster, zone, name string) string { return cluster + "|" + zone + "|" + name }

// startControllers starts the cluster's background controllers once, in
// the background generation of ctx (the bring-up's): a bring-up from
// before a Reset starts none (#293).
func (s *Service) startControllers(ctx context.Context, key string) {
	g := genOf(ctx)
	if g == nil || !s.join(g, 7) {
		return
	}
	rt := s.rtFor(key)
	rt.mu.Lock()
	if rt.cancel != nil {
		rt.mu.Unlock()
		g.wg.Add(-7)
		return
	}
	ctx, cancel := context.WithCancel(g.ctx)
	rt.cancel = cancel
	rt.mu.Unlock()
	go func() {
		defer g.wg.Done()
		s.secretSyncLoop(ctx, key)
	}()
	go func() {
		defer g.wg.Done()
		s.lbServicesLoop(ctx, key)
	}()
	go func() {
		defer g.wg.Done()
		s.hostServicesLoop(ctx, key)
	}()
	go func() {
		defer g.wg.Done()
		s.systemMetricsLoop(ctx, key)
	}()
	go func() {
		defer g.wg.Done()
		s.prometheusLoop(ctx, key)
	}()
	go func() {
		defer g.wg.Done()
		s.autoscaleLoop(ctx, key)
	}()
	go func() {
		defer g.wg.Done()
		t := time.NewTicker(negInterval)
		defer t.Stop()
		for {
			if err := s.syncLoadBalancers(ctx, key); err != nil && ctx.Err() == nil {
				s.env.Log.Debug("gke: NEG and load balancer sync", "cluster", key, "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// negAnnotation is the cloud.google.com/neg annotation value. Ingress is
// not consulted: load balancers always use NEGs (l7.go), as GKE does on
// VPC-native clusters.
type negAnnotation struct {
	Ingress      bool `json:"ingress"`
	ExposedPorts map[string]struct {
		Name string `json:"name"`
	} `json:"exposed_ports"`
}

// syncNEGs reconciles the NEGs of one cluster: those Kubernetes Services'
// NEG annotations expose and those the load balancers send to, lbPorts
// (Kubernetes Service ports by namespace/name). A NEG a load balancer
// still uses is deleted on a later sync, once it no longer does.
func (s *Service) syncNEGs(ctx context.Context, key string, lbPorts map[string]map[int]bool) error {
	kc, err := s.kube(key)
	if err != nil {
		return err
	}
	rec, err := s.loadKey(key)
	if err != nil {
		return err
	}
	c := rec.cluster()
	vpc := s.vpc()
	var svcs kubeList[kubeService]
	if err := kc.get(ctx, "/api/v1/services", &svcs); err != nil {
		return err
	}
	nodeZone := map[string]string{}
	for _, n := range rec.Int.Nodes {
		nodeZone[n.Name] = n.Zone
	}
	zones := negZones(rec)

	owned := map[string]negRecord{}
	_ = s.env.Store.View(func(tx store.Tx) error {
		recs, _ := store.ListJSON[negRecord](tx, nsNEGs, key+"|")
		for _, r := range recs {
			owned[negKey(key, r.Zone, r.Name)] = r
		}
		return nil
	})
	desired := map[string]bool{}
	var errs []error
	for _, svc := range svcs.Items {
		raw := svc.Metadata.Annotations[annoNEG]
		svcLB := lbPorts[svc.Metadata.Namespace+"/"+svc.Metadata.Name]
		if raw == "" && len(svcLB) == 0 {
			continue
		}
		var ann negAnnotation
		if err := json.Unmarshal([]byte(raw), &ann); err != nil && len(svcLB) == 0 {
			continue
		}
		status := map[string]string{}
		portSet := map[int]bool{}
		for p := range ann.ExposedPorts {
			if port, err := strconv.Atoi(p); err == nil {
				portSet[port] = true
			}
		}
		for port := range svcLB {
			portSet[port] = true
		}
		ports := make([]int, 0, len(portSet))
		for port := range portSet {
			ports = append(ports, port)
		}
		sort.Ints(ports)
		for _, port := range ports {
			p := strconv.Itoa(port)
			name := negNameFor(c.Id, &svc, port)
			status[p] = name
			eps, err := s.negEndpoints(ctx, kc, svc, port, nodeZone)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			for _, z := range zones {
				k := negKey(key, z, name)
				desired[k] = true
				cur, exists := owned[k]
				h := hashEndpoints(eps[z])
				if exists && cur.Hash == h {
					continue
				}
				if !exists {
					desc := fmt.Sprintf(`{"cluster-uid":%q,"namespace":%q,"service-name":%q,"port":%q}`, c.Id, svc.Metadata.Namespace, svc.Metadata.Name, p)
					if err := vpc.UpsertNEG(ctx, rec.Int.Project, z, name, c.NetworkConfig.GetNetwork(), c.NetworkConfig.GetSubnetwork(), port, desc); err != nil {
						errs = append(errs, err)
						continue
					}
				}
				if err := vpc.SetNEGEndpoints(ctx, rec.Int.Project, z, name, eps[z]); err != nil {
					errs = append(errs, err)
					continue
				}
				nr := negRecord{Project: rec.Int.Project, Zone: z, Name: name, Namespace: svc.Metadata.Namespace, Service: svc.Metadata.Name, Port: port, Hash: h}
				_ = s.env.Store.Update(func(tx store.Tx) error { return store.PutJSON(tx, nsNEGs, k, nr) })
			}
		}
		// Report the NEGs on the Service.
		want, _ := json.Marshal(map[string]any{"network_endpoint_groups": status, "zones": zones})
		if svc.Metadata.Annotations[annoNEGStatus] != string(want) {
			patch := map[string]any{"metadata": map[string]any{"annotations": map[string]any{annoNEGStatus: string(want)}}}
			if err := kc.mergePatch(ctx, "/api/v1/namespaces/"+svc.Metadata.Namespace+"/services/"+svc.Metadata.Name, patch); err != nil {
				errs = append(errs, err)
			}
		}
	}
	for k, r := range owned {
		if desired[k] {
			continue
		}
		if err := vpc.DeleteNEG(ctx, r.Project, r.Zone, r.Name); err != nil {
			if apierr.From(err).LegacyReason != "resourceInUseByAnotherResource" {
				errs = append(errs, err)
			}
			continue
		}
		_ = s.env.Store.Update(func(tx store.Tx) error { return tx.Delete(nsNEGs, k) })
	}
	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}

// negEndpoints returns the ready endpoints of svc's port, by zone.
func (s *Service) negEndpoints(ctx context.Context, kc *kubeClient, svc kubeService, port int, nodeZone map[string]string) (map[string][]emu.NEGEndpoint, error) {
	var sp *kubeServicePort
	for i := range svc.Spec.Ports {
		if svc.Spec.Ports[i].Port == port {
			sp = &svc.Spec.Ports[i]
		}
	}
	out := map[string][]emu.NEGEndpoint{}
	if sp == nil {
		return out, nil
	}
	var slices kubeList[kubeEndpointSlice]
	q := "/apis/discovery.k8s.io/v1/namespaces/" + svc.Metadata.Namespace + "/endpointslices?labelSelector=kubernetes.io%2Fservice-name%3D" + svc.Metadata.Name
	if err := kc.get(ctx, q, &slices); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, sl := range slices.Items {
		if sl.AddressType != "IPv4" {
			continue
		}
		target := 0
		for _, p := range sl.Ports {
			name := ""
			if p.Name != nil {
				name = *p.Name
			}
			if name == sp.Name && p.Port != nil {
				target = *p.Port
			}
		}
		if target == 0 {
			continue
		}
		for _, ep := range sl.Endpoints {
			if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
				continue
			}
			z := nodeZone[ep.NodeName]
			for _, ip := range ep.Addresses {
				k := ip + ":" + strconv.Itoa(target)
				if seen[k] {
					continue
				}
				seen[k] = true
				out[z] = append(out[z], emu.NEGEndpoint{IP: ip, Port: target, Instance: ep.NodeName})
			}
		}
	}
	for z := range out {
		sort.Slice(out[z], func(i, j int) bool {
			a, b := out[z][i], out[z][j]
			if a.IP != b.IP {
				return a.IP < b.IP
			}
			return a.Port < b.Port
		})
	}
	return out, nil
}

func hashEndpoints(eps []emu.NEGEndpoint) string {
	h := sha256.New()
	for _, e := range eps {
		fmt.Fprintf(h, "%s:%d@%s\n", e.IP, e.Port, e.Instance)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// negNameFor returns the NEG name of a Service port: the one its
// cloud.google.com/neg annotation gives, or negName's.
func negNameFor(clusterID string, svc *kubeService, port int) string {
	var ann negAnnotation
	if json.Unmarshal([]byte(svc.Metadata.Annotations[annoNEG]), &ann) == nil {
		if n := ann.ExposedPorts[strconv.Itoa(port)].Name; n != "" {
			return n
		}
	}
	return negName(clusterID, svc.Metadata.Namespace, svc.Metadata.Name, port)
}

// negName generates a NEG name like GKE's NEG controller:
// k8s1-<cluster>-<namespace>-<service>-<port>-<hash>.
func negName(clusterID, ns, svc string, port int) string {
	id := clusterID
	if len(id) > 8 {
		id = id[:8]
	}
	h := shortHash(clusterID + "/" + ns + "/" + svc + "/" + strconv.Itoa(port))
	return strings.ToLower("k8s1-" + id + "-" + trunc(ns, 15) + "-" + trunc(svc, 15) + "-" + strconv.Itoa(port) + "-" + h)
}

// deleteNEGs removes every NEG a cluster owns.
func (s *Service) deleteNEGs(ctx context.Context, key string) {
	var recs []negRecord
	_ = s.env.Store.View(func(tx store.Tx) error {
		recs, _ = store.ListJSON[negRecord](tx, nsNEGs, key+"|")
		return nil
	})
	vpc := s.vpc()
	for _, r := range recs {
		if err := vpc.DeleteNEG(ctx, r.Project, r.Zone, r.Name); err != nil {
			s.env.Log.Warn("gke: deleting NEG", "neg", r.Name, "zone", r.Zone, "err", err)
		}
		k := negKey(key, r.Zone, r.Name)
		_ = s.env.Store.Update(func(tx store.Tx) error { return tx.Delete(nsNEGs, k) })
	}
}

func containsIP(cidr, ip string) bool { return netplane.Contains(cidr, ip) }
