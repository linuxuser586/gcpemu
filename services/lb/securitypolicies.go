package lb

import (
	"context"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/services/compute"
)

// Cloud Armor security policies (securityPolicies and
// regionSecurityPolicies; FR-LB-011). They are recorded: stored, validated
// and attached to backend services and buckets, but the proxy does not
// evaluate their rules (SRS 10.2).

var kindSecurityPolicy = &kind{
	coll: "securityPolicies", typ: "compute#securityPolicy", snake: "security_policy",
	gperm: "compute.securityPolicies", rperm: "compute.regionSecurityPolicies",
	aggKind: "compute#securityPoliciesAggregatedList", listKind: "compute#securityPolicyList",
	noUpdate: true,
	newObj:   func() any { return &computev1.SecurityPolicy{} },
}

// defaultRulePriority is the priority of the match-all default rule every
// policy has.
const defaultRulePriority = 2147483647

func defaultRule() *computev1.SecurityPolicyRule {
	return &computev1.SecurityPolicyRule{
		Action: "allow", Description: "default rule", Priority: defaultRulePriority,
		Match: &computev1.SecurityPolicyRuleMatcher{
			VersionedExpr: "SRC_IPS_V1",
			Config:        &computev1.SecurityPolicyRuleMatcherConfig{SrcIpRanges: []string{"*"}},
		},
	}
}

func prepareSecurityPolicy(ctx context.Context, s *Service, sc scope, path string, obj, old any) error {
	p := obj.(*computev1.SecurityPolicy)
	if old != nil {
		o := old.(*computev1.SecurityPolicy)
		if p.Type == "" {
			p.Type = o.Type
		}
		if p.Type != o.Type {
			return errInvalid("resource.type", p.Type, "The type of a security policy cannot be changed.")
		}
	}
	if p.Type == "" {
		p.Type = "CLOUD_ARMOR"
	}
	switch p.Type {
	case "CLOUD_ARMOR", "CLOUD_ARMOR_EDGE":
	case "CLOUD_ARMOR_NETWORK":
		if sc.region == "" {
			return errInvalid("resource.type", p.Type, "CLOUD_ARMOR_NETWORK policies must be regional.")
		}
	default:
		return errInvalid("resource.type", p.Type, "Must be one of CLOUD_ARMOR, CLOUD_ARMOR_EDGE or CLOUD_ARMOR_NETWORK.")
	}
	if old == nil && len(p.Rules) == 0 {
		p.Rules = []*computev1.SecurityPolicyRule{defaultRule()}
	}
	return normalizeRules(p)
}

// normalizeRules validates the rules, requires the default rule and sorts
// them by priority (GCP's evaluation and list order).
func normalizeRules(p *computev1.SecurityPolicy) error {
	seen := map[int64]bool{}
	for i, r := range p.Rules {
		if err := normalizeRule("resource.rules["+itoa(i)+"]", p.Type, r); err != nil {
			return err
		}
		if seen[r.Priority] {
			return errInvalid("resource.rules["+itoa(i)+"].priority", r.Priority, "Multiple rules have the same priority.")
		}
		seen[r.Priority] = true
	}
	if !seen[defaultRulePriority] {
		return errInvalid("resource.rules", "", "A security policy must have a default rule at priority 2147483647.")
	}
	sort.Slice(p.Rules, func(i, j int) bool { return p.Rules[i].Priority < p.Rules[j].Priority })
	return nil
}

// ruleActions are the actions a rule may take.
var ruleActions = map[string]bool{
	"allow": true, "deny": true, "deny(403)": true, "deny(404)": true, "deny(502)": true,
	"redirect": true, "throttle": true, "rate_based_ban": true, "fairshare": true,
}

func normalizeRule(f, typ string, r *computev1.SecurityPolicyRule) error {
	r.Kind = "compute#securityPolicyRule"
	if r.Priority < 0 || r.Priority > defaultRulePriority {
		return errInvalid(f+".priority", r.Priority, "Must be between 0 and 2147483647.")
	}
	r.ForceSendFields = append(r.ForceSendFields, "Priority", "Preview")
	if r.Action == "" {
		return errRequired(f + ".action")
	}
	if !ruleActions[r.Action] {
		return errInvalid(f+".action", r.Action, "")
	}
	switch r.Action {
	case "throttle", "rate_based_ban":
		if r.RateLimitOptions == nil {
			return errRequired(f + ".rateLimitOptions")
		}
	case "redirect":
		if r.RedirectOptions == nil {
			return errRequired(f + ".redirectOptions")
		}
	}
	m := r.Match
	if typ == "CLOUD_ARMOR_NETWORK" {
		if r.NetworkMatch == nil && m == nil {
			return errRequired(f + ".networkMatch")
		}
		return nil
	}
	if m == nil {
		return errRequired(f + ".match")
	}
	hasExpr := m.Expr != nil && m.Expr.Expression != ""
	switch {
	case m.VersionedExpr != "":
		if m.VersionedExpr != "SRC_IPS_V1" {
			return errInvalid(f+".match.versionedExpr", m.VersionedExpr, "Must be SRC_IPS_V1.")
		}
		if hasExpr {
			return errInvalid(f+".match", "", "Exactly one of versionedExpr and expr may be specified.")
		}
		if m.Config == nil || len(m.Config.SrcIpRanges) == 0 {
			return errRequired(f + ".match.config.srcIpRanges")
		}
		if len(m.Config.SrcIpRanges) > 10 {
			return errInvalid(f+".match.config.srcIpRanges", len(m.Config.SrcIpRanges), "At most 10 IP ranges can be specified.")
		}
		for j, ip := range m.Config.SrcIpRanges {
			if ip == "*" {
				continue
			}
			if _, _, err := net.ParseCIDR(ip); err != nil && net.ParseIP(ip) == nil {
				return errInvalid(f+".match.config.srcIpRanges["+itoa(j)+"]", ip, "Must be an IP address or range.")
			}
		}
	case hasExpr:
	default:
		return errInvalid(f+".match", "", "One of versionedExpr and expr must be specified.")
	}
	if r.Priority == defaultRulePriority && (m.VersionedExpr == "" || len(m.Config.SrcIpRanges) != 1 || m.Config.SrcIpRanges[0] != "*") {
		return errInvalid(f+".match", "", "The default rule (priority 2147483647) must match all source IPs ('*').")
	}
	return nil
}

func (s *Service) securityPolicyMethods() map[string]http.HandlerFunc {
	k := kindSecurityPolicy
	sp := func(o any) *computev1.SecurityPolicy { return o.(*computev1.SecurityPolicy) }
	return map[string]http.HandlerFunc{
		"addRule": s.mutateH(k, "update", "addRule",
			func() any { return &computev1.SecurityPolicyRule{} },
			func(r *http.Request, sc scope, obj, req any) error {
				rule := req.(*computev1.SecurityPolicyRule)
				for _, cur := range sp(obj).Rules {
					if cur.Priority == rule.Priority {
						return errInvalid("priority", rule.Priority, "A rule with this priority already exists.")
					}
				}
				sp(obj).Rules = append(sp(obj).Rules, rule)
				return nil
			}),
		"patchRule": s.mutateH(k, "update", "patchRule", nil,
			func(r *http.Request, sc scope, obj, _ any) error {
				i, err := ruleIndex(r, sp(obj))
				if err != nil {
					return err
				}
				body, err := readBody(r)
				if err != nil {
					return err
				}
				next := &computev1.SecurityPolicyRule{}
				if err := compute.MergePatch(sp(obj).Rules[i], normalizeBody(body, next), next); err != nil {
					return err
				}
				sp(obj).Rules[i] = next
				return nil
			}),
		"removeRule": s.mutateH(k, "update", "removeRule", nil,
			func(r *http.Request, sc scope, obj, _ any) error {
				i, err := ruleIndex(r, sp(obj))
				if err != nil {
					return err
				}
				if sp(obj).Rules[i].Priority == defaultRulePriority {
					return errInvalid("priority", defaultRulePriority, "The default rule cannot be removed.")
				}
				sp(obj).Rules = append(sp(obj).Rules[:i], sp(obj).Rules[i+1:]...)
				return nil
			}),
		"setLabels": s.setLabelsH(k),
	}
}

// ruleIndex finds the rule named by the priority query parameter (default
// 0, as in GCP).
func ruleIndex(r *http.Request, p *computev1.SecurityPolicy) (int, error) {
	pr := int64(0)
	if v := r.URL.Query().Get("priority"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, errInvalid("priority", v, "")
		}
		pr = n
	}
	for i, rule := range p.Rules {
		if rule.Priority == pr {
			return i, nil
		}
	}
	return 0, apierr.InvalidArgument("Invalid value for field 'priority': '%d'. The rule with the given priority does not exist.", pr).WithLegacy("invalid")
}

// getRule implements securityPolicies.getRule (a GET custom method).
func (s *Service) getRule(w http.ResponseWriter, r *http.Request) {
	_, _, obj, err := s.loadReq(r, kindSecurityPolicy, "get")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	p := obj.(*computev1.SecurityPolicy)
	i, err := ruleIndex(r, p)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p.Rules[i])
}

// refSecurityPolicy canonicalises a backend's security policy reference
// ("" clears it) and checks that the policy has one of the types.
func (s *Service) refSecurityPolicy(sc scope, ref, f string, types ...string) (string, error) {
	if ref == "" {
		return "", nil
	}
	l, _, err := s.refExisting(sc, ref, f, kindSecurityPolicy)
	if err != nil {
		return "", err
	}
	obj, _ := s.load(kindSecurityPolicy, relPath(l))
	typ := obj.(*computev1.SecurityPolicy).Type
	for _, t := range types {
		if t == typ {
			return l, nil
		}
	}
	return "", errInvalid(f, ref, "The security policy type must be "+strings.Join(types, " or ")+".")
}
