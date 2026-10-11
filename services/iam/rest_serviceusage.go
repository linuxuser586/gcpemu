package iam

import (
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/project"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// nsServices records a project's services that were enabled or disabled
// through Service Usage: "P/NAME" → serviceRecord.
const nsServices = "iam/services"

// defaultServices are enabled in every project until disabled: the APIs
// the emulator serves.
var defaultServices = []string{
	"artifactregistry.googleapis.com", "certificatemanager.googleapis.com",
	"cloudbilling.googleapis.com", "cloudresourcemanager.googleapis.com",
	"compute.googleapis.com", "connectgateway.googleapis.com",
	"container.googleapis.com", "dns.googleapis.com", "iam.googleapis.com",
	"iamcredentials.googleapis.com", "networksecurity.googleapis.com",
	"pubsub.googleapis.com", "secretmanager.googleapis.com",
	"servicenetworking.googleapis.com", "serviceusage.googleapis.com",
	"sqladmin.googleapis.com", "storage.googleapis.com", "sts.googleapis.com",
}

var serviceNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?\.[a-z]{2,}$`)

const (
	stateEnabled  = "ENABLED"
	stateDisabled = "DISABLED"
)

type serviceRecord struct {
	State string `json:"state"`
}

// serviceJSON is google.api.serviceusage.v1.Service.
type serviceJSON struct {
	Name   string         `json:"name"`
	Parent string         `json:"parent"`
	Config map[string]any `json:"config"`
	State  string         `json:"state"`
}

// serveServiceUsage serves Service Usage v1: services get, list, enable,
// disable and batchEnable, and operations.get. Every change completes
// immediately; the emulator does not refuse calls to a disabled API.
// google_project_service lists the enabled services, batch-enables the
// missing ones and disables on destroy; gcloud checks an API is enabled
// before `gcloud container` calls.
func (s *Service) serveServiceUsage(w http.ResponseWriter, r *http.Request) {
	if err := s.routeServiceUsage(w, r, parseRoute(r.URL.Path)); err != nil {
		apierr.Write(w, err)
	}
}

func (s *Service) routeServiceUsage(w http.ResponseWriter, r *http.Request, rt route) error {
	if c, ok := rt.match("", "v1", "operations", "*"); ok && r.Method == http.MethodGet {
		writeJSON(w, map[string]any{"name": "operations/" + c[0], "done": true})
		return nil
	}
	var pid, name string
	c, ok := rt.match(rt.verb, "v1", "projects", "*", "services", "*")
	if ok {
		name = c[1]
	} else if c, ok = rt.match(rt.verb, "v1", "projects", "*", "services"); !ok {
		notFoundRoute(w, r)
		return nil
	}
	_ = s.env.Store.View(func(tx store.Tx) error { pid = s.projectID(tx, c[0]); return nil })
	if err := s.env.EnsureProject(pid); err != nil {
		return err
	}
	res, ctx := projectResource(pid), r.Context()
	switch {
	case name == "" && rt.verb == "" && r.Method == http.MethodGet:
		if err := s.check(ctx, "serviceusage.services.list", res); err != nil {
			return err
		}
		return s.listServices(w, r, pid)
	case name == "" && rt.verb == "batchEnable" && r.Method == http.MethodPost:
		var req struct {
			ServiceIDs []string `json:"serviceIds"`
		}
		if err := readJSON(r, &req); err != nil {
			return err
		}
		if len(req.ServiceIDs) == 0 || len(req.ServiceIDs) > 20 {
			return apierr.InvalidArgument("serviceIds must name between 1 and 20 services.")
		}
		if err := s.check(ctx, "serviceusage.services.enable", res); err != nil {
			return err
		}
		out, err := s.setServiceState(pid, req.ServiceIDs, stateEnabled)
		if err != nil {
			return err
		}
		writeJSON(w, doneOperation("BatchEnableServicesResponse", "services", out))
		return nil
	case name != "" && rt.verb == "" && r.Method == http.MethodGet:
		if !serviceNameRE.MatchString(name) {
			return serviceNotFound(name)
		}
		if err := s.check(ctx, "serviceusage.services.get", res); err != nil {
			return err
		}
		var st string
		_ = s.env.Store.View(func(tx store.Tx) error { st = serviceState(tx, pid, name); return nil })
		writeJSON(w, serviceOf(pid, name, st))
		return nil
	case name != "" && (rt.verb == "enable" || rt.verb == "disable") && r.Method == http.MethodPost:
		state, resp := stateEnabled, "EnableServiceResponse"
		if rt.verb == "disable" {
			state, resp = stateDisabled, "DisableServiceResponse"
		}
		if err := s.check(ctx, "serviceusage.services."+rt.verb, res); err != nil {
			return err
		}
		out, err := s.setServiceState(pid, []string{name}, state)
		if err != nil {
			return err
		}
		writeJSON(w, doneOperation(resp, "service", out[0]))
		return nil
	}
	notFoundRoute(w, r)
	return nil
}

// listServices lists the default services and those enabled or disabled
// since, filtered by "state:ENABLED" or "state:DISABLED".
func (s *Service) listServices(w http.ResponseWriter, r *http.Request, pid string) error {
	want := ""
	switch f := strings.ReplaceAll(r.URL.Query().Get("filter"), " ", ""); f {
	case "":
	case "state:ENABLED", "state:DISABLED":
		want = strings.TrimPrefix(f, "state:")
	default:
		return apierr.InvalidArgument("Invalid filter %q.", f)
	}
	size, offset, err := pageParams(r, 50, 200)
	if err != nil {
		return err
	}
	states := map[string]string{}
	for _, n := range defaultServices {
		states[n] = stateEnabled
	}
	_ = s.env.Store.View(func(tx store.Tx) error {
		tx.Scan(nsServices, pid+"/", func(k string, _ []byte) bool {
			n := strings.TrimPrefix(k, pid+"/")
			states[n] = serviceState(tx, pid, n)
			return true
		})
		return nil
	})
	var all []serviceJSON
	for _, n := range slices.Sorted(maps.Keys(states)) {
		if want == "" || states[n] == want {
			all = append(all, serviceOf(pid, n, states[n]))
		}
	}
	items, next := page(all, size, offset)
	out := map[string]any{"services": items}
	if next != "" {
		out["nextPageToken"] = next
	}
	writeJSON(w, out)
	return nil
}

// setServiceState records names as state and returns them.
func (s *Service) setServiceState(pid string, names []string, state string) ([]serviceJSON, error) {
	for _, n := range names {
		if !serviceNameRE.MatchString(n) {
			return nil, serviceNotFound(n)
		}
	}
	err := s.env.Store.Update(func(tx store.Tx) error {
		for _, n := range names {
			if err := store.PutJSON(tx, nsServices, pid+"/"+n, serviceRecord{State: state}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]serviceJSON, len(names))
	for i, n := range names {
		out[i] = serviceOf(pid, n, state)
	}
	return out, nil
}

// serviceState is a service's state in a project: as last set, else
// ENABLED for a default service and DISABLED for any other.
func serviceState(tx store.Tx, pid, name string) string {
	var rec serviceRecord
	if store.GetJSON(tx, nsServices, pid+"/"+name, &rec) == nil {
		return rec.State
	}
	if slices.Contains(defaultServices, name) {
		return stateEnabled
	}
	return stateDisabled
}

// serviceOf names the service by project number, as GCP does.
func serviceOf(pid, name, state string) serviceJSON {
	parent := "projects/" + project.NumberString(pid)
	return serviceJSON{
		Name:   parent + "/services/" + name,
		Parent: parent,
		Config: map[string]any{"name": name},
		State:  state,
	}
}

// doneOperation is a completed long-running operation with a typed
// response carrying v under field.
func doneOperation(typ, field string, v any) map[string]any {
	return map[string]any{
		"name": "operations/noop.DONE_OPERATION",
		"done": true,
		"response": map[string]any{
			"@type": "type.googleapis.com/google.api.serviceusage.v1." + typ,
			field:   v,
		},
	}
}

// serviceNotFound is GCP's error for a name that is not a service.
func serviceNotFound(name string) error {
	return apierr.PermissionDenied("Not found or permission denied for service(s): %s.", name)
}
