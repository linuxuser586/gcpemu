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

// startControllers starts the cluster's background controllers once.
func (s *Service) startControllers(key string) {
	rt := s.rtFor(key)
	rt.mu.Lock()
	if rt.cancel != nil {
		rt.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	rt.cancel = cancel
	rt.mu.Unlock()
	s.wg.Add(2)
	go func() {
		defer s.wg.Done()
		s.secretSyncLoop(ctx, key)
	}()
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(negInterval)
		defer t.Stop()
		for {
			if err := s.syncNEGs(ctx, key); err != nil && ctx.Err() == nil {
				s.env.Log.Debug("gke: NEG sync", "cluster", key, "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// negAnnotation is the cloud.google.com/neg annotation value.
type negAnnotation struct {
	Ingress      bool `json:"ingress"`
	ExposedPorts map[string]struct {
		Name string `json:"name"`
	} `json:"exposed_ports"`
}

// syncNEGs reconciles the NEGs of one cluster.
func (s *Service) syncNEGs(ctx context.Context, key string) error {
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
	zoneSet := map[string]bool{}
	for _, n := range rec.Int.Nodes {
		nodeZone[n.Name] = n.Zone
		zoneSet[n.Zone] = true
	}
	if len(zoneSet) == 0 {
		for _, z := range c.Locations {
			zoneSet[z] = true
		}
	}
	zones := make([]string, 0, len(zoneSet))
	for z := range zoneSet {
		zones = append(zones, z)
	}
	sort.Strings(zones)

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
		if raw == "" {
			continue
		}
		var ann negAnnotation
		if err := json.Unmarshal([]byte(raw), &ann); err != nil {
			continue
		}
		status := map[string]string{}
		ports := make([]string, 0, len(ann.ExposedPorts))
		for p := range ann.ExposedPorts {
			ports = append(ports, p)
		}
		sort.Strings(ports)
		for _, p := range ports {
			port, err := strconv.Atoi(p)
			if err != nil {
				continue
			}
			name := ann.ExposedPorts[p].Name
			if name == "" {
				name = negName(c.Id, svc.Metadata.Namespace, svc.Metadata.Name, port)
			}
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
			errs = append(errs, err)
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
