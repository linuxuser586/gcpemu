package gke

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/project"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Store namespaces.
const (
	nsClusters   = "gke/clusters"   // PROJECT/LOCATION/NAME → clusterRecord
	nsOperations = "gke/operations" // PROJECT/OPERATION → protojson Operation
	nsNEGs       = "gke/negs"       // CLUSTERKEY|ZONE|NAME → negRecord
)

const apiBase = "https://container.googleapis.com/v1/"

// clusterRecord is the persisted form of a cluster: the API resource plus
// the emulator's data-plane bookkeeping.
type clusterRecord struct {
	Cluster json.RawMessage `json:"cluster"`
	Int     clusterInternal `json:"internal"`
}

// clusterInternal is data-plane state that is not part of the API.
type clusterInternal struct {
	Project  string `json:"project"`
	Location string `json:"location"`
	Name     string `json:"name"`
	// Secret authenticates the cluster's webhooks and node agents to the
	// emulator (it is part of their URL path).
	Secret string `json:"secret"`
	// Token is the k3s join token.
	Token string `json:"token"`
	// Subnetwork is the canonical subnetwork the nodes live on.
	Subnetwork string `json:"subnetwork"`
	// ServerIP is the control plane's address on the subnetwork (private
	// endpoint); ExternalIP its address on the external network (public
	// endpoint), empty for private-endpoint-only clusters.
	ServerIP   string `json:"serverIp"`
	ExternalIP string `json:"externalIp,omitempty"`
	// PodCIDR / ServiceCIDR are the ranges k3s runs with.
	PodCIDR     string       `json:"podCidr"`
	ServiceCIDR string       `json:"serviceCidr"`
	Nodes       []nodeRecord `json:"nodes,omitempty"`
}

// nodeRecord is one node container.
type nodeRecord struct {
	Name       string `json:"name"`
	Pool       string `json:"pool"`
	Zone       string `json:"zone"`
	IP         string `json:"ip"`
	ExternalIP string `json:"externalIp,omitempty"`
	// Password is the k3s node password, kept so that a recreated node
	// container can re-register under the same name.
	Password string `json:"password"`
}

func clusterKey(projectID, location, name string) string {
	return projectID + "/" + location + "/" + name
}

func (r *clusterRecord) key() string { return clusterKey(r.Int.Project, r.Int.Location, r.Int.Name) }

// cluster decodes the API resource.
func (r *clusterRecord) cluster() *containerpb.Cluster {
	c := &containerpb.Cluster{}
	_ = protojson.Unmarshal(r.Cluster, c)
	return c
}

func (r *clusterRecord) setCluster(c *containerpb.Cluster) {
	r.Cluster, _ = protojson.Marshal(c)
}

func (r *clusterRecord) nodesOf(pool string) []nodeRecord {
	var out []nodeRecord
	for _, n := range r.Int.Nodes {
		if n.Pool == pool {
			out = append(out, n)
		}
	}
	return out
}

func getCluster(tx store.Tx, key string) (*clusterRecord, error) {
	var r clusterRecord
	if err := store.GetJSON(tx, nsClusters, key, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func putCluster(tx store.Tx, r *clusterRecord) error {
	return store.PutJSON(tx, nsClusters, r.key(), r)
}

// ---- names ----

var clusterNameRE = regexp.MustCompile(`^[a-z](?:[-a-z0-9]{0,38}[a-z0-9])?$`)

// validateName checks a cluster or node pool name (RFC 1035, ≤ 40 chars).
func validateName(kind, n string) error {
	if !clusterNameRE.MatchString(n) {
		return apierr.InvalidArgument("Invalid value for field '%s.name': %q. Must match regex '(?:[a-z](?:[-a-z0-9]{0,38}[a-z0-9])?)'.", kind, n)
	}
	return nil
}

// ref identifies a cluster (and optionally a node pool) from either the
// resource name or the legacy project/zone/cluster fields.
type ref struct {
	Project, Location, Cluster, Pool string
}

func (r ref) key() string { return clusterKey(r.Project, r.Location, r.Cluster) }

func (r ref) parent() string { return "projects/" + r.Project + "/locations/" + r.Location }

func (r ref) clusterName() string { return r.parent() + "/clusters/" + r.Cluster }

func (r ref) poolName() string { return r.clusterName() + "/nodePools/" + r.Pool }

// clusterResource is the IAM full resource name of the cluster.
func (r ref) clusterResource() string { return "//container.googleapis.com/" + r.clusterName() }

func projectResource(p string) string { return "//cloudresourcemanager.googleapis.com/projects/" + p }

// parseRef resolves name ("projects/P/locations/L[/clusters/C[/nodePools/N]]")
// or the legacy fields. want is the number of trailing components needed:
// 0 = location, 1 = cluster, 2 = node pool.
func parseRef(name, projectID, zone, clusterID, poolID string, want int) (ref, error) {
	var r ref
	if name != "" {
		segs := strings.Split(name, "/")
		if len(segs) < 4 || segs[0] != "projects" || segs[2] != "locations" {
			return r, apierr.InvalidArgument("Invalid resource name %q.", name)
		}
		r.Project, r.Location = segs[1], segs[3]
		rest := segs[4:]
		if want >= 1 {
			if len(rest) < 2 || rest[0] != "clusters" {
				return r, apierr.InvalidArgument("Invalid cluster name %q.", name)
			}
			r.Cluster = rest[1]
			rest = rest[2:]
		}
		if want >= 2 {
			if len(rest) < 2 || rest[0] != "nodePools" {
				return r, apierr.InvalidArgument("Invalid node pool name %q.", name)
			}
			r.Pool = rest[1]
			rest = rest[2:]
		}
		if len(rest) != 0 {
			return r, apierr.InvalidArgument("Invalid resource name %q.", name)
		}
	} else {
		r = ref{Project: projectID, Location: zone, Cluster: clusterID, Pool: poolID}
		if r.Project == "" {
			return r, apierr.InvalidArgument("Required field 'project_id' or 'name' is missing.")
		}
		if r.Location == "" {
			return r, apierr.InvalidArgument("Required field 'zone' or 'name' is missing.")
		}
		if want >= 1 && r.Cluster == "" {
			return r, apierr.InvalidArgument("Required field 'cluster_id' or 'name' is missing.")
		}
		if want >= 2 && r.Pool == "" {
			return r, apierr.InvalidArgument("Required field 'node_pool_id' or 'name' is missing.")
		}
	}
	return r, nil
}

// selfLink returns a GKE self link: zonal resources use the legacy
// "zones/" form, regional ones "locations/".
func selfLink(projectID, location, rest string) string {
	kind := "locations"
	if isZone(location) {
		kind = "zones"
	}
	return apiBase + "projects/" + projectID + "/" + kind + "/" + location + "/" + rest
}

// projectNumber returns the deterministic project number.
func projectNumber(p string) string { return project.NumberString(p) }

func notFoundCluster(r ref) error {
	return apierr.NotFound("Not found: %s.", r.clusterName())
}

func notFoundPool(r ref) error {
	return apierr.NotFound("Not found: %s.", r.poolName())
}

func cloneCluster(c *containerpb.Cluster) *containerpb.Cluster {
	return proto.Clone(c).(*containerpb.Cluster)
}

// findPool returns the pool with name n.
func findPool(c *containerpb.Cluster, n string) *containerpb.NodePool {
	for _, p := range c.NodePools {
		if p.Name == n {
			return p
		}
	}
	return nil
}

// totalNodes is the number of nodes a pool runs (count per zone × zones).
func poolNodeCount(np *containerpb.NodePool) int {
	return int(np.InitialNodeCount) * max(1, len(np.Locations))
}

func errf(format string, a ...any) error { return fmt.Errorf(format, a...) }

// listClusters returns every cluster record under prefix.
func listClusters(tx store.Tx, prefix string) []*clusterRecord {
	recs, _ := store.ListJSON[*clusterRecord](tx, nsClusters, prefix)
	return recs
}
