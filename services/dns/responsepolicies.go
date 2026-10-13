package dns

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	dnsv1 "google.golang.org/api/dns/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Response policies and their rules (FR-DNS-006). The data plane applies
// them to queries from bound VPC networks (index.go, server.go); GKE
// cluster bindings are Recorded.

const behaviorBypass = "bypassResponsePolicy"

func respRuleKey(policyKey, rule string) string { return policyKey + "/" + rule }

func respPolicyResource(project, name string) string {
	return "//dns.googleapis.com/projects/" + project + "/responsePolicies/" + name
}

var gkeClusterRE = regexp.MustCompile(`(?:^|/)projects/[^/]+/locations/[^/]+/clusters/[^/]+$`)

// normalizeRespPolicy validates a response policy's mutable fields and
// fills kinds. With checkIAM it also checks the caller in ctx may bind its
// networks and clusters.
func (s *Service) normalizeRespPolicy(ctx context.Context, p *dnsv1.ResponsePolicy, checkIAM bool) error {
	if len(p.Description) > 1024 {
		return errInvalid("entity.responsePolicy.description", p.Description)
	}
	perm := ""
	if checkIAM {
		perm = "dns.networks.bindDNSResponsePolicy"
	}
	seen := map[string]bool{}
	for i, n := range p.Networks {
		f := "entity.responsePolicy.networks[" + strconv.Itoa(i) + "].networkUrl"
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
		n.Kind, n.NetworkUrl = kindRespPolicyNet, u
	}
	for i, c := range p.GkeClusters {
		f := "entity.responsePolicy.gkeClusters[" + strconv.Itoa(i) + "].gkeClusterName"
		if c == nil || !gkeClusterRE.MatchString(c.GkeClusterName) {
			name := ""
			if c != nil {
				name = c.GkeClusterName
			}
			return errInvalid(f, name)
		}
		if checkIAM {
			name := c.GkeClusterName[strings.Index(c.GkeClusterName, "projects/"):]
			if err := s.env.Auth.Check(ctx, "dns.gkeClusters.bindDNSResponsePolicy", "//container.googleapis.com/"+name); err != nil {
				return err
			}
		}
		c.Kind = kindRespPolicyGKE
	}
	p.Kind = kindRespPolicy
	return nil
}

// respPolicyFor checks perm on the response policy named in the request
// and loads it with its store key.
func (s *Service) respPolicyFor(r *http.Request, perm string) (string, *dnsv1.ResponsePolicy, error) {
	proj, err := s.projectParam(r)
	if err != nil {
		return "", nil, err
	}
	name := r.PathValue("responsePolicy")
	if err := s.env.Auth.Check(r.Context(), perm, respPolicyResource(proj, name)); err != nil {
		return "", nil, err
	}
	k := policyKey(proj, name)
	var p dnsv1.ResponsePolicy
	err = s.env.Store.View(func(tx store.Tx) error { return store.GetJSON(tx, nsRespPolicies, k, &p) })
	if err != nil {
		return "", nil, errNotFound("parameters.responsePolicy", name)
	}
	return k, &p, nil
}

func (s *Service) createRespPolicy(w http.ResponseWriter, r *http.Request) {
	proj, err := s.projectParam(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.env.Auth.Check(r.Context(), "dns.responsePolicies.create", projectResource(proj)); err != nil {
		apierr.Write(w, err)
		return
	}
	var p dnsv1.ResponsePolicy
	if err := decode(r, &p); err != nil {
		apierr.Write(w, err)
		return
	}
	switch {
	case p.ResponsePolicyName == "":
		err = errRequired("entity.responsePolicy.responsePolicyName")
	case !validZoneName(p.ResponsePolicyName):
		err = errInvalid("entity.responsePolicy.responsePolicyName", p.ResponsePolicyName)
	default:
		err = s.normalizeRespPolicy(r.Context(), &p, true)
	}
	if err != nil {
		apierr.Write(w, err)
		return
	}
	p.Id = int64(s.env.IDs.Uint64() >> 1)
	k := policyKey(proj, p.ResponsePolicyName)
	err = s.update(func(tx store.Tx) error {
		if store.Exists(tx, nsRespPolicies, k) {
			return errExists("entity.responsePolicy", p.ResponsePolicyName)
		}
		return store.PutJSON(tx, nsRespPolicies, k, &p)
	})
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, &p)
}

func (s *Service) getRespPolicy(w http.ResponseWriter, r *http.Request) {
	_, p, err := s.respPolicyFor(r, "dns.responsePolicies.get")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Service) listRespPolicies(w http.ResponseWriter, r *http.Request) {
	proj, err := s.projectParam(r)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.env.Auth.Check(r.Context(), "dns.responsePolicies.list", projectResource(proj)); err != nil {
		apierr.Write(w, err)
		return
	}
	var items []keyed[*dnsv1.ResponsePolicy]
	_ = s.env.Store.View(func(tx store.Tx) error {
		recs, err := store.ListJSON[*dnsv1.ResponsePolicy](tx, nsRespPolicies, proj+"/")
		for _, p := range recs {
			items = append(items, keyed[*dnsv1.ResponsePolicy]{p.ResponsePolicyName, p})
		}
		return err
	})
	page, next, err := paginate(r, items)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if page == nil {
		page = []*dnsv1.ResponsePolicy{}
	}
	writeJSON(w, http.StatusOK, &dnsv1.ResponsePoliciesListResponse{ResponsePolicies: page, NextPageToken: next})
}

func (s *Service) patchRespPolicy(w http.ResponseWriter, r *http.Request) {
	s.modifyRespPolicy(w, r, true)
}
func (s *Service) updateRespPolicy(w http.ResponseWriter, r *http.Request) {
	s.modifyRespPolicy(w, r, false)
}

// modifyRespPolicy implements responsePolicies.patch (merge) and update
// (replace). Both answer {"responsePolicy": ...}.
func (s *Service) modifyRespPolicy(w http.ResponseWriter, r *http.Request, merge bool) {
	k, old, err := s.respPolicyFor(r, "dns.responsePolicies.update")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var in dnsv1.ResponsePolicy
	set, err := decodeFields(r, &in, merge)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if in.ResponsePolicyName != "" && in.ResponsePolicyName != old.ResponsePolicyName {
		apierr.Write(w, errInvalid("entity.responsePolicy.responsePolicyName", in.ResponsePolicyName))
		return
	}
	np := *old
	if set("description") {
		np.Description = in.Description
	}
	if set("labels") {
		np.Labels = in.Labels
	}
	if set("networks") {
		np.Networks = in.Networks
	}
	if set("gkeClusters") {
		np.GkeClusters = in.GkeClusters
	}
	if err := s.normalizeRespPolicy(r.Context(), &np, true); err != nil {
		apierr.Write(w, err)
		return
	}
	err = s.update(func(tx store.Tx) error {
		if !store.Exists(tx, nsRespPolicies, k) {
			return errNotFound("parameters.responsePolicy", old.ResponsePolicyName)
		}
		return store.PutJSON(tx, nsRespPolicies, k, &np)
	})
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, &dnsv1.ResponsePoliciesPatchResponse{ResponsePolicy: &np})
}

// deleteRespPolicy deletes a response policy that has no rules and no
// bound networks or clusters.
func (s *Service) deleteRespPolicy(w http.ResponseWriter, r *http.Request) {
	k, p, err := s.respPolicyFor(r, "dns.responsePolicies.delete")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	name := p.ResponsePolicyName
	err = s.update(func(tx store.Tx) error {
		var cur dnsv1.ResponsePolicy
		if err := store.GetJSON(tx, nsRespPolicies, k, &cur); err != nil {
			return errNotFound("parameters.responsePolicy", name)
		}
		if store.Count(tx, nsRespRules, k+"/") > 0 {
			return errNotEmpty(name)
		}
		if len(cur.Networks) > 0 || len(cur.GkeClusters) > 0 {
			return errInUse("entity.responsePolicy", name)
		}
		return tx.Delete(nsRespPolicies, k)
	})
	if err != nil {
		apierr.Write(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Rules.

// validateRule checks a rule's DNS name, behavior and local data. A rule
// either bypasses the policy or answers with local data owned by its name.
func validateRule(rl *dnsv1.ResponsePolicyRule) error {
	const f = "entity.responsePolicyRule"
	switch {
	case rl.DnsName == "":
		return errRequired(f + ".dnsName")
	case !validDNSName(rl.DnsName) || strings.Contains(strings.TrimPrefix(rl.DnsName, "*."), "*"):
		return errInvalid(f+".dnsName", rl.DnsName)
	}
	switch rl.Behavior {
	case "", "behaviorUnspecified":
		if rl.LocalData == nil || len(rl.LocalData.LocalDatas) == 0 {
			return errRequired(f + ".localData")
		}
	case behaviorBypass:
		if rl.LocalData != nil && len(rl.LocalData.LocalDatas) > 0 {
			return apierr.InvalidArgument("The resource '%s' cannot specify both behavior and localData.", f)
		}
		rl.LocalData = nil
	default:
		return errInvalid(f+".behavior", rl.Behavior)
	}
	if rl.LocalData != nil {
		types := map[string]bool{}
		for i, rs := range rl.LocalData.LocalDatas {
			lf := f + ".localData.localDatas[" + strconv.Itoa(i) + "]"
			// Validated as below the zone of the rule's parent, so a CNAME
			// is not at an apex.
			if err := validateRRSet(rs, lf, parent(strings.ToLower(rl.DnsName))); err != nil {
				return err
			}
			if !strings.EqualFold(rs.Name, rl.DnsName) || rs.RoutingPolicy != nil {
				return errInvalid(lf+".name", rs.Name)
			}
			if types[rs.Type] {
				return errExists(lf, rrsetLabel(rs))
			}
			types[rs.Type] = true
			rs.Kind = kindRRSet
		}
		if types["CNAME"] && len(types) > 1 {
			return apierr.InvalidArgument("The resource '%s.localData' cannot have a CNAME record set together with other record sets.", f)
		}
	}
	rl.Kind = kindRespRule
	return nil
}

// checkRuleName fails if another rule of the policy (keys under prefix)
// has rl's DNS name.
func checkRuleName(tx store.Tx, prefix, key string, rl *dnsv1.ResponsePolicyRule) error {
	var err error
	tx.Scan(nsRespRules, prefix, func(k string, b []byte) bool {
		var o dnsv1.ResponsePolicyRule
		if k != key && json.Unmarshal(b, &o) == nil && strings.EqualFold(o.DnsName, rl.DnsName) {
			err = errExists("entity.responsePolicyRule.dnsName", rl.DnsName)
			return false
		}
		return true
	})
	return err
}

// ruleFor checks perm on the response policy of the request and loads the
// rule named in it with its store key.
func (s *Service) ruleFor(r *http.Request, perm string) (string, *dnsv1.ResponsePolicyRule, error) {
	pk, _, err := s.respPolicyFor(r, perm)
	if err != nil {
		return "", nil, err
	}
	name := r.PathValue("rule")
	k := respRuleKey(pk, name)
	var rl dnsv1.ResponsePolicyRule
	if err := s.env.Store.View(func(tx store.Tx) error { return store.GetJSON(tx, nsRespRules, k, &rl) }); err != nil {
		return "", nil, errNotFound("parameters.responsePolicyRule", name)
	}
	return k, &rl, nil
}

func (s *Service) createRule(w http.ResponseWriter, r *http.Request) {
	pk, _, err := s.respPolicyFor(r, "dns.responsePolicyRules.create")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var rl dnsv1.ResponsePolicyRule
	if err := decode(r, &rl); err != nil {
		apierr.Write(w, err)
		return
	}
	switch {
	case rl.RuleName == "":
		err = errRequired("entity.responsePolicyRule.ruleName")
	case !validZoneName(rl.RuleName):
		err = errInvalid("entity.responsePolicyRule.ruleName", rl.RuleName)
	default:
		err = validateRule(&rl)
	}
	if err != nil {
		apierr.Write(w, err)
		return
	}
	k := respRuleKey(pk, rl.RuleName)
	err = s.update(func(tx store.Tx) error {
		if !store.Exists(tx, nsRespPolicies, pk) {
			return errNotFound("parameters.responsePolicy", r.PathValue("responsePolicy"))
		}
		if store.Exists(tx, nsRespRules, k) {
			return errExists("entity.responsePolicyRule", rl.RuleName)
		}
		if err := checkRuleName(tx, pk+"/", k, &rl); err != nil {
			return err
		}
		return store.PutJSON(tx, nsRespRules, k, &rl)
	})
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, &rl)
}

func (s *Service) getRule(w http.ResponseWriter, r *http.Request) {
	_, rl, err := s.ruleFor(r, "dns.responsePolicyRules.get")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rl)
}

func (s *Service) listRules(w http.ResponseWriter, r *http.Request) {
	pk, _, err := s.respPolicyFor(r, "dns.responsePolicyRules.list")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var items []keyed[*dnsv1.ResponsePolicyRule]
	_ = s.env.Store.View(func(tx store.Tx) error {
		recs, err := store.ListJSON[*dnsv1.ResponsePolicyRule](tx, nsRespRules, pk+"/")
		for _, rl := range recs {
			items = append(items, keyed[*dnsv1.ResponsePolicyRule]{rl.RuleName, rl})
		}
		return err
	})
	page, next, err := paginate(r, items)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if page == nil {
		page = []*dnsv1.ResponsePolicyRule{}
	}
	writeJSON(w, http.StatusOK, &dnsv1.ResponsePolicyRulesListResponse{ResponsePolicyRules: page, NextPageToken: next})
}

func (s *Service) patchRule(w http.ResponseWriter, r *http.Request)  { s.modifyRule(w, r, true) }
func (s *Service) updateRule(w http.ResponseWriter, r *http.Request) { s.modifyRule(w, r, false) }

// modifyRule implements responsePolicyRules.patch (merge) and update
// (replace). Both answer {"responsePolicyRule": ...}.
func (s *Service) modifyRule(w http.ResponseWriter, r *http.Request, merge bool) {
	k, old, err := s.ruleFor(r, "dns.responsePolicyRules.update")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var in dnsv1.ResponsePolicyRule
	set, err := decodeFields(r, &in, merge)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if in.RuleName != "" && in.RuleName != old.RuleName {
		apierr.Write(w, errInvalid("entity.responsePolicyRule.ruleName", in.RuleName))
		return
	}
	nr := *old
	if set("dnsName") {
		nr.DnsName = in.DnsName
	}
	if set("behavior") {
		nr.Behavior = in.Behavior
		if in.Behavior == behaviorBypass {
			nr.LocalData = nil
		}
	}
	if set("localData") {
		nr.LocalData = in.LocalData
	}
	if err := validateRule(&nr); err != nil {
		apierr.Write(w, err)
		return
	}
	pk := k[:strings.LastIndexByte(k, '/')]
	err = s.update(func(tx store.Tx) error {
		if !store.Exists(tx, nsRespRules, k) {
			return errNotFound("parameters.responsePolicyRule", old.RuleName)
		}
		if err := checkRuleName(tx, pk+"/", k, &nr); err != nil {
			return err
		}
		return store.PutJSON(tx, nsRespRules, k, &nr)
	})
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, &dnsv1.ResponsePolicyRulesPatchResponse{ResponsePolicyRule: &nr})
}

func (s *Service) deleteRule(w http.ResponseWriter, r *http.Request) {
	k, rl, err := s.ruleFor(r, "dns.responsePolicyRules.delete")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	err = s.update(func(tx store.Tx) error {
		if !store.Exists(tx, nsRespRules, k) {
			return errNotFound("parameters.responsePolicyRule", rl.RuleName)
		}
		return tx.Delete(nsRespRules, k)
	})
	if err != nil {
		apierr.Write(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
