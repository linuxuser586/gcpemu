package instance

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/link"
)

// fakeLB stands in for the lb Service: it lists the Service endpoints
// local and tells a front Instance the --lb-bind endpoints lan.
type fakeLB struct {
	env   *emu.Env
	local map[string]string
	lan   []emu.LANFrontendAddrs
}

func (f *fakeLB) Name() string                             { return "lb" }
func (f *fakeLB) Register(emu.Router) error                { return nil }
func (f *fakeLB) Stop(context.Context) error               { return nil }
func (f *fakeLB) Ready() error                             { return nil }
func (f *fakeLB) LANFrontendAddrs() []emu.LANFrontendAddrs { return f.lan }
func (f *fakeLB) Start(context.Context) error {
	for k, v := range f.local {
		f.env.Endpoints.Set(k, v)
	}
	return nil
}

// withFakeLB runs a fakeLB of local and lan as the lb Service.
func withFakeLB(local map[string]string, lan ...emu.LANFrontendAddrs) map[string]Factory {
	return map[string]Factory{"lb": func(env *emu.Env) emu.Service { return &fakeLB{env: env, local: local, lan: lan} }}
}

// enableLB adds the lb Service to change's.
func enableLB(change func(*config.Config)) func(*config.Config) {
	return func(c *config.Config) {
		c.Services = append(c.Services, "lb")
		if change != nil {
			change(c)
		}
	}
}

// envWithNotes is the front Instance's `gcpemu env`, with its notes.
type envWithNotes struct {
	Vars  map[string]string
	Notes []string
}

// lbDelegated pairs a laptop running lb with local endpoints with an
// office, configured with --lb-bind when lbBind, whose lb tells it lan,
// sharing team-shared, which the laptop delegates, and other-own, which it
// does not.
func lbDelegated(t *testing.T, lbBind bool, local map[string]string, lan ...emu.LANFrontendAddrs) (laptop, office *Instance) {
	t.Helper()
	laptop = startWith(t, "laptop", enableLB(nil), withFakeLB(local))
	office = startWith(t, "office", enableLB(func(c *config.Config) {
		if lbBind {
			c.LBBind = []string{"127.0.0.1"}
		}
	}), withFakeLB(nil, lan...))
	pairInstances(t, laptop, office)
	for _, p := range []string{shared, "other-own"} {
		callAdmin(t, office, http.MethodPost, "/_emu/v1/links/shares", map[string]string{"project": p}, nil)
	}
	callAdmin(t, laptop, http.MethodPost, "/_emu/v1/links/delegations", map[string]string{"project": shared, "link": "office"}, nil)
	return laptop, office
}

// The front Instance's `gcpemu env` names each Delegated Project's load
// balancing endpoints at the --lb-bind address its Linked Instance tells a
// LAN caller arriving on the --link-bind address, deriving the variables
// over every Instance's endpoints together, and says on stderr why one
// gets none (#715).
func TestFrontEnvNamesLinkedLBBind(t *testing.T) {
	laptop, _ := lbDelegated(t, true,
		map[string]string{"lb:laptop-own/global/web": "127.0.0.5:8080", "gke:dev/default/web/80": "127.0.0.6:80"},
		emu.LANFrontendAddrs{Name: "lb:" + shared + "/global/web", Project: shared, What: "forwarding rule projects/" + shared + "/global/forwardingRules/web",
			Addrs: []string{"192.0.2.1:8080", "127.0.0.1:8080"}},
		emu.LANFrontendAddrs{Name: "lb:" + shared + "/global/api", Project: shared, What: "forwarding rule projects/" + shared + "/global/forwardingRules/api",
			Error: "192.0.2.1:8081: already serves forwarding rule other"},
		emu.LANFrontendAddrs{Name: "lb:" + shared + "/us-central1/edge", Project: shared, What: "forwarding rule projects/" + shared + "/regions/us-central1/forwardingRules/edge",
			Addrs: []string{"127.0.0.1:443"}},
		emu.LANFrontendAddrs{Name: "gke:dev/default/web/80", Project: shared, What: "LoadBalancer Service " + shared + "/us-central1/dev/default/web port 80",
			Addrs: []string{"127.0.0.1:80"}},
		emu.LANFrontendAddrs{Name: "lb:other-own/global/secret", Project: "other-own", What: "forwarding rule projects/other-own/global/forwardingRules/secret",
			Addrs: []string{"127.0.0.1:9000"}},
	)
	var env envWithNotes
	callAdmin(t, laptop, http.MethodGet, "/_emu/v1/env?notes=true", nil, &env)
	want := map[string]string{
		"GCPEMU_LB_TEAM_SHARED_GLOBAL_WEB":       "127.0.0.1:8080",
		"GCPEMU_LB_LAPTOP_OWN_GLOBAL_WEB":        "127.0.0.5:8080",
		"GCPEMU_LB_TEAM_SHARED_US_CENTRAL1_EDGE": "127.0.0.1:443",
		"GCPEMU_LB_EDGE":                         "127.0.0.1:443",
	}
	for k, v := range want {
		if env.Vars[k] != v {
			t.Errorf("%s = %q, want %q", k, env.Vars[k], v)
		}
	}
	// Withheld: a name on both Instances, a cluster name on both, and an
	// endpoint with no --lb-bind address; and never a Project not
	// delegated to the laptop.
	for _, k := range []string{"GCPEMU_LB_WEB", "GCPEMU_GKE_DEV_DEFAULT_WEB_80", "GCPEMU_LB_TEAM_SHARED_GLOBAL_API", "GCPEMU_LB_API",
		"GCPEMU_LB_OTHER_OWN_GLOBAL_SECRET", "GCPEMU_LB_SECRET"} {
		if v, ok := env.Vars[k]; ok {
			t.Errorf("%s = %q, want it withheld", k, v)
		}
	}
	if !slices.ContainsFunc(env.Notes, func(n string) bool {
		return strings.Contains(n, "forwardingRules/api") && strings.Contains(n, `link "office"`) && strings.Contains(n, "already serves")
	}) {
		t.Errorf("notes = %q, want one naming api, the link and why", env.Notes)
	}
	if !slices.ContainsFunc(env.Notes, func(n string) bool {
		return strings.Contains(n, "GCPEMU_LB_WEB") && strings.Contains(n, "lb:laptop-own/global/web") && strings.Contains(n, `link "office"`)
	}) {
		t.Errorf("notes = %q, want GCPEMU_LB_WEB named withheld, with both rules", env.Notes)
	}
	if slices.ContainsFunc(env.Notes, func(n string) bool { return strings.Contains(n, "no --lb-bind") }) {
		t.Errorf("notes = %q, want none saying the office has no --lb-bind", env.Notes)
	}
	if got := laptop.EnvVars()["GCPEMU_LB_TEAM_SHARED_GLOBAL_WEB"]; got != "127.0.0.1:8080" {
		t.Errorf("EnvVars() names the Delegated Project's rule %q", got)
	}

	var st struct{ Links []link.LinkFrontends }
	callAdmin(t, laptop, http.MethodGet, "/_emu/v1/links/lb-frontends", nil, &st)
	if len(st.Links) != 1 || st.Links[0].Link != "office" || st.Links[0].Error != "" {
		t.Fatalf("linked endpoints = %+v, want the office's", st.Links)
	}
	var names []string
	for _, e := range st.Links[0].Frontends {
		names = append(names, e.Name)
		if e.Name == "lb:"+shared+"/global/web" && e.Address != "127.0.0.1:8080" {
			t.Errorf("web at %q, want 127.0.0.1:8080", e.Address)
		}
	}
	if slices.Contains(names, "lb:other-own/global/secret") || !slices.Contains(names, "lb:"+shared+"/global/api") {
		t.Errorf("linked endpoints %v, want team-shared's only, api included", names)
	}
}

// An unreachable Linked Instance leaves its variables out and is named on
// stderr; `gcpemu env` still answers (#715).
func TestFrontEnvWithLinkedInstanceDown(t *testing.T) {
	laptop, office := lbDelegated(t, false, map[string]string{"lb:laptop-own/global/web": "127.0.0.5:8080", "gke:dev/default/web/80": "127.0.0.6:80"},
		emu.LANFrontendAddrs{Name: "lb:" + shared + "/global/api", Project: shared, What: "forwarding rule api", Addrs: []string{"127.0.0.1:8081"}})
	if err := office.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	var env envWithNotes
	callAdmin(t, laptop, http.MethodGet, "/_emu/v1/env?notes=true", nil, &env)
	if env.Vars["GCPEMU_LB_LAPTOP_OWN_GLOBAL_WEB"] != "127.0.0.5:8080" || env.Vars["GCPEMU_GATEWAY"] == "" {
		t.Errorf("vars = %v, want the laptop's own", env.Vars)
	}
	if v, ok := env.Vars["GCPEMU_LB_TEAM_SHARED_GLOBAL_API"]; ok {
		t.Errorf("GCPEMU_LB_TEAM_SHARED_GLOBAL_API = %q with the office down", v)
	}
	if !slices.ContainsFunc(env.Notes, func(n string) bool { return strings.Contains(n, `link "office"`) && strings.Contains(n, "unreachable") }) {
		t.Errorf("notes = %q, want the office named unreachable", env.Notes)
	}
	// The office may have a rule web too, so the short variable is
	// withheld rather than pointed at the laptop's while it cannot be
	// asked; a Service's only variable stays, with a warning.
	if v, ok := env.Vars["GCPEMU_LB_WEB"]; ok {
		t.Errorf("GCPEMU_LB_WEB = %q with the office unreachable, want it withheld", v)
	}
	if env.Vars["GCPEMU_GKE_DEV_DEFAULT_WEB_80"] != "127.0.0.6:80" {
		t.Errorf("GCPEMU_GKE_DEV_DEFAULT_WEB_80 = %q, want the laptop's Service", env.Vars["GCPEMU_GKE_DEV_DEFAULT_WEB_80"])
	}
	if !slices.ContainsFunc(env.Notes, func(n string) bool {
		return strings.Contains(n, "GCPEMU_LB_<NAME>") && strings.Contains(n, "GCPEMU_GKE_")
	}) {
		t.Errorf("notes = %q, want the short variables named withheld and GCPEMU_GKE_* warned of", env.Notes)
	}
	var st struct{ Links []link.LinkFrontends }
	callAdmin(t, laptop, http.MethodGet, "/_emu/v1/links/lb-frontends", nil, &st)
	if len(st.Links) != 1 || st.Links[0].Error == "" {
		t.Errorf("linked endpoints = %+v, want the office unreachable", st.Links)
	}
}

// A Linked Instance without --lb-bind is named on stderr, so that its
// LoadBalancer Services, which it does not list without --lb-bind, are
// accounted for too (#715).
func TestFrontEnvWithoutLinkedLBBind(t *testing.T) {
	laptop, _ := lbDelegated(t, false, nil)
	var env envWithNotes
	callAdmin(t, laptop, http.MethodGet, "/_emu/v1/env?notes=true", nil, &env)
	if !slices.ContainsFunc(env.Notes, func(n string) bool {
		return strings.Contains(n, `link "office" has no --lb-bind`) && strings.Contains(n, "LoadBalancer Services")
	}) {
		t.Errorf("notes = %q, want the office named, without --lb-bind", env.Notes)
	}
}

// A Linked Instance with --lb-bind but no lb Service serves nothing there,
// and is named as having no --lb-bind (#715).
func TestFrontEnvWithoutLinkedLBService(t *testing.T) {
	laptop := startWith(t, "laptop", enableLB(nil), withFakeLB(nil))
	office := startWithProjects(t, "office", func(c *config.Config) { c.LBBind = []string{"127.0.0.1"} })
	pairInstances(t, laptop, office)
	callAdmin(t, office, http.MethodPost, "/_emu/v1/links/shares", map[string]string{"project": shared}, nil)
	callAdmin(t, laptop, http.MethodPost, "/_emu/v1/links/delegations", map[string]string{"project": shared, "link": "office"}, nil)
	var env envWithNotes
	callAdmin(t, laptop, http.MethodGet, "/_emu/v1/env?notes=true", nil, &env)
	if !slices.ContainsFunc(env.Notes, func(n string) bool { return strings.Contains(n, `link "office" has no --lb-bind`) }) {
		t.Errorf("notes = %q, want the office named, without --lb-bind", env.Notes)
	}
}
