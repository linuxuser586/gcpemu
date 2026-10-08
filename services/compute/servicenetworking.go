package compute

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	computev1 "google.golang.org/api/compute/v1"
	snv1 "google.golang.org/api/servicenetworking/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/project"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// servicenetworking.googleapis.com v1 (minimal): private services access
// connections bind VPC_PEERING global addresses of a VPC to the service
// producer (Cloud SQL private IP). This is what OpenTofu's
// google_service_networking_connection drives. Operations are
// google.longrunning (internal/lro).

const (
	nsPSA = "compute/psaConnections"
	// psaService is the only producer service emulated.
	psaService     = "servicenetworking.googleapis.com"
	psaPeeringName = "servicenetworking-googleapis-com"
	snOpParent     = "services/" + psaService
)

// psaConnection is a stored private services access connection, keyed by
// the consumer network path.
type psaConnection struct {
	Network               string   `json:"network"` // projects/ID/global/networks/N
	ReservedPeeringRanges []string `json:"reservedPeeringRanges"`
	CreateTime            string   `json:"createTime"`
}

// peering renders the connection as the network's peering entry.
func (c *psaConnection) peering(s *Service) *computev1.NetworkPeering {
	p, _, _ := pathParts(c.Network)
	return &computev1.NetworkPeering{
		Name:                 psaPeeringName,
		Network:              link("projects/" + tenantProject(p) + "/global/networks/servicenetworking"),
		State:                "ACTIVE",
		StateDetails:         "[" + c.CreateTime + "]: Connected.",
		AutoCreateRoutes:     true,
		ExchangeSubnetRoutes: true,
		ExportCustomRoutes:   false,
		ImportCustomRoutes:   false,
		StackType:            "IPV4_ONLY",
		ForceSendFields:      []string{"ExportCustomRoutes", "ImportCustomRoutes"},
	}
}

// tenantProject names the producer tenant project for a consumer.
func tenantProject(p string) string {
	n := project.NumberString(p)
	return "y" + n[len(n)-6:] + "-tp"
}

func (s *Service) snRoutes() {
	m := s.sn
	m.HandleFunc("POST /v1/services/{service}/connections", s.createConnection)
	m.HandleFunc("GET /v1/services/{service}/connections", s.listConnections)
	m.HandleFunc("PATCH /v1/services/{service}/connections/{name}", s.patchConnection)
	m.HandleFunc("POST /v1/services/{service}/connections/{verb}", s.connectionVerb)
	m.HandleFunc("/v1/{path...}", func(w http.ResponseWriter, r *http.Request) {
		if !s.lro.ServeREST(w, r, r.PathValue("path")) {
			notFoundHandler(w, r)
		}
	})
	m.HandleFunc("/", notFoundHandler)
}

// projectIDOf maps a project ID or number to the project ID.
func (s *Service) projectIDOf(idOrNum string) string {
	if !isDigits(idOrNum) {
		return idOrNum
	}
	id := idOrNum
	_ = s.env.Store.View(func(tx store.Tx) error {
		tx.Scan("core/projects", "", func(k string, b []byte) bool {
			var rec struct {
				ProjectNumber string `json:"projectNumber"`
			}
			if json.Unmarshal(b, &rec) == nil && rec.ProjectNumber == idOrNum {
				id = k
				return false
			}
			return true
		})
		return nil
	})
	if id == idOrNum {
		for _, p := range s.env.Config.Projects {
			if project.NumberString(p) == idOrNum {
				return p
			}
		}
	}
	return id
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// consumerNetwork canonicalises "projects/{id|number}/global/networks/N".
func (s *Service) consumerNetwork(ref string) (string, string, error) {
	segs := strings.Split(relPath(ref), "/")
	if len(segs) != 5 || segs[0] != "projects" || segs[2] != "global" || segs[3] != "networks" {
		return "", "", apierr.InvalidArgument("Invalid network %q: expected projects/{project}/global/networks/{network}.", ref)
	}
	p := s.projectIDOf(segs[1])
	if err := s.env.EnsureProject(p); err != nil {
		return "", "", err
	}
	return p, networkPath(p, segs[4]), nil
}

func checkService(r *http.Request) error {
	if r.PathValue("service") != psaService {
		return apierr.NotFound("Service %q not found or permission denied.", r.PathValue("service"))
	}
	return nil
}

func (s *Service) createConnection(w http.ResponseWriter, r *http.Request) {
	if err := checkService(r); err != nil {
		apierr.Write(w, err)
		return
	}
	var c snv1.Connection
	if _, err := decode(r, &c); err != nil {
		apierr.Write(w, err)
		return
	}
	p, np, err := s.consumerNetwork(c.Network)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.env.Auth.Check(r.Context(), "servicenetworking.services.addPeering", "//cloudresourcemanager.googleapis.com/projects/"+p); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.env.Store.View(func(tx store.Tx) error {
		if store.Exists(tx, nsPSA, np) {
			return apierr.FailedPrecondition("Cannot modify allocated ranges in CreateConnection. Please use UpdateConnection.")
		}
		return validatePSARanges(tx, p, np, c.ReservedPeeringRanges)
	}); err != nil {
		apierr.Write(w, err)
		return
	}
	op, err := s.lro.Run(r.Context(), snOpParent, nil, func(ctx context.Context) (proto.Message, error) {
		return nil, s.putConnection(p, np, c.ReservedPeeringRanges, false)
	})
	writeLRO(w, op, err)
}

// validatePSARanges checks that every range names a VPC_PEERING global
// address of the network.
func validatePSARanges(tx store.Tx, project, np string, ranges []string) error {
	if len(ranges) == 0 {
		return apierr.InvalidArgument("At least one reserved peering range is required.")
	}
	for _, name := range ranges {
		a, ok := get[computev1.Address](tx, nsGlobalAddresses, "projects/"+project+"/global/addresses/"+name)
		if !ok {
			return apierr.FailedPrecondition("Allocated IP range '%s' not found in network.", name)
		}
		if a.Purpose != "VPC_PEERING" || relPath(a.Network) != np {
			return apierr.FailedPrecondition("Allocated IP range '%s' is not a VPC_PEERING range of network '%s'.", name, np)
		}
	}
	if _, ok := get[computev1.Network](tx, nsNetworks, np); !ok {
		return apierr.NotFound("Network '%s' not found.", np)
	}
	return nil
}

func (s *Service) putConnection(project, np string, ranges []string, update bool) error {
	return s.env.Store.Update(func(tx store.Tx) error {
		if err := validatePSARanges(tx, project, np, ranges); err != nil {
			return err
		}
		c := &psaConnection{Network: np, ReservedPeeringRanges: ranges, CreateTime: stamp(s.env.Clock.Now())}
		if old, ok := get[psaConnection](tx, nsPSA, np); ok {
			if !update {
				return apierr.FailedPrecondition("Cannot modify allocated ranges in CreateConnection. Please use UpdateConnection.")
			}
			c.CreateTime = old.CreateTime
		}
		return store.PutJSON(tx, nsPSA, np, c)
	})
}

func (s *Service) connectionResource(c *psaConnection) *snv1.Connection {
	p, _, name := pathParts(c.Network)
	return &snv1.Connection{
		Network:               "projects/" + project.NumberString(p) + "/global/networks/" + name,
		Peering:               psaPeeringName,
		ReservedPeeringRanges: c.ReservedPeeringRanges,
		Service:               "services/" + psaService,
	}
}

func (s *Service) listConnections(w http.ResponseWriter, r *http.Request) {
	if err := checkService(r); err != nil {
		apierr.Write(w, err)
		return
	}
	netRef := r.URL.Query().Get("network")
	if netRef == "" {
		apierr.Write(w, apierr.InvalidArgument("The network parameter is required."))
		return
	}
	p, np, err := s.consumerNetwork(netRef)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.env.Auth.Check(r.Context(), "servicenetworking.services.get", "//cloudresourcemanager.googleapis.com/projects/"+p); err != nil {
		apierr.Write(w, err)
		return
	}
	out := &snv1.ListConnectionsResponse{Connections: []*snv1.Connection{}}
	_ = s.env.Store.View(func(tx store.Tx) error {
		if c, ok := get[psaConnection](tx, nsPSA, np); ok {
			out.Connections = append(out.Connections, s.connectionResource(c))
		}
		return nil
	})
	writeJSON(w, http.StatusOK, out)
}

// patchConnection updates reservedPeeringRanges (connections.patch).
func (s *Service) patchConnection(w http.ResponseWriter, r *http.Request) {
	if err := checkService(r); err != nil {
		apierr.Write(w, err)
		return
	}
	var c snv1.Connection
	if _, err := decode(r, &c); err != nil {
		apierr.Write(w, err)
		return
	}
	p, np, err := s.consumerNetwork(c.Network)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.env.Auth.Check(r.Context(), "servicenetworking.services.addPeering", "//cloudresourcemanager.googleapis.com/projects/"+p); err != nil {
		apierr.Write(w, err)
		return
	}
	force := r.URL.Query().Get("force") == "true"
	if err := s.env.Store.View(func(tx store.Tx) error {
		old, ok := get[psaConnection](tx, nsPSA, np)
		if !ok {
			return apierr.NotFound("Connection for network '%s' not found.", np)
		}
		if !force {
			for _, rg := range old.ReservedPeeringRanges {
				if !contains(c.ReservedPeeringRanges, rg) && s.psaInUse(tx, np) != "" {
					return apierr.FailedPrecondition("Cannot remove allocated range '%s' while producer services are using it; set force=true.", rg)
				}
			}
		}
		return validatePSARanges(tx, p, np, c.ReservedPeeringRanges)
	}); err != nil {
		apierr.Write(w, err)
		return
	}
	op, err := s.lro.Run(r.Context(), snOpParent, nil, func(ctx context.Context) (proto.Message, error) {
		return nil, s.putConnection(p, np, c.ReservedPeeringRanges, true)
	})
	writeLRO(w, op, err)
}

// connectionVerb serves deleteConnection, which the discovery document maps
// to POST v1/{name=services/*/connections/*} (also accepted with a
// ":deleteConnection" suffix).
func (s *Service) connectionVerb(w http.ResponseWriter, r *http.Request) {
	if err := checkService(r); err != nil {
		apierr.Write(w, err)
		return
	}
	_, verb, _ := strings.Cut(r.PathValue("verb"), ":")
	if verb != "" && verb != "deleteConnection" {
		notFoundHandler(w, r)
		return
	}
	var req snv1.DeleteConnectionRequest
	if _, err := decode(r, &req); err != nil {
		apierr.Write(w, err)
		return
	}
	p, np, err := s.consumerNetwork(req.ConsumerNetwork)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.env.Auth.Check(r.Context(), "servicenetworking.services.deleteConnection", "//cloudresourcemanager.googleapis.com/projects/"+p); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.checkDeleteConnection(np); err != nil {
		apierr.Write(w, err)
		return
	}
	op, err := s.lro.Run(r.Context(), snOpParent, nil, func(ctx context.Context) (proto.Message, error) {
		return nil, s.dropConnection(ctx, np)
	})
	writeLRO(w, op, err)
}

func (s *Service) checkDeleteConnection(np string) error {
	var err error
	_ = s.env.Store.View(func(tx store.Tx) error {
		if !store.Exists(tx, nsPSA, np) {
			err = apierr.NotFound("Connection for network '%s' not found.", np)
		} else if u := s.psaInUse(tx, np); u != "" {
			err = apierr.FailedPrecondition("Producer services (e.g. CloudSQL, Cloud Memstore, etc.) are still using this connection (%s).", u)
		}
		return nil
	})
	return err
}

// dropConnection deletes a connection and its realised network.
func (s *Service) dropConnection(ctx context.Context, np string) error {
	if err := s.checkDeleteConnection(np); err != nil {
		return err
	}
	if err := s.env.Store.Update(func(tx store.Tx) error { return tx.Delete(nsPSA, np) }); err != nil {
		return err
	}
	s.releaseNetworkRuntime(ctx, "", []string{psaKey(np)})
	s.networkChanged(np)
	return nil
}

// psaInUse returns an owner holding an IP on the network's private
// services access range.
func (s *Service) psaInUse(tx store.Tx, np string) string {
	if st, ok := get[vpcNet](tx, nsVPCNets, psaKey(np)); ok {
		return firstOwner(tx, st.Name)
	}
	return ""
}

// removePeering implements compute networks.removePeering for user
// peerings (peering.go) and the servicenetworking peering (older OpenTofu
// providers delete the connection this way).
func (s *Service) removePeering(w http.ResponseWriter, r *http.Request) {
	path, cur, err := s.loadNetwork(r, "compute.networks.removePeering")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var req computev1.NetworksRemovePeeringRequest
	if _, err := decode(r, &req); err != nil {
		apierr.Write(w, err)
		return
	}
	if req.Name != psaPeeringName {
		if !s.removeUserPeering(w, r, path, cur, req.Name) {
			apierr.Write(w, errNotFound(path+"/peerings/"+req.Name))
		}
		return
	}
	if err := s.checkDeleteConnection(path); err != nil {
		apierr.Write(w, err)
		return
	}
	p, _, _ := pathParts(path)
	op, err := s.startOp(r.Context(), opSpec{project: p, scope: "global", opType: "removePeering", target: path, targetID: cur.Id},
		func(ctx context.Context) error { return s.dropConnection(ctx, path) })
	reply(w, op, err)
}

// writeLRO renders a google.longrunning operation as REST JSON.
func writeLRO(w http.ResponseWriter, op proto.Message, err error) {
	if err != nil {
		apierr.Write(w, err)
		return
	}
	b, merr := protojson.Marshal(op)
	if merr != nil {
		apierr.Write(w, merr)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_, _ = w.Write(b)
}
