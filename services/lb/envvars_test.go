package lb_test

import (
	"net/http"
	"strings"
	"testing"

	computev1 "google.golang.org/api/compute/v1"
)

// rule creates forwarding rule name on port 8080 in project, global when
// region is "", with a backend service, URL map and target HTTP proxy of
// its own, and returns its listener endpoint.
func (e *env) rule(project, region, name string) string {
	e.t.Helper()
	c := e.c
	bs := &computev1.BackendService{Name: name, LoadBalancingScheme: "EXTERNAL_MANAGED", Protocol: "HTTP"}
	fr := &computev1.ForwardingRule{Name: name, LoadBalancingScheme: "EXTERNAL_MANAGED", PortRange: "8080"}
	scope := "global"
	if region == "" {
		e.do(c.BackendServices.Insert(project, bs).Do())
		e.do(c.UrlMaps.Insert(project, &computev1.UrlMap{Name: name, DefaultService: "global/backendServices/" + name}).Do())
		e.do(c.TargetHttpProxies.Insert(project, &computev1.TargetHttpProxy{Name: name, UrlMap: "global/urlMaps/" + name}).Do())
		fr.Target = "global/targetHttpProxies/" + name
		e.do(c.GlobalForwardingRules.Insert(project, fr).Do())
	} else {
		scope = region
		e.do(c.RegionBackendServices.Insert(project, region, bs).Do())
		e.do(c.RegionUrlMaps.Insert(project, region, &computev1.UrlMap{Name: name, DefaultService: "regions/" + region + "/backendServices/" + name}).Do())
		e.do(c.RegionTargetHttpProxies.Insert(project, region, &computev1.TargetHttpProxy{Name: name, UrlMap: "regions/" + region + "/urlMaps/" + name}).Do())
		fr.Target = "regions/" + region + "/targetHttpProxies/" + name
		e.do(c.ForwardingRules.Insert(project, region, fr).Do())
	}
	addr := e.inst.Endpoint("lb:" + project + "/" + scope + "/" + name)
	if addr == "" {
		e.t.Fatalf("forwarding rule %s/%s/%s has no endpoint", project, scope, name)
	}
	return addr
}

// lbVars returns the GCPEMU_LB_* variables `gcpemu env` prints.
func (e *env) lbVars() map[string]string {
	out := map[string]string{}
	for k, v := range e.inst.EnvVars() {
		if strings.HasPrefix(k, "GCPEMU_LB_") {
			out[k] = v
		}
	}
	return out
}

// One forwarding rule has a qualified variable and, its name being
// unique, the short one, with the same address (ADR 0013).
func TestEnvVarsQualifiedAndShort(t *testing.T) {
	e := start(t)
	addr := e.rule(proj, "", "web")
	got := e.lbVars()
	if got["GCPEMU_LB_LB_TEST_GLOBAL_WEB"] != addr || got["GCPEMU_LB_WEB"] != addr || len(got) != 2 {
		t.Errorf("GCPEMU_LB_* = %v, want GCPEMU_LB_LB_TEST_GLOBAL_WEB and GCPEMU_LB_WEB = %s", got, addr)
	}
}

// A global and a regional rule, and rules in two projects, sharing a name
// each keep their own qualified variable, and the short one is printed
// for neither; deleting one brings it back for the other (#314).
func TestEnvVarsSharedName(t *testing.T) {
	e := start(t)
	global := e.rule(proj, "", "web")
	regional := e.rule(proj, "us-central1", "web")
	other := e.rule("lb-other", "", "web")
	if global == regional || global == other {
		t.Fatalf("rules share a listener: %s, %s, %s", global, regional, other)
	}
	want := map[string]string{
		"GCPEMU_LB_LB_TEST_GLOBAL_WEB":      global,
		"GCPEMU_LB_LB_TEST_US_CENTRAL1_WEB": regional,
		"GCPEMU_LB_LB_OTHER_GLOBAL_WEB":     other,
	}
	if got := e.lbVars(); !sameVars(got, want) {
		t.Errorf("GCPEMU_LB_* = %v, want %v", got, want)
	}

	e.do(e.c.ForwardingRules.Delete(proj, "us-central1", "web").Do())
	e.do(e.c.GlobalForwardingRules.Delete("lb-other", "web").Do())
	want = map[string]string{"GCPEMU_LB_LB_TEST_GLOBAL_WEB": global, "GCPEMU_LB_WEB": global}
	if got := e.lbVars(); !sameVars(got, want) {
		t.Errorf("after deleting the others, GCPEMU_LB_* = %v, want %v", got, want)
	}
}

// Two rules whose qualified keys flatten to one variable publish neither
// as it, and the admin API lists every withheld variable with its rules.
func TestEnvVarsFlattenedCollision(t *testing.T) {
	e := start(t)
	x := e.rule("proj-a", "", "global-x")
	y := e.rule("proj-a-global", "", "x")
	want := map[string]string{"GCPEMU_LB_GLOBAL_X": x, "GCPEMU_LB_X": y}
	if got := e.lbVars(); !sameVars(got, want) {
		t.Errorf("GCPEMU_LB_* = %v, want %v", got, want)
	}
	withheld := e.rest(http.MethodGet, "/_emu/v1/lb/listeners", nil)["withheld"].([]any)
	if len(withheld) != 1 {
		t.Fatalf("withheld = %v, want one variable", withheld)
	}
	w := withheld[0].(map[string]any)
	rules := w["forwardingRules"].([]any)
	if w["variable"] != "GCPEMU_LB_PROJ_A_GLOBAL_GLOBAL_X" || len(rules) != 2 ||
		rules[0] != "projects/proj-a-global/global/forwardingRules/x" || rules[1] != "projects/proj-a/global/forwardingRules/global-x" {
		t.Errorf("withheld = %v", w)
	}
}

func sameVars(got, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}
