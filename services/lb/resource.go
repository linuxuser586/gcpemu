package lb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"strings"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/locations"
	"github.com/linuxuser586/gcpemu/internal/store"
	"github.com/linuxuser586/gcpemu/services/compute"
)

// Generic handling of the load-balancing collections (FR-LB-001). Every
// collection is a compute v1 resource stored as its JSON form under
// "lb/<collection>" keyed by its relative path; global and regional
// variants share a kind and differ only in scope and IAM permission
// prefix. Mutations are compute Operations (FR-CORE-023).

// scope is a project plus "global" (region == "") or a region.
type scope struct{ project, region string }

// String renders the operation scope ("global" or "regions/R").
func (sc scope) String() string {
	if sc.region == "" {
		return "global"
	}
	return "regions/" + sc.region
}

// coll returns the collection path for a URL collection segment.
func (sc scope) coll(c string) string {
	if sc.region == "" {
		return "projects/" + sc.project + "/global/" + c
	}
	return "projects/" + sc.project + "/regions/" + sc.region + "/" + c
}

// regionLink is the region selfLink of a regional scope.
func (sc scope) regionLink() string {
	if sc.region == "" {
		return ""
	}
	return compute.SelfLink("projects/" + sc.project + "/regions/" + sc.region)
}

// scopeOfPath derives the scope of "projects/P/{global|regions/R}/...".
func scopeOfPath(path string) scope {
	segs := strings.Split(path, "/")
	sc := scope{}
	if len(segs) > 1 {
		sc.project = segs[1]
	}
	if len(segs) > 3 && segs[2] == "regions" {
		sc.region = segs[3]
	}
	return sc
}

// kind describes one collection.
type kind struct {
	coll  string // URL collection segment, e.g. "urlMaps"
	typ   string // "compute#urlMap"
	snake string // name in resource-in-use errors, e.g. "url_map"
	// gperm and rperm are the IAM permission prefixes of the global and
	// regional variants ("" when the variant does not exist).
	gperm, rperm string
	// noPatch / noUpdate disable PATCH / PUT.
	noPatch, noUpdate bool
	// listKind and aggKind override the derived list kinds.
	listKind, aggKind string
	newObj            func() any
	// prepare validates and canonicalises obj; old is nil on insert.
	prepare func(ctx context.Context, s *Service, sc scope, path string, obj, old any) error
	// refs lists the resource paths obj references (FR-CORE-026).
	refs func(obj any) []string
	// afterSave / afterDelete run after the store commit.
	afterSave   func(ctx context.Context, s *Service, path string, obj, old any)
	afterDelete func(ctx context.Context, s *Service, path string, obj any)
	// inTx runs inside the store transaction that saves obj, after the
	// common fields are set (secrets are moved out of the resource here).
	inTx func(tx store.Tx, path string, obj any) error
	// keep lists extra output-only fields preserved on PATCH/PUT.
	keep []string
}

func (k *kind) ns() string { return "lb/" + k.coll }

func (k *kind) perm(sc scope, verb string) string {
	if sc.region == "" {
		return k.gperm + "." + verb
	}
	return k.rperm + "." + verb
}

func (k *kind) listKindName() string {
	if k.listKind != "" {
		return k.listKind
	}
	return k.typ + "List"
}

func (k *kind) aggKindName() string {
	if k.aggKind != "" {
		return k.aggKind
	}
	return k.typ + "AggregatedList"
}

// --- errors (compute's shapes) ---

func errNotFound(path string) error {
	return apierr.NotFound("The resource '%s' was not found", path).WithLegacy("notFound")
}

func errExists(path string) error {
	return apierr.AlreadyExists("The resource '%s' already exists", path).WithLegacy("alreadyExists")
}

func errInUse(snake, path, user string) error {
	return apierr.FailedPrecondition("The %s resource '%s' is already being used by '%s'", snake, path, user).
		WithLegacy("resourceInUseByAnotherResource")
}

func errInvalid(field string, value any, why string) error {
	msg := fmt.Sprintf("Invalid value for field '%s': '%v'.", field, value)
	if why != "" {
		msg += " " + why
	}
	return apierr.InvalidArgument("%s", msg).WithLegacy("invalid")
}

func errRequired(field string) error {
	return apierr.InvalidArgument("Required field '%s' not specified", field).WithLegacy("required")
}

func errFingerprint() error {
	return apierr.FailedPrecondition("Supplied fingerprint does not match current fingerprint.").
		WithLegacy("conditionNotMet").WithHTTP(http.StatusPreconditionFailed)
}

var nameRE = regexp.MustCompile(`^[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?$`)

func validName(field, name string) error {
	if !nameRE.MatchString(name) {
		return apierr.InvalidArgument("Invalid value for field '%s': '%s'. Must be a match of regex '(?:[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?)'", field, name).WithLegacy("invalid")
	}
	return nil
}

// --- JSON helpers ---

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func reply(w http.ResponseWriter, v any, err error) {
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func readBody(r *http.Request) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return nil, apierr.InvalidArgument("Failed to read request body: %v", err).WithLegacy("parseError")
	}
	return b, nil
}

// decode reads a required JSON body into v.
func decode(r *http.Request, v any) ([]byte, error) {
	b, err := readBody(r)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, apierr.InvalidArgument("Required request body is missing.").WithLegacy("required")
	}
	if err := unmarshalLenient(b, v); err != nil {
		return nil, apierr.InvalidArgument("Invalid JSON payload received. %v", err).WithLegacy("parseError")
	}
	return b, nil
}

// clone deep-copies a resource through JSON.
func clone(k *kind, v any) any {
	b, _ := json.Marshal(v)
	out := k.newObj()
	_ = json.Unmarshal(b, out)
	return out
}

// field returns a pointer-addressable struct field by name.
func field(obj any, name string) reflect.Value {
	v := reflect.ValueOf(obj)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return reflect.Value{}
	}
	return v.FieldByName(name)
}

func getString(obj any, name string) string {
	if f := field(obj, name); f.IsValid() && f.Kind() == reflect.String {
		return f.String()
	}
	return ""
}

func setString(obj any, name, val string) {
	if f := field(obj, name); f.IsValid() && f.Kind() == reflect.String && f.CanSet() {
		f.SetString(val)
	}
}

func getID(obj any) uint64 {
	if f := field(obj, "Id"); f.IsValid() && f.Kind() == reflect.Uint64 {
		return f.Uint()
	}
	return 0
}

func setID(obj any, id uint64) {
	if f := field(obj, "Id"); f.IsValid() && f.Kind() == reflect.Uint64 {
		f.SetUint(id)
	}
}

// finish sets the output-only common fields of a stored resource.
func (s *Service) finish(k *kind, sc scope, path string, obj any) {
	setString(obj, "Kind", k.typ)
	setString(obj, "SelfLink", compute.SelfLink(path))
	setString(obj, "Region", sc.regionLink())
	if getString(obj, "CreationTimestamp") == "" {
		setString(obj, "CreationTimestamp", s.cmp.Timestamp())
	}
	if field(obj, "Fingerprint").IsValid() {
		setString(obj, "Fingerprint", compute.Fingerprint(obj))
	}
	if f := field(obj, "LabelFingerprint"); f.IsValid() {
		labels, _ := field(obj, "Labels").Interface().(map[string]string)
		setString(obj, "LabelFingerprint", compute.LabelFingerprint(labels))
	}
}

// --- store access ---

func (s *Service) load(k *kind, path string) (any, bool) {
	obj := k.newObj()
	var err error
	_ = s.env.Store.View(func(tx store.Tx) error { err = store.GetJSON(tx, k.ns(), path, obj); return nil })
	return obj, err == nil
}

func (s *Service) loadAll(k *kind, prefix string) []any {
	var out []any
	_ = s.env.Store.View(func(tx store.Tx) error {
		tx.Scan(k.ns(), prefix, func(_ string, b []byte) bool {
			obj := k.newObj()
			if json.Unmarshal(b, obj) == nil {
				out = append(out, obj)
			}
			return true
		})
		return nil
	})
	return out
}

// --- request scope ---

// reqScope validates the project and region of a request.
func (s *Service) reqScope(r *http.Request, k *kind) (scope, error) {
	sc := scope{project: r.PathValue("project"), region: r.PathValue("region")}
	if err := s.env.EnsureProject(sc.project); err != nil {
		return sc, err
	}
	if sc.region != "" && !locations.IsRegion(sc.region) {
		return sc, errNotFound("projects/" + sc.project + "/regions/" + sc.region)
	}
	return sc, nil
}

func (s *Service) check(ctx context.Context, perm, path string) error {
	return s.env.Auth.Check(ctx, perm, "//compute.googleapis.com/"+path)
}

// loadReq resolves, authorizes and loads the resource named by a request.
func (s *Service) loadReq(r *http.Request, k *kind, verb string) (scope, string, any, error) {
	sc, err := s.reqScope(r, k)
	if err != nil {
		return sc, "", nil, err
	}
	path := sc.coll(k.coll) + "/" + r.PathValue("name")
	if err := s.check(r.Context(), k.perm(sc, verb), path); err != nil {
		return sc, "", nil, err
	}
	obj, ok := s.load(k, path)
	if !ok {
		return sc, "", nil, errNotFound(path)
	}
	return sc, path, obj, nil
}

// --- generic handlers ---

func (s *Service) insertH(k *kind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sc, err := s.reqScope(r, k)
		if err != nil {
			apierr.Write(w, err)
			return
		}
		obj := k.newObj()
		if _, err := decode(r, obj); err != nil {
			apierr.Write(w, err)
			return
		}
		name := getString(obj, "Name")
		if name == "" {
			apierr.Write(w, errRequired("resource.name"))
			return
		}
		if err := validName("resource.name", name); err != nil {
			apierr.Write(w, err)
			return
		}
		path := sc.coll(k.coll) + "/" + name
		if err := s.check(r.Context(), k.perm(sc, "create"), path); err != nil {
			apierr.Write(w, err)
			return
		}
		if _, exists := s.load(k, path); exists {
			apierr.Write(w, errExists(path))
			return
		}
		// Output-only fields in the request are ignored.
		setString(obj, "CreationTimestamp", "")
		if k.prepare != nil {
			if err := k.prepare(r.Context(), s, sc, path, obj, nil); err != nil {
				apierr.Write(w, err)
				return
			}
		}
		setID(obj, s.env.IDs.Uint64())
		op, err := s.cmp.StartOperation(r.Context(), sc.project, sc.String(), "insert", path, getID(obj), func(ctx context.Context) error {
			if err := s.env.Store.Update(func(tx store.Tx) error {
				if store.Exists(tx, k.ns(), path) {
					return errExists(path)
				}
				s.finish(k, sc, path, obj)
				if k.inTx != nil {
					if err := k.inTx(tx, path, obj); err != nil {
						return err
					}
				}
				return store.PutJSON(tx, k.ns(), path, obj)
			}); err != nil {
				return err
			}
			if k.afterSave != nil {
				k.afterSave(ctx, s, path, obj, nil)
			}
			s.changed()
			return nil
		})
		reply(w, op, err)
	}
}

func (s *Service) getH(k *kind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _, obj, err := s.loadReq(r, k, "get")
		reply(w, obj, err)
	}
}

func (s *Service) listH(k *kind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sc, err := s.reqScope(r, k)
		if err != nil {
			apierr.Write(w, err)
			return
		}
		if err := s.check(r.Context(), k.perm(sc, "list"), "projects/"+sc.project); err != nil {
			apierr.Write(w, err)
			return
		}
		coll := sc.coll(k.coll)
		var items []compute.ListItem
		for _, obj := range s.loadAll(k, coll+"/") {
			items = append(items, compute.ListItem{Key: getString(obj, "Name"), Value: obj})
		}
		compute.WriteList(w, r, k.listKindName(), coll, items)
	}
}

func (s *Service) aggregatedH(k *kind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := r.PathValue("project")
		if err := s.env.EnsureProject(p); err != nil {
			apierr.Write(w, err)
			return
		}
		perm := k.gperm
		if perm == "" || k == kindForwardingRule {
			perm = k.rperm
		}
		if err := s.check(r.Context(), perm+".list", "projects/"+p); err != nil {
			apierr.Write(w, err)
			return
		}
		scoped := map[string][]compute.ListItem{}
		for _, obj := range s.loadAll(k, "projects/"+p+"/") {
			sc := scopeOfPath(relPath(getString(obj, "SelfLink")))
			key := sc.String()
			scoped[key] = append(scoped[key], compute.ListItem{Key: getString(obj, "Name"), Value: obj})
		}
		compute.WriteAggregated(w, r, k.aggKindName(), "projects/"+p+"/aggregated/"+k.coll, k.coll, scoped)
	}
}

func (s *Service) deleteH(k *kind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sc, path, obj, err := s.loadReq(r, k, "delete")
		if err != nil {
			apierr.Write(w, err)
			return
		}
		if u := s.usedBy(r.Context(), path); u != "" {
			apierr.Write(w, errInUse(k.snake, path, u))
			return
		}
		op, err := s.cmp.StartOperation(r.Context(), sc.project, sc.String(), "delete", path, getID(obj), func(ctx context.Context) error {
			if u := s.usedBy(ctx, path); u != "" {
				return errInUse(k.snake, path, u)
			}
			if err := s.env.Store.Update(func(tx store.Tx) error { return tx.Delete(k.ns(), path) }); err != nil {
				return err
			}
			if k.afterDelete != nil {
				k.afterDelete(ctx, s, path, obj)
			}
			s.changed()
			return nil
		})
		reply(w, op, err)
	}
}

// outputOnly lists the common fields PATCH and PUT never change.
var outputOnly = []string{"id", "kind", "name", "selfLink", "creationTimestamp", "region", "fingerprint", "labelFingerprint"}

// patchH implements PATCH (JSON merge patch, compute semantics) and, with
// replace, PUT (update: the body replaces every mutable field).
func (s *Service) patchH(k *kind, replace bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		verb, opType := "update", "patch"
		if replace {
			opType = "update"
		}
		sc, path, cur, err := s.loadReq(r, k, verb)
		if err != nil {
			apierr.Write(w, err)
			return
		}
		body, err := readBody(r)
		if err != nil {
			apierr.Write(w, err)
			return
		}
		body = normalizeBody(body, k.newObj())
		var probe map[string]any
		if err := json.Unmarshal(body, &probe); err != nil {
			apierr.Write(w, apierr.InvalidArgument("Invalid JSON payload received. %v", err).WithLegacy("parseError"))
			return
		}
		if fp, ok := probe["fingerprint"].(string); ok && fp != "" && field(cur, "Fingerprint").IsValid() && !fpEqual(fp, getString(cur, "Fingerprint")) {
			apierr.Write(w, errFingerprint())
			return
		}
		if n, ok := probe["name"].(string); ok && n != "" && n != getString(cur, "Name") {
			apierr.Write(w, errInvalid("resource.name", n, "Resource name cannot be changed."))
			return
		}
		next := k.newObj()
		keep := append(append([]string{}, outputOnly...), k.keep...)
		if replace {
			if err := unmarshalLenient(body, next); err != nil {
				apierr.Write(w, apierr.InvalidArgument("Invalid JSON payload received. %v", err).WithLegacy("parseError"))
				return
			}
			b, _ := json.Marshal(next)
			if err := compute.MergePatch(cur, b, next, keep...); err != nil {
				apierr.Write(w, err)
				return
			}
			// PUT replaces: drop fields absent from the body.
			full := map[string]any{}
			_ = json.Unmarshal(body, &full)
			cm := map[string]any{}
			b, _ = json.Marshal(cur)
			_ = json.Unmarshal(b, &cm)
			for _, f := range keep {
				if v, ok := cm[f]; ok {
					full[f] = v
				}
			}
			b, _ = json.Marshal(full)
			next = k.newObj()
			_ = json.Unmarshal(b, next)
		} else if err := compute.MergePatch(cur, body, next, keep...); err != nil {
			apierr.Write(w, err)
			return
		}
		if k.prepare != nil {
			if err := k.prepare(r.Context(), s, sc, path, next, cur); err != nil {
				apierr.Write(w, err)
				return
			}
		}
		op, err := s.cmp.StartOperation(r.Context(), sc.project, sc.String(), opType, path, getID(cur), func(ctx context.Context) error {
			return s.save(ctx, k, sc, path, next, cur)
		})
		reply(w, op, err)
	}
}

// save stores an updated resource and reconciles.
func (s *Service) save(ctx context.Context, k *kind, sc scope, path string, next, cur any) error {
	if err := s.env.Store.Update(func(tx store.Tx) error {
		if !store.Exists(tx, k.ns(), path) {
			return errNotFound(path)
		}
		s.finish(k, sc, path, next)
		if k.inTx != nil {
			if err := k.inTx(tx, path, next); err != nil {
				return err
			}
		}
		return store.PutJSON(tx, k.ns(), path, next)
	}); err != nil {
		return err
	}
	if k.afterSave != nil {
		k.afterSave(ctx, s, path, next, cur)
	}
	s.changed()
	return nil
}

// mutateH builds a custom update method (setUrlMap, setTarget, ...): req is
// decoded from the body (nil for none) and apply changes a copy of the
// resource.
func (s *Service) mutateH(k *kind, verb, opType string, newReq func() any, apply func(r *http.Request, sc scope, obj, req any) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sc, path, cur, err := s.loadReq(r, k, verb)
		if err != nil {
			apierr.Write(w, err)
			return
		}
		var req any
		if newReq != nil {
			req = newReq()
			if _, err := decode(r, req); err != nil {
				apierr.Write(w, err)
				return
			}
		}
		next := clone(k, cur)
		if err := apply(r, sc, next, req); err != nil {
			apierr.Write(w, err)
			return
		}
		if k.prepare != nil {
			if err := k.prepare(r.Context(), s, sc, path, next, cur); err != nil {
				apierr.Write(w, err)
				return
			}
		}
		op, err := s.cmp.StartOperation(r.Context(), sc.project, sc.String(), opType, path, getID(cur), func(ctx context.Context) error {
			return s.save(ctx, k, sc, path, next, cur)
		})
		reply(w, op, err)
	}
}

// setLabelsH implements setLabels with the labelFingerprint precondition.
func (s *Service) setLabelsH(k *kind) http.HandlerFunc {
	return s.mutateH(k, "setLabels", "setLabels",
		func() any { return &computev1.GlobalSetLabelsRequest{} },
		func(r *http.Request, sc scope, obj, req any) error {
			lr := req.(*computev1.GlobalSetLabelsRequest)
			if !fpEqual(lr.LabelFingerprint, getString(obj, "LabelFingerprint")) {
				return apierr.FailedPrecondition("Labels fingerprint either invalid or resource labels have changed").
					WithLegacy("conditionNotMet").WithHTTP(http.StatusPreconditionFailed)
			}
			field(obj, "Labels").Set(reflect.ValueOf(lr.Labels))
			return nil
		})
}

// routes registers every collection on the compute mux.
func (s *Service) routes(h func(pattern string, fn http.HandlerFunc)) {
	const p = "/compute/v1/projects/{project}"
	for _, k := range allKinds() {
		var bases []string
		if k.gperm != "" {
			bases = append(bases, p+"/global/"+k.coll)
		}
		if k.rperm != "" {
			bases = append(bases, p+"/regions/{region}/"+k.coll)
		}
		for _, b := range bases {
			h("POST "+b, s.insertH(k))
			h("GET "+b, s.listH(k))
			h("GET "+b+"/{name}", s.getH(k))
			h("DELETE "+b+"/{name}", s.deleteH(k))
			if !k.noPatch {
				h("PATCH "+b+"/{name}", s.patchH(k, false))
			}
			if !k.noUpdate {
				h("PUT "+b+"/{name}", s.patchH(k, true))
			}
			for verb, fn := range s.customMethods(k) {
				h("POST "+b+"/{name}/"+verb, fn)
			}
		}
		if k.gperm != "" && k.rperm != "" || k == kindForwardingRule {
			h("GET "+p+"/aggregated/"+k.coll, s.aggregatedH(k))
		}
	}
	s.extraRoutes(h, p)
}

// fpEqual compares fingerprints given in standard or URL-safe base64
// (gcloud re-encodes byte fields URL-safe).
func fpEqual(a, b string) bool {
	norm := func(s string) string { return strings.NewReplacer("-", "+", "_", "/").Replace(s) }
	return norm(a) == norm(b)
}
