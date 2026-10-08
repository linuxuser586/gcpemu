package compute

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/locations"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Network endpoint groups: zonal (FR-GKE-008), regional (serverless,
// Private Service Connect, internet and port-mapping NEGs) and global
// (internet NEGs). GKE's NEG sync uses the emu.VPC methods below; users and
// the load balancer use the REST API. Only zonal and internet IP endpoints
// carry traffic; the other kinds are recorded.

const (
	nsNEGs        = "compute/networkEndpointGroups"
	nsNEGEndpoint = "compute/networkEndpoints"
)

func negPath(project, zone, name string) string {
	return "projects/" + project + "/zones/" + zone + "/networkEndpointGroups/" + name
}

// negLoc validates the project and location of a NEG request and returns
// the location segment ("zones/Z", "regions/R" or "global").
func (s *Service) negLoc(r *http.Request) (project, loc string, err error) {
	p := r.PathValue("project")
	if err := s.env.EnsureProject(p); err != nil {
		return p, "", err
	}
	switch zone, region := r.PathValue("zone"), r.PathValue("region"); {
	case zone != "":
		return p, "zones/" + zone, checkZone(p, zone)
	case region != "":
		return p, "regions/" + region, checkRegion(p, region)
	}
	return p, "global", nil
}

// negPerm names the IAM permission of verb on NEGs at loc: regional NEGs
// have their own resource type.
func negPerm(loc, verb string) string {
	if strings.HasPrefix(loc, "regions/") {
		return "compute.regionNetworkEndpointGroups." + verb
	}
	return "compute.networkEndpointGroups." + verb
}

// negLocOf returns the location segment of a NEG path.
func negLocOf(path string) string {
	segs := strings.Split(path, "/")
	if len(segs) == 5 {
		return "global"
	}
	return segs[2] + "/" + segs[3]
}

func (s *Service) insertNEG(w http.ResponseWriter, r *http.Request) {
	p, loc, err := s.negLoc(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var g computev1.NetworkEndpointGroup
	if _, err := decode(r, &g); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := validName("resource.name", g.Name); err != nil {
		apierr.Write(w, err)
		return
	}
	path := "projects/" + p + "/" + loc + "/networkEndpointGroups/" + g.Name
	if err := s.check(r.Context(), negPerm(loc, "create"), path); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.env.Store.View(func(tx store.Tx) error { return s.prepareNEG(tx, path, &g) }); err != nil {
		apierr.Write(w, err)
		return
	}
	g.Id = s.env.IDs.Uint64()
	op, err := s.startOp(r.Context(), opSpec{project: p, scope: loc, opType: "insert", target: path, targetID: g.Id},
		func(ctx context.Context) error { return s.createNEG(path, &g) })
	reply(w, op, err)
}

// prepareNEG validates a NEG and canonicalises its references.
func (s *Service) prepareNEG(tx store.Tx, path string, g *computev1.NetworkEndpointGroup) error {
	if store.Exists(tx, nsNEGs, path) {
		return errExists(path)
	}
	p, l, _ := pathParts(path)
	loc := negLocOf(path)
	if g.NetworkEndpointType == "" {
		g.NetworkEndpointType = "GCE_VM_IP_PORT"
	}
	region := l
	switch {
	case strings.HasPrefix(loc, "zones/"):
		region, _ = locations.ZoneRegion(l)
		switch g.NetworkEndpointType {
		case "GCE_VM_IP_PORT", "GCE_VM_IP", "NON_GCP_PRIVATE_IP_PORT":
		default:
			return errInvalidField("resource.networkEndpointType", g.NetworkEndpointType, "Zonal network endpoint groups support GCE_VM_IP_PORT, GCE_VM_IP and NON_GCP_PRIVATE_IP_PORT.")
		}
	case loc == "global":
		switch g.NetworkEndpointType {
		case "INTERNET_FQDN_PORT", "INTERNET_IP_PORT":
		default:
			return errInvalidField("resource.networkEndpointType", g.NetworkEndpointType, "Global network endpoint groups support INTERNET_FQDN_PORT and INTERNET_IP_PORT.")
		}
		if g.Network != "" || g.Subnetwork != "" {
			return errInvalidField("resource.network", g.Network, "Global network endpoint groups cannot specify a network.")
		}
		return s.checkNEGDefaultPort(g)
	default:
		switch g.NetworkEndpointType {
		case "SERVERLESS":
			if err := checkServerlessNEG(g); err != nil {
				return err
			}
			if g.Network != "" || g.Subnetwork != "" {
				return errInvalidField("resource.network", g.Network, "Serverless network endpoint groups cannot specify a network.")
			}
			return nil
		case "PRIVATE_SERVICE_CONNECT":
			if g.PscTargetService == "" {
				return errRequired("resource.pscTargetService")
			}
			if g.Network == "" && g.Subnetwork == "" {
				return nil
			}
		case "INTERNET_FQDN_PORT", "INTERNET_IP_PORT":
			if g.Network == "" {
				return errRequired("resource.network")
			}
		case "GCE_VM_IP_PORTMAP":
		default:
			return errInvalidField("resource.networkEndpointType", g.NetworkEndpointType, "Regional network endpoint groups support SERVERLESS, PRIVATE_SERVICE_CONNECT, INTERNET_FQDN_PORT, INTERNET_IP_PORT and GCE_VM_IP_PORTMAP.")
		}
	}
	if g.Subnetwork != "" {
		sp, err := regionalRef(p, region, "subnetworks", g.Subnetwork)
		if err != nil {
			return errInvalidField("resource.subnetwork", g.Subnetwork, "The URL is malformed.")
		}
		sn, ok := get[computev1.Subnetwork](tx, nsSubnets, sp)
		if !ok {
			return errInvalidField("resource.subnetwork", g.Subnetwork, "The referenced subnetwork resource cannot be found.")
		}
		g.Subnetwork = sn.SelfLink
		if g.Network == "" {
			g.Network = sn.Network
		}
	}
	np, err := resolveNetworkRef(p, g.Network)
	if err != nil {
		return err
	}
	if _, ok := get[computev1.Network](tx, nsNetworks, np); !ok {
		return errInvalidField("resource.network", g.Network, "The referenced network resource cannot be found.")
	}
	g.Network = link(np)
	return s.checkNEGDefaultPort(g)
}

func (s *Service) checkNEGDefaultPort(g *computev1.NetworkEndpointGroup) error {
	if g.DefaultPort < 0 || g.DefaultPort > 65535 {
		return errInvalidField("resource.defaultPort", g.DefaultPort, "Must be between 1 and 65535.")
	}
	return nil
}

// checkServerlessNEG requires exactly one serverless target.
func checkServerlessNEG(g *computev1.NetworkEndpointGroup) error {
	n := 0
	for _, set := range []bool{g.CloudRun != nil, g.AppEngine != nil, g.CloudFunction != nil} {
		if set {
			n++
		}
	}
	if n != 1 {
		return errInvalidField("resource", g.Name, "Exactly one of cloudRun, appEngine and cloudFunction must be specified for a SERVERLESS network endpoint group.")
	}
	if g.DefaultPort != 0 {
		return errInvalidField("resource.defaultPort", g.DefaultPort, "Serverless network endpoint groups cannot specify a default port.")
	}
	return nil
}

// endpointless reports NEG types whose endpoints the user cannot manage.
func endpointless(g *computev1.NetworkEndpointGroup) bool {
	return g.NetworkEndpointType == "SERVERLESS" || g.NetworkEndpointType == "PRIVATE_SERVICE_CONNECT"
}

func internetNEG(g *computev1.NetworkEndpointGroup) bool {
	return g.NetworkEndpointType == "INTERNET_FQDN_PORT" || g.NetworkEndpointType == "INTERNET_IP_PORT"
}

func (s *Service) createNEG(path string, g *computev1.NetworkEndpointGroup) error {
	p, _, _ := pathParts(path)
	loc := negLocOf(path)
	return s.env.Store.Update(func(tx store.Tx) error {
		if err := s.prepareNEG(tx, path, g); err != nil {
			return err
		}
		if g.Id == 0 {
			g.Id = s.env.IDs.Uint64()
		}
		g.Kind = "compute#networkEndpointGroup"
		g.CreationTimestamp = stamp(s.env.Clock.Now())
		g.SelfLink = link(path)
		g.Zone, g.Region = "", ""
		switch {
		case strings.HasPrefix(loc, "zones/"):
			g.Zone = link("projects/" + p + "/" + loc)
		case strings.HasPrefix(loc, "regions/"):
			g.Region = link("projects/" + p + "/" + loc)
		}
		g.Size = 0
		g.ForceSendFields = []string{"Size"}
		return store.PutJSON(tx, nsNEGs, path, g)
	})
}

// loadNEG authorizes verb on the NEG a request names and loads it.
func (s *Service) loadNEG(r *http.Request, verb string) (string, *computev1.NetworkEndpointGroup, error) {
	p, loc, err := s.negLoc(r)
	if err != nil {
		return "", nil, err
	}
	path := "projects/" + p + "/" + loc + "/networkEndpointGroups/" + r.PathValue("neg")
	if err := s.check(r.Context(), negPerm(loc, verb), path); err != nil {
		return "", nil, err
	}
	var g *computev1.NetworkEndpointGroup
	var ok bool
	_ = s.env.Store.View(func(tx store.Tx) error { g, ok = get[computev1.NetworkEndpointGroup](tx, nsNEGs, path); return nil })
	if !ok {
		return "", nil, errNotFound(path)
	}
	g.ForceSendFields = []string{"Size"}
	return path, g, nil
}

func (s *Service) getNEG(w http.ResponseWriter, r *http.Request) {
	_, g, err := s.loadNEG(r, "get")
	reply(w, g, err)
}

func (s *Service) listNEGs(w http.ResponseWriter, r *http.Request) {
	p, loc, err := s.negLoc(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.check(r.Context(), negPerm(loc, "list"), "projects/"+p); err != nil {
		apierr.Write(w, err)
		return
	}
	coll := "projects/" + p + "/" + loc + "/networkEndpointGroups"
	var items []listItem
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, g := range list[computev1.NetworkEndpointGroup](tx, nsNEGs, coll+"/") {
			g.ForceSendFields = []string{"Size"}
			items = append(items, listItem{key: g.Name, v: g})
		}
		return nil
	})
	writeList(w, r, "compute#networkEndpointGroupList", coll, items)
}

func (s *Service) aggregatedNEGs(w http.ResponseWriter, r *http.Request) {
	s.aggregated(w, r, []string{nsNEGs}, "compute.networkEndpointGroups.list", "compute#networkEndpointGroupAggregatedList", "networkEndpointGroups",
		func(b []byte) (string, string, any) {
			var v computev1.NetworkEndpointGroup
			_ = json.Unmarshal(b, &v)
			v.ForceSendFields = []string{"Size"}
			return v.SelfLink, v.Name, &v
		})
}

func (s *Service) deleteNEG(w http.ResponseWriter, r *http.Request) {
	path, g, err := s.loadNEG(r, "delete")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if u := s.externalUser(r.Context(), path); u != "" {
		apierr.Write(w, errInUse("networkEndpointGroup", path, u))
		return
	}
	p, _, _ := pathParts(path)
	op, err := s.startOp(r.Context(), opSpec{project: p, scope: negLocOf(path), opType: "delete", target: path, targetID: g.Id},
		func(ctx context.Context) error {
			if u := s.externalUser(ctx, path); u != "" {
				return errInUse("networkEndpointGroup", path, u)
			}
			return s.env.Store.Update(func(tx store.Tx) error {
				if !store.Exists(tx, nsNEGs, path) {
					return errNotFound(path)
				}
				if err := tx.Delete(nsNEGEndpoint, path); err != nil {
					return err
				}
				return tx.Delete(nsNEGs, path)
			})
		})
	reply(w, op, err)
}

// endpointKey identifies an endpoint for attach/detach de-duplication.
func endpointKey(e *computev1.NetworkEndpoint) string {
	return fmt.Sprintf("%s|%s|%s|%d", lastSeg(e.Instance), e.IpAddress, e.Fqdn, e.Port)
}

// normalizeEndpoints validates endpoints against the NEG.
func normalizeEndpoints(g *computev1.NetworkEndpointGroup, eps []*computev1.NetworkEndpoint) error {
	if endpointless(g) {
		return errInvalidField("networkEndpoints", len(eps), "Network endpoints cannot be attached to a "+g.NetworkEndpointType+" network endpoint group.")
	}
	for i, e := range eps {
		f := fmt.Sprintf("networkEndpoints[%d]", i)
		switch g.NetworkEndpointType {
		case "INTERNET_FQDN_PORT":
			if e.Fqdn == "" {
				return errRequired(f + ".fqdn")
			}
			if e.IpAddress != "" || e.Instance != "" {
				return errInvalidField(f+".ipAddress", e.IpAddress, "INTERNET_FQDN_PORT endpoints have only fqdn and port.")
			}
		case "INTERNET_IP_PORT":
			if e.Fqdn != "" || e.Instance != "" {
				return errInvalidField(f+".fqdn", e.Fqdn, "INTERNET_IP_PORT endpoints have only ipAddress and port.")
			}
		default:
			if e.Fqdn != "" {
				return errInvalidField(f+".fqdn", e.Fqdn, "Only INTERNET_FQDN_PORT endpoints have an fqdn.")
			}
		}
		if internetNEG(g) {
			if e.IpAddress != "" {
				if _, ok := parseIP(e.IpAddress); !ok {
					return errInvalidField(f+".ipAddress", e.IpAddress, "Must be a valid IPv4 address.")
				}
			}
			if e.Port == 0 {
				e.Port = g.DefaultPort
			}
			if e.Port < 0 || e.Port > 65535 {
				return errInvalidField(f+".port", e.Port, "Must be between 1 and 65535.")
			}
			continue
		}
		if e.IpAddress == "" && e.Instance == "" {
			return errRequired(f + ".ipAddress")
		}
		if e.IpAddress != "" {
			if _, ok := parseIP(e.IpAddress); !ok {
				return errInvalidField(f+".ipAddress", e.IpAddress, "Must be a valid IPv4 address.")
			}
		}
		if g.NetworkEndpointType == "GCE_VM_IP" {
			continue
		}
		if e.Port == 0 {
			if g.DefaultPort == 0 {
				return errRequired(f + ".port")
			}
			e.Port = g.DefaultPort
		}
		if e.Port < 1 || e.Port > 65535 {
			return errInvalidField(f+".port", e.Port, "Must be between 1 and 65535.")
		}
		if e.Instance != "" {
			e.Instance = lastSeg(e.Instance)
		}
	}
	return nil
}

// changeEndpoints applies attach (add=true) or detach and keeps size.
func (s *Service) changeEndpoints(tx store.Tx, path string, eps []*computev1.NetworkEndpoint, add, replace bool) error {
	g, ok := get[computev1.NetworkEndpointGroup](tx, nsNEGs, path)
	if !ok {
		return errNotFound(path)
	}
	var cur []*computev1.NetworkEndpoint
	if !replace {
		_ = store.GetJSON(tx, nsNEGEndpoint, path, &cur)
	}
	idx := map[string]int{}
	for i, e := range cur {
		idx[endpointKey(e)] = i
	}
	for _, e := range eps {
		k := endpointKey(e)
		_, have := idx[k]
		switch {
		case add && !have:
			idx[k] = len(cur)
			cur = append(cur, e)
		case !add && have:
			cur[idx[k]] = nil
		}
	}
	kept := cur[:0]
	for _, e := range cur {
		if e != nil {
			kept = append(kept, e)
		}
	}
	if internetNEG(g) && len(kept) > 1 {
		return errInvalidField("networkEndpoints", len(kept), "Internet network endpoint groups can have at most one endpoint.")
	}
	g.Size = int64(len(kept))
	g.ForceSendFields = []string{"Size"}
	if err := store.PutJSON(tx, nsNEGEndpoint, path, kept); err != nil {
		return err
	}
	return store.PutJSON(tx, nsNEGs, path, g)
}

func (s *Service) attachEndpoints(w http.ResponseWriter, r *http.Request) { s.attachDetach(w, r, true) }
func (s *Service) detachEndpoints(w http.ResponseWriter, r *http.Request) {
	s.attachDetach(w, r, false)
}

func (s *Service) attachDetach(w http.ResponseWriter, r *http.Request, add bool) {
	verb, opType := "attachNetworkEndpoints", "attachNetworkEndpoints"
	if !add {
		verb, opType = "detachNetworkEndpoints", "detachNetworkEndpoints"
	}
	path, g, err := s.loadNEG(r, verb)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var req struct {
		NetworkEndpoints []*computev1.NetworkEndpoint `json:"networkEndpoints"`
	}
	if _, err := decode(r, &req); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := normalizeEndpoints(g, req.NetworkEndpoints); err != nil {
		apierr.Write(w, err)
		return
	}
	p, _, _ := pathParts(path)
	op, err := s.startOp(r.Context(), opSpec{project: p, scope: negLocOf(path), opType: opType, target: path, targetID: g.Id},
		func(ctx context.Context) error {
			return s.env.Store.Update(func(tx store.Tx) error { return s.changeEndpoints(tx, path, req.NetworkEndpoints, add, false) })
		})
	reply(w, op, err)
}

func (s *Service) listNetworkEndpoints(w http.ResponseWriter, r *http.Request) {
	path, _, err := s.loadNEG(r, "get")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var req computev1.NetworkEndpointGroupsListEndpointsRequest
	if b, _ := readBody(r, true); len(b) > 0 {
		_ = json.Unmarshal(b, &req)
	}
	var eps []*computev1.NetworkEndpoint
	_ = s.env.Store.View(func(tx store.Tx) error { return store.GetJSON(tx, nsNEGEndpoint, path, &eps) })
	var items []listItem
	for _, e := range eps {
		it := &computev1.NetworkEndpointWithHealthStatus{NetworkEndpoint: e}
		items = append(items, listItem{key: endpointKey(e), v: it})
	}
	pg, next, err := page(r, items)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	out := map[string]any{"kind": "compute#networkEndpointGroupsListNetworkEndpoints", "id": path + "/listNetworkEndpoints"}
	if len(pg) > 0 {
		out["items"] = pg
	}
	if next != "" {
		out["nextPageToken"] = next
	}
	writeJSON(w, http.StatusOK, out)
}

// --- emu.VPC NEG methods (FR-GKE-008) ---

// UpsertNEG creates a zonal GCE_VM_IP_PORT NEG if absent.
func (s *Service) UpsertNEG(ctx context.Context, project, zone, name, network, subnetwork string, defaultPort int, description string) error {
	if err := s.env.EnsureProject(project); err != nil {
		return err
	}
	if !locations.IsZone(zone) {
		return errNotFound("projects/" + project + "/zones/" + zone)
	}
	path := negPath(project, zone, name)
	exists := false
	_ = s.env.Store.View(func(tx store.Tx) error { exists = store.Exists(tx, nsNEGs, path); return nil })
	if exists {
		return nil
	}
	g := &computev1.NetworkEndpointGroup{
		Name: name, NetworkEndpointType: "GCE_VM_IP_PORT", Network: network, Subnetwork: subnetwork,
		DefaultPort: int64(defaultPort), Description: description,
	}
	if err := s.createNEG(path, g); err != nil && apierr.From(err).LegacyReason != "alreadyExists" {
		return err
	}
	return nil
}

// SetNEGEndpoints replaces a NEG's endpoints.
func (s *Service) SetNEGEndpoints(ctx context.Context, project, zone, name string, eps []emu.NEGEndpoint) error {
	path := negPath(project, zone, name)
	var in []*computev1.NetworkEndpoint
	for _, e := range eps {
		in = append(in, &computev1.NetworkEndpoint{IpAddress: e.IP, Port: int64(e.Port), Instance: e.Instance})
	}
	return s.env.Store.Update(func(tx store.Tx) error {
		g, ok := get[computev1.NetworkEndpointGroup](tx, nsNEGs, path)
		if !ok {
			return errNotFound(path)
		}
		if err := normalizeEndpoints(g, in); err != nil {
			return err
		}
		return s.changeEndpoints(tx, path, in, true, true)
	})
}

// DeleteNEG removes a NEG and its endpoints; a missing NEG is not an error.
// It fails while a load balancer backend references the NEG.
func (s *Service) DeleteNEG(ctx context.Context, project, zone, name string) error {
	path := negPath(project, zone, name)
	if u := s.externalUser(ctx, path); u != "" {
		return errInUse("networkEndpointGroup", path, u)
	}
	return s.env.Store.Update(func(tx store.Tx) error {
		if err := tx.Delete(nsNEGEndpoint, path); err != nil {
			return err
		}
		return tx.Delete(nsNEGs, path)
	})
}

// NEGEndpoints returns the endpoints of a NEG given by path or URL (used by
// the M3 load balancer).
func (s *Service) NEGEndpoints(ctx context.Context, neg string) ([]emu.NEGEndpoint, error) {
	path := relPath(neg)
	if !strings.Contains(path, "/networkEndpointGroups/") {
		return nil, errNotFound(path)
	}
	var eps []*computev1.NetworkEndpoint
	var ok bool
	_ = s.env.Store.View(func(tx store.Tx) error {
		ok = store.Exists(tx, nsNEGs, path)
		return store.GetJSON(tx, nsNEGEndpoint, path, &eps)
	})
	if !ok {
		return nil, errNotFound(path)
	}
	out := make([]emu.NEGEndpoint, 0, len(eps))
	for _, e := range eps {
		if e.IpAddress == "" {
			continue // FQDN endpoints are recorded, not dialled
		}
		out = append(out, emu.NEGEndpoint{IP: e.IpAddress, Port: int(e.Port), Instance: e.Instance})
	}
	return out, nil
}
