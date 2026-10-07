package gke

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"

	"cloud.google.com/go/container/apiv1/containerpb"
	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
	"github.com/linuxuser586/gcpemu/services/compute"
)

// Node pool instance groups. GKE reports each pool's nodes as a zonal
// managed instance group (nodePool.instanceGroupUrls); clients follow
// those URLs into the compute API: the OpenTofu provider lists the zone's
// instanceGroupManagers to compute node_count and managed_instance_group_urls
// and treats a 404 as a vanished pool. The groups are served read-only on
// compute's API from the node pool records.

// registerInstanceGroups serves the read-only instance group collections
// on the compute API when the compute service runs.
func (s *Service) registerInstanceGroups() {
	svc, ok := s.env.Lookup("compute")
	if !ok {
		return
	}
	cmp, ok := svc.(*compute.Service)
	if !ok {
		return
	}
	const z = "/compute/v1/projects/{project}/zones/{zone}/"
	cmp.HandleFunc("GET "+z+"instanceGroupManagers", s.listIGMs)
	cmp.HandleFunc("GET "+z+"instanceGroupManagers/{name}", s.getIGM)
	cmp.HandleFunc("GET "+z+"instanceGroups", s.listIGs)
	cmp.HandleFunc("GET "+z+"instanceGroups/{name}", s.getIG)
	cmp.HandleFunc("POST "+z+"instanceGroups/{name}/setNamedPorts", func(w http.ResponseWriter, r *http.Request) {
		s.setNamedPorts(w, r, cmp)
	})
	cmp.AddInstanceGroupResolver(s.resolveIG)
}

// poolGroup is one node pool's instance group in one zone.
type poolGroup struct {
	name, project, zone string
	cluster             *containerpb.Cluster
	pool                *containerpb.NodePool
	nodes               []nodeRecord
	namedPorts          []*computev1.NamedPort
}

// poolGroups returns the instance groups of every node pool in a zone.
func (s *Service) poolGroups(projectID, zone string) []poolGroup {
	var out []poolGroup
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, rec := range listClusters(tx, projectID+"/") {
			c := rec.cluster()
			for _, np := range c.NodePools {
				for _, u := range np.InstanceGroupUrls {
					if !strings.Contains(u, "/zones/"+zone+"/") {
						continue
					}
					g := poolGroup{name: path.Base(u), project: projectID, zone: zone, cluster: c, pool: np,
						namedPorts: rec.Int.NamedPorts[path.Base(u)]}
					for _, n := range rec.Int.Nodes {
						if n.Pool == np.Name && n.Zone == zone {
							g.nodes = append(g.nodes, n)
						}
					}
					out = append(out, g)
				}
			}
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func (s *Service) findGroup(r *http.Request) (poolGroup, bool) {
	for _, g := range s.poolGroups(r.PathValue("project"), r.PathValue("zone")) {
		if g.name == r.PathValue("name") {
			return g, true
		}
	}
	return poolGroup{}, false
}

func (g poolGroup) size() int64 { return int64(len(g.nodes)) }

// fingerprint changes with the pool and the group's named ports.
func (g poolGroup) fingerprint() string {
	if len(g.namedPorts) == 0 {
		return g.pool.Etag
	}
	return compute.Fingerprint([]any{g.pool.Etag, g.namedPorts})
}

func (g poolGroup) zonePath() string { return "projects/" + g.project + "/zones/" + g.zone }

func (g poolGroup) id() uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(g.zonePath() + "/" + g.name))
	return h.Sum64() >> 1
}

func (g poolGroup) igm() *computev1.InstanceGroupManager {
	zp := g.zonePath()
	return &computev1.InstanceGroupManager{
		Kind:              "compute#instanceGroupManager",
		Id:                g.id(),
		Name:              g.name,
		Zone:              compute.SelfLink(zp),
		SelfLink:          compute.SelfLink(zp + "/instanceGroupManagers/" + g.name),
		InstanceGroup:     compute.SelfLink(zp + "/instanceGroups/" + g.name),
		InstanceTemplate:  compute.SelfLink("projects/" + g.project + "/global/instanceTemplates/" + strings.TrimSuffix(g.name, "-grp")),
		BaseInstanceName:  strings.TrimSuffix(g.name, "-grp"),
		TargetSize:        g.size(),
		CreationTimestamp: g.cluster.CreateTime,
		Fingerprint:       g.pool.Etag,
		CurrentActions:    &computev1.InstanceGroupManagerActionsSummary{None: g.size(), ForceSendFields: []string{"None"}},
		Status:            &computev1.InstanceGroupManagerStatus{IsStable: true, ForceSendFields: []string{"IsStable"}},
		ForceSendFields:   []string{"TargetSize"},
	}
}

func (g poolGroup) ig() *computev1.InstanceGroup {
	zp := g.zonePath()
	return &computev1.InstanceGroup{
		Kind:              "compute#instanceGroup",
		Id:                g.id(),
		Name:              g.name,
		Zone:              compute.SelfLink(zp),
		SelfLink:          compute.SelfLink(zp + "/instanceGroups/" + g.name),
		Network:           compute.SelfLink("projects/" + g.project + "/global/networks/" + g.cluster.Network),
		Size:              g.size(),
		NamedPorts:        g.namedPorts,
		CreationTimestamp: g.cluster.CreateTime,
		Fingerprint:       g.fingerprint(),
		ForceSendFields:   []string{"Size"},
	}
}

// checkIG authorizes a read of an instance group collection.
func (s *Service) checkIG(w http.ResponseWriter, r *http.Request, perm string) bool {
	if err := s.env.Auth.Check(r.Context(), perm, "//compute.googleapis.com/projects/"+r.PathValue("project")); err != nil {
		apierr.Write(w, err)
		return false
	}
	return true
}

func (s *Service) listIGMs(w http.ResponseWriter, r *http.Request) {
	if !s.checkIG(w, r, "compute.instanceGroupManagers.list") {
		return
	}
	var items []compute.ListItem
	for _, g := range s.poolGroups(r.PathValue("project"), r.PathValue("zone")) {
		items = append(items, compute.ListItem{Key: g.name, Value: g.igm()})
	}
	compute.WriteList(w, r, "compute#instanceGroupManagerList", "projects/"+r.PathValue("project")+"/zones/"+r.PathValue("zone")+"/instanceGroupManagers", items)
}

func (s *Service) getIGM(w http.ResponseWriter, r *http.Request) {
	if !s.checkIG(w, r, "compute.instanceGroupManagers.get") {
		return
	}
	g, ok := s.findGroup(r)
	if !ok {
		apierr.Write(w, igNotFound(r, "instanceGroupManagers"))
		return
	}
	writeJSON(w, g.igm())
}

func (s *Service) listIGs(w http.ResponseWriter, r *http.Request) {
	if !s.checkIG(w, r, "compute.instanceGroups.list") {
		return
	}
	var items []compute.ListItem
	for _, g := range s.poolGroups(r.PathValue("project"), r.PathValue("zone")) {
		items = append(items, compute.ListItem{Key: g.name, Value: g.ig()})
	}
	compute.WriteList(w, r, "compute#instanceGroupList", "projects/"+r.PathValue("project")+"/zones/"+r.PathValue("zone")+"/instanceGroups", items)
}

func (s *Service) getIG(w http.ResponseWriter, r *http.Request) {
	if !s.checkIG(w, r, "compute.instanceGroups.get") {
		return
	}
	g, ok := s.findGroup(r)
	if !ok {
		apierr.Write(w, igNotFound(r, "instanceGroups"))
		return
	}
	writeJSON(w, g.ig())
}

func igNotFound(r *http.Request, coll string) error {
	return apierr.NotFound("The resource 'projects/%s/zones/%s/%s/%s' was not found",
		r.PathValue("project"), r.PathValue("zone"), coll, r.PathValue("name"))
}

var namedPortRE = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)

// setNamedPorts sets a node pool group's named ports, which instance
// group backends of a load balancer resolve their portName against
// (FR-LB-005). The group's fingerprint is optional; a stale one is a 412.
func (s *Service) setNamedPorts(w http.ResponseWriter, r *http.Request, cmp *compute.Service) {
	if !s.checkIG(w, r, "compute.instanceGroups.update") {
		return
	}
	var req computev1.InstanceGroupsSetNamedPortsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apierr.Write(w, apierr.InvalidArgument("Invalid JSON payload received. %v", err))
		return
	}
	for i, np := range req.NamedPorts {
		if np == nil {
			np = &computev1.NamedPort{}
			req.NamedPorts[i] = np
		}
		if !namedPortRE.MatchString(np.Name) {
			apierr.Write(w, apierr.InvalidArgument("Invalid value for field 'namedPorts[%d].name': %q. Must be a match of regex '[a-z]([-a-z0-9]{0,61}[a-z0-9])?'", i, np.Name))
			return
		}
		if np.Port < 1 || np.Port > 65535 {
			apierr.Write(w, apierr.InvalidArgument("Invalid value for field 'namedPorts[%d].port': '%d'. Must be greater than or equal to 1", i, np.Port))
			return
		}
	}
	g, ok := s.findGroup(r)
	if !ok {
		apierr.Write(w, igNotFound(r, "instanceGroups"))
		return
	}
	if req.Fingerprint != "" && req.Fingerprint != g.fingerprint() {
		apierr.Write(w, apierr.FailedPrecondition("Supplied fingerprint does not match current metadata fingerprint.").
			WithLegacy("conditionNotMet").WithHTTP(http.StatusPreconditionFailed))
		return
	}
	key := clusterKey(g.project, g.cluster.Location, g.cluster.Name)
	op, err := cmp.StartOperation(r.Context(), g.project, "zones/"+g.zone, "setNamedPorts",
		g.zonePath()+"/instanceGroups/"+g.name, g.id(), func(ctx context.Context) error {
			return s.env.Store.Update(func(tx store.Tx) error {
				rec, err := getCluster(tx, key)
				if err != nil {
					return err
				}
				rec.Int.setNamedPorts(rec.cluster(), g.name, req.NamedPorts)
				return putCluster(tx, rec)
			})
		})
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, op)
}

// setNamedPorts records group's named ports and drops those of groups the
// cluster no longer has.
func (in *clusterInternal) setNamedPorts(c *containerpb.Cluster, group string, ports []*computev1.NamedPort) {
	live := map[string]bool{}
	for _, np := range c.NodePools {
		for _, u := range np.InstanceGroupUrls {
			live[path.Base(u)] = true
		}
	}
	for k := range in.NamedPorts {
		if !live[k] {
			delete(in.NamedPorts, k)
		}
	}
	if len(ports) == 0 {
		delete(in.NamedPorts, group)
		return
	}
	if in.NamedPorts == nil {
		in.NamedPorts = map[string][]*computev1.NamedPort{}
	}
	in.NamedPorts[group] = ports
}

// resolveIG serves node pool groups to load balancer backends: the nodes'
// internal addresses and the group's named ports.
func (s *Service) resolveIG(_ context.Context, p string) (compute.InstanceGroup, bool) {
	segs := strings.Split(p, "/")
	if len(segs) != 6 || segs[0] != "projects" || segs[2] != "zones" || segs[4] != "instanceGroups" {
		return compute.InstanceGroup{}, false
	}
	for _, g := range s.poolGroups(segs[1], segs[3]) {
		if g.name != segs[5] {
			continue
		}
		ig := compute.InstanceGroup{NamedPorts: map[string]int{}}
		for _, n := range g.nodes {
			ig.Instances = append(ig.Instances, emu.NEGEndpoint{IP: n.IP, Instance: n.Name})
		}
		for _, np := range g.namedPorts {
			if _, dup := ig.NamedPorts[np.Name]; !dup {
				ig.NamedPorts[np.Name] = int(np.Port)
			}
		}
		return ig, true
	}
	return compute.InstanceGroup{}, false
}
