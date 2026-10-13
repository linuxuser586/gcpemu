package dns

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"

	dnsv1 "google.golang.org/api/dns/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// DNS server policies (FR-DNS-006). A policy and its network bindings are
// Recorded: inbound forwarding, logging, alternative name servers and DNS64
// are stored and returned but do not change how the data plane answers.

func policyKey(project, name string) string { return project + "/" + name }

func policyResource(project, name string) string {
	return "//dns.googleapis.com/projects/" + project + "/policies/" + name
}

// normalizePolicy validates a policy's mutable fields and fills kinds. With
// checkIAM it also checks the caller in ctx may bind the policy's networks.
func (s *Service) normalizePolicy(ctx context.Context, p *dnsv1.Policy, checkIAM bool) error {
	if len(p.Description) > 1024 {
		return errInvalid("entity.policy.description", p.Description)
	}
	perm := ""
	if checkIAM {
		perm = "dns.networks.bindPrivateDNSPolicy"
	}
	seen := map[string]bool{}
	for i, n := range p.Networks {
		f := "entity.policy.networks[" + strconv.Itoa(i) + "].networkUrl"
		if n == nil {
			return errRequired(f)
		}
		u, err := s.networkURL(ctx, f, n.NetworkUrl, perm)
		if err != nil {
			return err
		}
		if seen[u] {
			return errInvalid(f, n.NetworkUrl)
		}
		seen[u] = true
		n.Kind, n.NetworkUrl = kindPolicyNet, u
	}
	if c := p.AlternativeNameServerConfig; c != nil {
		c.Kind = kindPolicyANS
		for i, t := range c.TargetNameServers {
			f := "entity.policy.alternativeNameServerConfig.targetNameServers[" + strconv.Itoa(i) + "]"
			if t == nil || (t.Ipv4Address == "" && t.Ipv6Address == "") {
				return errRequired(f + ".ipv4Address")
			}
			if ip := net.ParseIP(t.Ipv4Address); t.Ipv4Address != "" && (ip == nil || ip.To4() == nil) {
				return errInvalid(f+".ipv4Address", t.Ipv4Address)
			}
			if ip := net.ParseIP(t.Ipv6Address); t.Ipv6Address != "" && (ip == nil || ip.To4() != nil) {
				return errInvalid(f+".ipv6Address", t.Ipv6Address)
			}
			switch t.ForwardingPath {
			case "", "default", "private":
			default:
				return errInvalid(f+".forwardingPath", t.ForwardingPath)
			}
			t.Kind = kindPolicyTarget
		}
	}
	if c := p.Dns64Config; c != nil {
		c.Kind = kindPolicyDNS64
		if c.Scope != nil {
			c.Scope.Kind = kindPolicyScope
		}
	}
	p.Kind = kindPolicy
	return nil
}

// checkPolicyNetworks fails if one of p's networks is bound to another
// policy: Cloud DNS allows one policy per network.
func checkPolicyNetworks(tx store.Tx, key string, p *dnsv1.Policy) error {
	nets := map[string]int{}
	for i, n := range p.Networks {
		nets[n.NetworkUrl] = i
	}
	var err error
	tx.Scan(nsPolicies, "", func(k string, b []byte) bool {
		if k == key {
			return true
		}
		var o dnsv1.Policy
		if json.Unmarshal(b, &o) != nil {
			return true
		}
		for _, n := range o.Networks {
			if i, ok := nets[n.NetworkUrl]; ok {
				err = apierr.InvalidArgument("Invalid value for 'entity.policy.networks[%d]': the network '%s' is already bound to policy '%s'.", i, n.NetworkUrl, o.Name).
					WithLegacy("invalid")
				return false
			}
		}
		return true
	})
	return err
}

// policyFor checks perm on the policy named in the request and loads it.
func (s *Service) policyFor(r *http.Request, perm string) (string, *dnsv1.Policy, error) {
	proj, err := s.projectParam(r)
	if err != nil {
		return "", nil, err
	}
	name := r.PathValue("policy")
	if err := s.env.Auth.Check(r.Context(), perm, policyResource(proj, name)); err != nil {
		return "", nil, err
	}
	var p dnsv1.Policy
	err = s.env.Store.View(func(tx store.Tx) error { return store.GetJSON(tx, nsPolicies, policyKey(proj, name), &p) })
	if err != nil {
		return "", nil, errNotFound("parameters.policy", name)
	}
	return proj, &p, nil
}

func (s *Service) createPolicy(w http.ResponseWriter, r *http.Request) {
	proj, err := s.projectParam(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.env.Auth.Check(r.Context(), "dns.policies.create", projectResource(proj)); err != nil {
		apierr.Write(w, err)
		return
	}
	var p dnsv1.Policy
	if err := decode(r, &p); err != nil {
		apierr.Write(w, err)
		return
	}
	switch {
	case p.Name == "":
		err = errRequired("entity.policy.name")
	case !validZoneName(p.Name):
		err = errInvalid("entity.policy.name", p.Name)
	default:
		err = s.normalizePolicy(r.Context(), &p, true)
	}
	if err != nil {
		apierr.Write(w, err)
		return
	}
	p.Id = s.env.IDs.Uint64()
	k := policyKey(proj, p.Name)
	err = s.update(func(tx store.Tx) error {
		if store.Exists(tx, nsPolicies, k) {
			return errExists("entity.policy", p.Name)
		}
		if err := checkPolicyNetworks(tx, k, &p); err != nil {
			return err
		}
		return store.PutJSON(tx, nsPolicies, k, &p)
	})
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, &p)
}

func (s *Service) getPolicy(w http.ResponseWriter, r *http.Request) {
	_, p, err := s.policyFor(r, "dns.policies.get")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Service) listPolicies(w http.ResponseWriter, r *http.Request) {
	proj, err := s.projectParam(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.env.Auth.Check(r.Context(), "dns.policies.list", projectResource(proj)); err != nil {
		apierr.Write(w, err)
		return
	}
	var items []keyed[*dnsv1.Policy]
	_ = s.env.Store.View(func(tx store.Tx) error {
		recs, err := store.ListJSON[*dnsv1.Policy](tx, nsPolicies, proj+"/")
		for _, p := range recs {
			items = append(items, keyed[*dnsv1.Policy]{p.Name, p})
		}
		return err
	})
	page, next, err := paginate(r, items)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if page == nil {
		page = []*dnsv1.Policy{}
	}
	writeJSON(w, http.StatusOK, &dnsv1.PoliciesListResponse{Kind: kindPolicyList, Policies: page, NextPageToken: next})
}

func (s *Service) patchPolicy(w http.ResponseWriter, r *http.Request)  { s.modifyPolicy(w, r, true) }
func (s *Service) updatePolicy(w http.ResponseWriter, r *http.Request) { s.modifyPolicy(w, r, false) }

// modifyPolicy implements policies.patch (merge) and update (replace).
// Both answer {"policy": ...}.
func (s *Service) modifyPolicy(w http.ResponseWriter, r *http.Request, merge bool) {
	proj, old, err := s.policyFor(r, "dns.policies.update")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var in dnsv1.Policy
	set, err := decodeFields(r, &in, merge)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if in.Name != "" && in.Name != old.Name {
		apierr.Write(w, errInvalid("entity.policy.name", in.Name))
		return
	}
	np := *old
	if set("description") {
		np.Description = in.Description
	}
	if set("enableInboundForwarding") {
		np.EnableInboundForwarding = in.EnableInboundForwarding
	}
	if set("enableLogging") {
		np.EnableLogging = in.EnableLogging
	}
	if set("networks") {
		np.Networks = in.Networks
	}
	if set("alternativeNameServerConfig") {
		np.AlternativeNameServerConfig = in.AlternativeNameServerConfig
	}
	if set("dns64Config") {
		np.Dns64Config = in.Dns64Config
	}
	if err := s.normalizePolicy(r.Context(), &np, true); err != nil {
		apierr.Write(w, err)
		return
	}
	k := policyKey(proj, old.Name)
	err = s.update(func(tx store.Tx) error {
		if !store.Exists(tx, nsPolicies, k) {
			return errNotFound("parameters.policy", old.Name)
		}
		if err := checkPolicyNetworks(tx, k, &np); err != nil {
			return err
		}
		return store.PutJSON(tx, nsPolicies, k, &np)
	})
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, &dnsv1.PoliciesPatchResponse{Policy: &np})
}

// deletePolicy deletes a policy that no network is bound to.
func (s *Service) deletePolicy(w http.ResponseWriter, r *http.Request) {
	proj, p, err := s.policyFor(r, "dns.policies.delete")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	k := policyKey(proj, p.Name)
	err = s.update(func(tx store.Tx) error {
		var cur dnsv1.Policy
		if err := store.GetJSON(tx, nsPolicies, k, &cur); err != nil {
			return errNotFound("parameters.policy", p.Name)
		}
		if len(cur.Networks) > 0 {
			return errInUse("entity.policy", p.Name)
		}
		return tx.Delete(nsPolicies, k)
	})
	if err != nil {
		apierr.Write(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// decodeFields decodes a patch or update body into v and returns which
// top-level fields it sets: those present for a patch (merge), all of
// them for an update.
func decodeFields(r *http.Request, v any, merge bool) (func(string) bool, error) {
	var raw map[string]json.RawMessage
	if err := decode(r, &raw); err != nil {
		return nil, err
	}
	b, _ := json.Marshal(raw)
	if err := json.Unmarshal(b, v); err != nil {
		return nil, apierr.InvalidArgument("Invalid JSON payload received. %v", err).WithLegacy("parseError")
	}
	return func(key string) bool { _, ok := raw[key]; return ok || !merge }, nil
}
