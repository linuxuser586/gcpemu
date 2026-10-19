package lb

import (
	"sort"
	"strings"
)

// Forwarding rule env vars (ADR 0013, #314). Each rule's Service endpoint
// is named "lb:<project>/<region|global>/<name>", which depends only on
// the rule.
// `gcpemu env` prints it as GCPEMU_LB_<PROJECT>_<REGION|GLOBAL>_<NAME>,
// and as GCPEMU_LB_<NAME> while the name is unique across all projects
// and scopes. A variable more than one rule would print (a shared name,
// or two qualified keys that flatten alike) is withheld, never pointed at
// either rule. The short variable is derived here, never stored.

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

// lbEnv derives the GCPEMU_LB_* variables from the lb: entries of
// endpoints, and the ones withheld as ambiguous, sorted by variable.
func lbEnv(endpoints map[string]string) (map[string]string, []withheldVar) {
	keysOf := map[string][]string{} // variable → the keys that print it
	for k := range endpoints {
		if !strings.HasPrefix(k, "lb:") {
			continue
		}
		vars := []string{qualifiedVar(k)}
		if i := strings.LastIndex(k, "/"); i >= 0 {
			vars = append(vars, "GCPEMU_LB_"+envName(k[i+1:]))
		}
		for _, v := range vars {
			keysOf[v] = append(keysOf[v], k)
		}
	}
	out := map[string]string{}
	var withheld []withheldVar
	for v, keys := range keysOf {
		if len(keys) == 1 {
			out[v] = endpoints[keys[0]]
			continue
		}
		sort.Strings(keys)
		withheld = append(withheld, withheldVar{name: v, keys: keys})
	}
	sort.Slice(withheld, func(i, j int) bool { return withheld[i].name < withheld[j].name })
	return out, withheld
}

// info describes w with its rules and their qualified variables.
func (w withheldVar) info() withheldInfo {
	out := withheldInfo{Variable: w.name}
	for _, k := range w.keys {
		out.ForwardingRules = append(out.ForwardingRules, rulePath(k))
		out.Qualified = append(out.Qualified, qualifiedVar(k))
	}
	return out
}

// qualifiedVar is GCPEMU_LB_<PROJECT>_<REGION|GLOBAL>_<NAME> for a Service
// endpoint name.
func qualifiedVar(key string) string { return "GCPEMU_LB_" + envName(strings.TrimPrefix(key, "lb:")) }

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

// envName upper-cases a forwarding rule name or Service endpoint name
// for an env var.
func envName(s string) string {
	var b strings.Builder
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z':
			b.WriteRune(c - 32)
		case c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b.WriteRune(c)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// warnWithheld logs each variable newly withheld since the last call.
// Callers hold d.mu.
func (d *dataplane) warnWithheld() {
	_, withheld := lbEnv(d.s.env.Endpoints.All())
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
