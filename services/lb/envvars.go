package lb

import (
	"strings"

	"github.com/linuxuser586/gcpemu/internal/lbenv"
)

// Forwarding rule env vars (ADR 0013, #314). Each rule's Service endpoint
// is named "lb:<project>/<region|global>/<name>", which depends only on
// the rule.
// `gcpemu env` prints it as GCPEMU_LB_<PROJECT>_<REGION|GLOBAL>_<NAME>,
// and as GCPEMU_LB_<NAME> while the name is unique across all projects
// and scopes. A variable more than one rule would print (a shared name,
// or two qualified keys that flatten alike) is withheld, never pointed at
// either rule. The variables are derived by package lbenv, never stored;
// the Instance derives the ones `gcpemu env` prints, and this Service
// reports the ones withheld.

// endpointKey is the name of a frontend's rule's Service endpoint.
func endpointKey(fe *frontend) string {
	sc := fe.region
	if sc == "" {
		sc = "global"
	}
	return "lb:" + fe.project + "/" + sc + "/" + fe.fr.Name
}

// withheldVar is a GCPEMU_LB_* variable that more than one rule would
// print, with the Service endpoint names of those rules in order.
type withheldVar struct {
	name string
	keys []string
}

// lbWithheld returns the GCPEMU_LB_* variables of the rules among the lb:
// entries of endpoints withheld as ambiguous (package lbenv), sorted by
// variable.
func lbWithheld(endpoints map[string]string) []withheldVar {
	rules := map[string]string{}
	for k, v := range endpoints {
		if strings.HasPrefix(k, lbenv.RulePrefix) {
			rules[k] = v
		}
	}
	_, ws := lbenv.Vars(rules)
	withheld := make([]withheldVar, len(ws))
	for i, w := range ws {
		withheld[i] = withheldVar{name: w.Name, keys: w.Keys}
	}
	return withheld
}

// info describes w with its rules and their qualified variables.
func (w withheldVar) info() withheldInfo {
	out := withheldInfo{Variable: w.name}
	for _, k := range w.keys {
		out.ForwardingRules = append(out.ForwardingRules, rulePath(k))
		out.Qualified = append(out.Qualified, lbenv.Qualified(k))
	}
	return out
}

// rulePath is the forwarding rule resource path of a Service endpoint
// name.
func rulePath(key string) string {
	project, rest, _ := strings.Cut(strings.TrimPrefix(key, "lb:"), "/")
	region, name, _ := strings.Cut(rest, "/")
	if region == "global" {
		region = ""
	}
	return scope{project: project, region: region}.coll("forwardingRules") + "/" + name
}

// warnWithheld logs each variable newly withheld since the last call.
// Callers hold d.mu.
func (d *dataplane) warnWithheld() {
	withheld := lbWithheld(d.s.env.Endpoints.All())
	now := map[string]bool{}
	for _, w := range withheld {
		now[w.name] = true
		if d.withheld[w.name] {
			continue
		}
		info := w.info()
		d.s.env.Log.Warn("lb: forwarding rules share an env var, which `gcpemu env` does not print",
			"variable", w.name, "forwardingRules", info.ForwardingRules, "qualified", info.Qualified)
	}
	d.withheld = now
}
