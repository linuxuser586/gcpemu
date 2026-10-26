// Package lbenv derives the `gcpemu env` variables of load balancing
// endpoints (ADR 0013): forwarding rules' GCPEMU_LB_* and GKE
// LoadBalancer Service ports' GCPEMU_GKE_*, from their Service endpoint
// names. A front Instance derives them over its own endpoints and its
// Linked Instances' together (ADR 0027, #715), so that uniqueness and
// withholding span Instances.
//
// A forwarding rule's endpoint is "lb:<project>/<region|global>/<name>",
// printed as GCPEMU_LB_<PROJECT>_<REGION|GLOBAL>_<NAME>, and as
// GCPEMU_LB_<NAME> while the name is unique. A Service port's is
// "gke:<cluster>/<namespace>/<service>/<port>", printed as
// GCPEMU_GKE_<CLUSTER>_<NAMESPACE>_<SERVICE>_<PORT>. A variable more than
// one endpoint would print (a shared name, a cluster name used on two
// Instances, or two names that flatten alike) is withheld, never pointed
// at either.
package lbenv

import (
	"sort"
	"strings"
)

// Prefixes of the endpoint names that print variables.
const (
	RulePrefix    = "lb:"
	ServicePrefix = "gke:"
)

// linkSep separates a Linked Instance's endpoint name from its link, a
// character no rule, cluster or Service name has.
const linkSep = "@"

// Linked is the name under which a front Instance lists the endpoint name
// of the Linked Instance link, beside its own.
func Linked(name, link string) string { return name + linkSep + link }

// Unlinked returns the endpoint name and link of a name Linked made, and
// a name of this Instance's own with link "".
func Unlinked(key string) (name, link string) {
	name, link, _ = strings.Cut(key, linkSep)
	return name, link
}

// Withheld is a variable more than one endpoint would print, with their
// names (Keys, as in the endpoints given) in order.
type Withheld struct {
	Name string
	Keys []string
}

// Vars returns the variables of the load balancing endpoints in
// endpoints, and those withheld as ambiguous, sorted by variable. Other
// endpoints are ignored.
func Vars(endpoints map[string]string) (map[string]string, []Withheld) {
	keysOf := map[string][]string{} // variable → the keys that print it
	for k := range endpoints {
		for _, v := range varsOf(k) {
			keysOf[v] = append(keysOf[v], k)
		}
	}
	out := map[string]string{}
	var withheld []Withheld
	for v, keys := range keysOf {
		if len(keys) == 1 {
			out[v] = endpoints[keys[0]]
			continue
		}
		sort.Strings(keys)
		withheld = append(withheld, Withheld{Name: v, Keys: keys})
	}
	sort.Slice(withheld, func(i, j int) bool { return withheld[i].Name < withheld[j].Name })
	return out, withheld
}

// varsOf returns the variables the endpoint key would print.
func varsOf(key string) []string {
	name, _ := Unlinked(key)
	if rest, ok := strings.CutPrefix(name, ServicePrefix); ok {
		return []string{"GCPEMU_GKE_" + EnvName(rest)}
	}
	if !strings.HasPrefix(name, RulePrefix) {
		return nil
	}
	vars := []string{Qualified(name)}
	if i := strings.LastIndex(name, "/"); i >= 0 {
		vars = append(vars, "GCPEMU_LB_"+EnvName(name[i+1:]))
	}
	return vars
}

// Qualified is GCPEMU_LB_<PROJECT>_<REGION|GLOBAL>_<NAME> for a forwarding
// rule's endpoint name.
func Qualified(key string) string {
	name, _ := Unlinked(key)
	return "GCPEMU_LB_" + EnvName(strings.TrimPrefix(name, RulePrefix))
}

// EnvName upper-cases a name for an env var, replacing other characters
// with '_'.
func EnvName(s string) string {
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
