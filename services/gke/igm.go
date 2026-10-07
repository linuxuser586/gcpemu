package gke

import (
	"hash/fnv"
	"net/http"
	"path"
	"sort"
	"strings"

	"cloud.google.com/go/container/apiv1/containerpb"
	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
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
}

// poolGroup is one node pool's instance group in one zone.
type poolGroup struct {
	name, project, zone string
	cluster             *containerpb.Cluster
	pool                *containerpb.NodePool
	size                int64
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
					g := poolGroup{name: path.Base(u), project: projectID, zone: zone, cluster: c, pool: np}
					for _, n := range rec.Int.Nodes {
						if n.Pool == np.Name && n.Zone == zone {
							g.size++
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
		TargetSize:        g.size,
		CreationTimestamp: g.cluster.CreateTime,
		Fingerprint:       g.pool.Etag,
		CurrentActions:    &computev1.InstanceGroupManagerActionsSummary{None: g.size, ForceSendFields: []string{"None"}},
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
		Size:              g.size,
		CreationTimestamp: g.cluster.CreateTime,
		Fingerprint:       g.pool.Etag,
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
