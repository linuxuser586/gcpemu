package compute

import (
	"context"
	"encoding/json"
	"net/http"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Firewall rules and routes are stored and returned (Recorded fidelity):
// the container networks do not enforce them.

const nsFirewalls = "compute/firewalls"

func (s *Service) insertFirewall(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("project")
	if err := s.env.EnsureProject(p); err != nil {
		apierr.Write(w, err)
		return
	}
	var fw computev1.Firewall
	raw, err := decode(r, &fw)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if hasKey(raw, "priority") {
		fw.ForceSendFields = append(fw.ForceSendFields, "Priority")
	}
	if err := validName("resource.name", fw.Name); err != nil {
		apierr.Write(w, err)
		return
	}
	path := "projects/" + p + "/global/firewalls/" + fw.Name
	if err := s.check(r.Context(), "compute.firewalls.create", path); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.validateFirewall(p, &fw); err != nil {
		apierr.Write(w, err)
		return
	}
	exists := false
	_ = s.env.Store.View(func(tx store.Tx) error { exists = store.Exists(tx, nsFirewalls, path); return nil })
	if exists {
		apierr.Write(w, errExists(path))
		return
	}
	fw.Id = s.env.IDs.Uint64()
	op, err := s.startOp(r.Context(), opSpec{project: p, scope: "global", opType: "insert", target: path, targetID: fw.Id},
		func(ctx context.Context) error { return s.createFirewall(p, &fw) })
	reply(w, op, err)
}

// validateFirewall applies GCP's defaults and checks.
func (s *Service) validateFirewall(project string, fw *computev1.Firewall) error {
	np, err := resolveNetworkRef(project, fw.Network)
	if err != nil {
		return err
	}
	if err := s.env.Store.View(func(tx store.Tx) error { _, err := networkOf(tx, np); return err }); err != nil {
		return errInvalidField("resource.network", fw.Network, "The referenced network resource cannot be found.")
	}
	fw.Network = link(np)
	if fw.Direction == "" {
		fw.Direction = "INGRESS"
	}
	if fw.Direction != "INGRESS" && fw.Direction != "EGRESS" {
		return errInvalidField("resource.direction", fw.Direction, "")
	}
	if (len(fw.Allowed) == 0) == (len(fw.Denied) == 0) {
		return apierr.InvalidArgument("Exactly one of 'allowed' or 'denied' must be specified.").WithLegacy("invalid")
	}
	if fw.Priority == 0 && !contains(fw.ForceSendFields, "Priority") {
		fw.Priority = 1000
	}
	if fw.Priority < 0 || fw.Priority > 65535 {
		return errInvalidField("resource.priority", fw.Priority, "Must be between 0 and 65535.")
	}
	if fw.Direction == "INGRESS" && len(fw.SourceRanges) == 0 && len(fw.SourceTags) == 0 && len(fw.SourceServiceAccounts) == 0 {
		fw.SourceRanges = []string{"0.0.0.0/0"}
	}
	if fw.Direction == "EGRESS" && len(fw.DestinationRanges) == 0 {
		fw.DestinationRanges = []string{"0.0.0.0/0"}
	}
	for _, rng := range append(append([]string{}, fw.SourceRanges...), fw.DestinationRanges...) {
		if _, err := parseCIDR(rng, false); err != nil {
			if _, ok := parseIP(rng); !ok {
				return errInvalidField("resource.sourceRanges", rng, "Must be a valid IPv4 CIDR address range.")
			}
		}
	}
	if fw.LogConfig == nil {
		fw.LogConfig = &computev1.FirewallLogConfig{}
	}
	fw.LogConfig.ForceSendFields = []string{"Enable"}
	if !contains(fw.ForceSendFields, "Disabled") {
		fw.ForceSendFields = append(fw.ForceSendFields, "Disabled")
	}
	return nil
}

func (s *Service) createFirewall(project string, fw *computev1.Firewall) error {
	if fw.Kind == "" {
		if err := s.validateFirewall(project, fw); err != nil {
			return err
		}
	}
	path := "projects/" + project + "/global/firewalls/" + fw.Name
	if fw.Id == 0 {
		fw.Id = s.env.IDs.Uint64()
	}
	fw.Kind = "compute#firewall"
	fw.CreationTimestamp = stamp(s.env.Clock.Now())
	fw.SelfLink = link(path)
	return s.env.Store.Update(func(tx store.Tx) error {
		if store.Exists(tx, nsFirewalls, path) {
			return errExists(path)
		}
		return store.PutJSON(tx, nsFirewalls, path, fw)
	})
}

func (s *Service) loadFirewall(r *http.Request, perm string) (string, *computev1.Firewall, error) {
	p := r.PathValue("project")
	if err := s.env.EnsureProject(p); err != nil {
		return "", nil, err
	}
	path := "projects/" + p + "/global/firewalls/" + r.PathValue("firewall")
	if err := s.check(r.Context(), perm, path); err != nil {
		return "", nil, err
	}
	var fw *computev1.Firewall
	var ok bool
	_ = s.env.Store.View(func(tx store.Tx) error { fw, ok = get[computev1.Firewall](tx, nsFirewalls, path); return nil })
	if !ok {
		return "", nil, errNotFound(path)
	}
	return path, fw, nil
}

func (s *Service) getFirewall(w http.ResponseWriter, r *http.Request) {
	_, fw, err := s.loadFirewall(r, "compute.firewalls.get")
	reply(w, fw, err)
}

func (s *Service) listFirewalls(w http.ResponseWriter, r *http.Request) {
	s.listGlobal(w, r, nsFirewalls, "firewalls", "compute.firewalls.list", "compute#firewallList", func(b []byte) (string, any) {
		var v computev1.Firewall
		_ = json.Unmarshal(b, &v)
		return v.Name, &v
	})
}

// listGlobal serves a list of a global collection.
func (s *Service) listGlobal(w http.ResponseWriter, r *http.Request, ns, coll, perm, kind string, dec func([]byte) (string, any)) {
	p := r.PathValue("project")
	if err := s.env.EnsureProject(p); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.check(r.Context(), perm, "projects/"+p); err != nil {
		apierr.Write(w, err)
		return
	}
	prefix := "projects/" + p + "/global/" + coll
	var items []listItem
	_ = s.env.Store.View(func(tx store.Tx) error {
		tx.Scan(ns, prefix+"/", func(_ string, b []byte) bool {
			k, v := dec(b)
			items = append(items, listItem{key: k, v: v})
			return true
		})
		return nil
	})
	writeList(w, r, kind, prefix, items)
}

// updateFirewall implements both patch (merge) and update (replace).
func (s *Service) updateFirewall(w http.ResponseWriter, r *http.Request) {
	path, cur, err := s.loadFirewall(r, "compute.firewalls.update")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	raw, err := readBody(r, false)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var next computev1.Firewall
	keep := []string{"name", "id", "kind", "selfLink", "creationTimestamp", "network", "direction"}
	if r.Method == http.MethodPut {
		if err := mergePatch(map[string]any{}, raw, &next); err != nil {
			apierr.Write(w, err)
			return
		}
		next.Name, next.Id, next.Kind, next.SelfLink, next.CreationTimestamp = cur.Name, cur.Id, cur.Kind, cur.SelfLink, cur.CreationTimestamp
		if next.Network == "" {
			next.Network = cur.Network
		}
	} else {
		var in map[string]any
		_ = json.Unmarshal(raw, &in)
		// A patch replacing allowed with denied (or vice versa) clears the other.
		if _, ok := in["allowed"]; ok {
			cur.Denied = nil
		}
		if _, ok := in["denied"]; ok {
			cur.Allowed = nil
		}
		if err := mergePatch(cur, raw, &next, keep...); err != nil {
			apierr.Write(w, err)
			return
		}
	}
	p, _, _ := pathParts(path)
	if relPath(next.Network) != relPath(cur.Network) {
		apierr.Write(w, errInvalidField("resource.network", next.Network, "The network of a firewall rule cannot be changed."))
		return
	}
	if err := s.validateFirewall(p, &next); err != nil {
		apierr.Write(w, err)
		return
	}
	opType := "patch"
	if r.Method == http.MethodPut {
		opType = "update"
	}
	op, err := s.startOp(r.Context(), opSpec{project: p, scope: "global", opType: opType, target: path, targetID: cur.Id},
		func(ctx context.Context) error {
			return s.env.Store.Update(func(tx store.Tx) error {
				if !store.Exists(tx, nsFirewalls, path) {
					return errNotFound(path)
				}
				return store.PutJSON(tx, nsFirewalls, path, &next)
			})
		})
	reply(w, op, err)
}

func (s *Service) deleteFirewall(w http.ResponseWriter, r *http.Request) {
	path, cur, err := s.loadFirewall(r, "compute.firewalls.delete")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	s.deleteSimple(w, r, nsFirewalls, path, "global", cur.Id)
}

// deleteSimple deletes a resource without references through an operation.
func (s *Service) deleteSimple(w http.ResponseWriter, r *http.Request, ns, path, scope string, id uint64) {
	p, _, _ := pathParts(path)
	op, err := s.startOp(r.Context(), opSpec{project: p, scope: scope, opType: "delete", target: path, targetID: id},
		func(ctx context.Context) error {
			return s.env.Store.Update(func(tx store.Tx) error {
				if !store.Exists(tx, ns, path) {
					return errNotFound(path)
				}
				return tx.Delete(ns, path)
			})
		})
	reply(w, op, err)
}

// --- routes ---

func (s *Service) insertRoute(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("project")
	if err := s.env.EnsureProject(p); err != nil {
		apierr.Write(w, err)
		return
	}
	var rt computev1.Route
	raw, err := decode(r, &rt)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if hasKey(raw, "priority") {
		rt.ForceSendFields = append(rt.ForceSendFields, "Priority")
	}
	if err := validName("resource.name", rt.Name); err != nil {
		apierr.Write(w, err)
		return
	}
	path := "projects/" + p + "/global/routes/" + rt.Name
	if err := s.check(r.Context(), "compute.routes.create", path); err != nil {
		apierr.Write(w, err)
		return
	}
	if rt.Network == "" {
		apierr.Write(w, errRequired("resource.network"))
		return
	}
	np, err := resolveNetworkRef(p, rt.Network)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if rt.DestRange == "" {
		apierr.Write(w, errRequired("resource.destRange"))
		return
	}
	if _, err := parseCIDR(rt.DestRange, false); err != nil {
		apierr.Write(w, errInvalidField("resource.destRange", rt.DestRange, "Must be a valid IPv4 CIDR address range."))
		return
	}
	hops := 0
	for _, h := range []string{rt.NextHopGateway, rt.NextHopInstance, rt.NextHopIp, rt.NextHopVpnTunnel, rt.NextHopIlb} {
		if h != "" {
			hops++
		}
	}
	if hops != 1 {
		apierr.Write(w, apierr.InvalidArgument("Exactly one of nextHopGateway, nextHopInstance, nextHopIp, nextHopVpnTunnel or nextHopIlb must be specified.").WithLegacy("invalid"))
		return
	}
	if rt.NextHopGateway != "" {
		gp, err := globalRef(p, "gateways", rt.NextHopGateway)
		if err != nil || lastSeg(gp) != "default-internet-gateway" {
			apierr.Write(w, errInvalidField("resource.nextHopGateway", rt.NextHopGateway, "The referenced gateway resource cannot be found."))
			return
		}
		rt.NextHopGateway = link(gp)
	}
	if err := s.env.Store.View(func(tx store.Tx) error {
		if store.Exists(tx, nsRoutes, path) {
			return errExists(path)
		}
		_, err := networkOf(tx, np)
		return err
	}); err != nil {
		apierr.Write(w, err)
		return
	}
	rt.Id = s.env.IDs.Uint64()
	op, err := s.startOp(r.Context(), opSpec{project: p, scope: "global", opType: "insert", target: path, targetID: rt.Id},
		func(ctx context.Context) error {
			rt.Kind = "compute#route"
			rt.CreationTimestamp = stamp(s.env.Clock.Now())
			rt.SelfLink = link(path)
			rt.Network = link(np)
			if rt.Priority == 0 && !contains(rt.ForceSendFields, "Priority") {
				rt.Priority = 1000
			}
			rt.RouteType = "STATIC"
			return s.env.Store.Update(func(tx store.Tx) error {
				if store.Exists(tx, nsRoutes, path) {
					return errExists(path)
				}
				return store.PutJSON(tx, nsRoutes, path, &rt)
			})
		})
	reply(w, op, err)
}

func (s *Service) loadRoute(r *http.Request, perm string) (string, *computev1.Route, error) {
	p := r.PathValue("project")
	if err := s.env.EnsureProject(p); err != nil {
		return "", nil, err
	}
	path := "projects/" + p + "/global/routes/" + r.PathValue("route")
	if err := s.check(r.Context(), perm, path); err != nil {
		return "", nil, err
	}
	var rt *computev1.Route
	var ok bool
	_ = s.env.Store.View(func(tx store.Tx) error { rt, ok = get[computev1.Route](tx, nsRoutes, path); return nil })
	if !ok {
		return "", nil, errNotFound(path)
	}
	return path, rt, nil
}

func (s *Service) getRoute(w http.ResponseWriter, r *http.Request) {
	_, rt, err := s.loadRoute(r, "compute.routes.get")
	reply(w, rt, err)
}

func (s *Service) listRoutes(w http.ResponseWriter, r *http.Request) {
	s.listGlobal(w, r, nsRoutes, "routes", "compute.routes.list", "compute#routeList", func(b []byte) (string, any) {
		var v computev1.Route
		_ = json.Unmarshal(b, &v)
		return v.Name, &v
	})
}

func (s *Service) deleteRoute(w http.ResponseWriter, r *http.Request) {
	path, rt, err := s.loadRoute(r, "compute.routes.delete")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	s.deleteSimple(w, r, nsRoutes, path, "global", rt.Id)
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// hasKey reports whether a JSON object has a top-level key.
func hasKey(raw []byte, key string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	_, ok := m[key]
	return ok
}
