package apidef

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// RESTAPI is a discovery (JSON/REST) API.
type RESTAPI struct {
	Name        string // e.g. compute
	Version     string // e.g. v1
	Revision    string // the discovery document's revision
	ServicePath string // e.g. compute/v1/
	Methods     []*RESTMethod
	Schemas     map[string]*Schema

	routes map[string][]*restRoute // HTTP method → routes, best match first
}

// RESTMethod is one discovery method.
type RESTMethod struct {
	ID           string   // e.g. compute.addresses.insert
	HTTPMethod   string   // GET, POST, ...
	Path         string   // flat path relative to ServicePath
	Request      string   // request body schema, if any
	RequestParam string   // the body's parameter name in errors (compute: "resource")
	Params       []string // required query parameters
}

// Schema is the field annotations of one discovery schema. Inline object
// schemas are named after their parent ("Firewall.allowed[]").
type Schema struct {
	// Required maps a property to the methods that require it
	// (annotations.required).
	Required map[string][]string
	// OutputOnly lists readOnly properties.
	OutputOnly []string
	// Refs are the properties whose values carry annotated schemas.
	Refs map[string]Ref
}

// RefKind is how a property holds a nested schema.
type RefKind uint8

const (
	RefObject RefKind = iota // a single object
	RefList                  // an array of objects
	RefMap                   // an object of objects (additionalProperties)
)

// Ref is a property's nested schema.
type Ref struct {
	Schema string
	Kind   RefKind
}

var restAPIs = map[string]*RESTAPI{}

// REST returns a discovery API by name and version, or nil.
func REST(name, version string) *RESTAPI { return restAPIs[name+"/"+version] }

// Revision returns the pinned discovery document revision of an API.
func Revision(name, version string) string {
	if a := REST(name, version); a != nil {
		return a.Revision
	}
	return ""
}

type restRoute struct {
	m     *RESTMethod
	segs  []string // literal, or "" for a variable
	verb  string   // ":verb" suffix of the last segment
	score int
}

func (a *RESTAPI) index() {
	a.routes = map[string][]*restRoute{}
	for _, m := range a.Methods {
		rt := &restRoute{m: m}
		for _, s := range strings.Split(m.Path, "/") {
			if strings.HasPrefix(s, "{") {
				if i := strings.Index(s, "}:"); i >= 0 {
					rt.verb = s[i+1:]
				}
				rt.segs = append(rt.segs, "")
				continue
			}
			rt.segs = append(rt.segs, s)
			rt.score++
		}
		if rt.verb != "" {
			rt.score += 100
		}
		a.routes[m.HTTPMethod] = append(a.routes[m.HTTPMethod], rt)
	}
	for _, rs := range a.routes {
		sort.SliceStable(rs, func(i, j int) bool { return rs[i].score > rs[j].score })
	}
}

// Rel returns a request path relative to the API's service path, accepting
// both the real host form (/compute/v1/projects/...) and the gateway's
// prefix-stripped form (/v1/projects/...).
func (a *RESTAPI) Rel(path string) string {
	if a.ServicePath != "" {
		if i := strings.Index(path, "/"+a.ServicePath); i >= 0 {
			return path[i+1+len(a.ServicePath):]
		}
		if i := strings.Index(path, "/"+a.Version+"/"); i >= 0 {
			return path[i+len(a.Version)+2:]
		}
	}
	return strings.TrimPrefix(path, "/")
}

// Match returns the method serving an HTTP method and an escaped path
// relative to the service path (see Rel), or nil.
func (a *RESTAPI) Match(httpMethod, rel string) *RESTMethod {
	req := strings.Split(rel, "/")
	for _, rt := range a.routes[httpMethod] {
		if rt.match(req) {
			return rt.m
		}
	}
	return nil
}

func (rt *restRoute) match(req []string) bool {
	if len(req) != len(rt.segs) {
		return false
	}
	last := len(req) - 1
	for i, s := range rt.segs {
		r := req[i]
		if i == last && rt.verb != "" {
			v, ok := strings.CutSuffix(r, rt.verb)
			if !ok {
				return false
			}
			r = v
		}
		if s == "" {
			if r == "" {
				return false
			}
		} else if s != r {
			return false
		}
	}
	return true
}

// Handler validates each request h serves with CheckJSON first (required
// parameters and body fields); requests that match no method pass through.
func (a *RESTAPI) Handler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := a.Match(r.Method, a.Rel(r.URL.EscapedPath()))
		if m == nil || (len(m.Params) == 0 && a.Schemas[m.Request] == nil) {
			h.ServeHTTP(w, r)
			return
		}
		var body []byte
		if a.Schemas[m.Request] != nil && r.Body != nil {
			b, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
			if err != nil || len(b) > maxBody {
				h.ServeHTTP(w, r) // the service reports it
				return
			}
			body = b
			r.Body = io.NopCloser(bytes.NewReader(b))
		}
		if err := a.CheckJSON(m, r.URL.Query(), body); err != nil {
			apierr.Write(w, err)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// maxBody bounds the request bodies Handler buffers to check.
const maxBody = 8 << 20

// CheckJSON validates a request against m's annotations: required query
// parameters, and required body fields (annotations.required naming m).
// The errors match the compute API's ("Required field 'resource.name' not
// specified").
func (a *RESTAPI) CheckJSON(m *RESTMethod, query url.Values, body []byte) error {
	for _, p := range m.Params {
		if query.Get(p) == "" {
			return apierr.InvalidArgument("Required parameter: %s", p).WithLegacy("required")
		}
	}
	if m.Request == "" {
		return nil
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil // services report a missing body themselves
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if dec.Decode(&v) != nil {
		return nil // and malformed JSON
	}
	prefix := m.RequestParam
	if prefix == "" {
		prefix = "resource"
	}
	return a.checkRequired(m.ID, m.Request, prefix, v)
}

func (a *RESTAPI) checkRequired(method, schema, path string, v any) error {
	sc := a.Schemas[schema]
	obj, ok := v.(map[string]any)
	if sc == nil || !ok {
		return nil
	}
	for _, f := range slices.Sorted(maps.Keys(sc.Required)) {
		if !slices.Contains(sc.Required[f], method) {
			continue
		}
		if isEmptyJSON(obj[f]) {
			return apierr.InvalidArgument("Required field '%s.%s' not specified", path, f).WithLegacy("required")
		}
	}
	for _, f := range slices.Sorted(maps.Keys(sc.Refs)) {
		ref := sc.Refs[f]
		fv, ok := obj[f]
		if !ok {
			continue
		}
		switch ref.Kind {
		case RefObject:
			if err := a.checkRequired(method, ref.Schema, path+"."+f, fv); err != nil {
				return err
			}
		case RefList:
			l, _ := fv.([]any)
			for i, e := range l {
				if err := a.checkRequired(method, ref.Schema, fmt.Sprintf("%s.%s[%d]", path, f, i), e); err != nil {
					return err
				}
			}
		case RefMap:
			m, _ := fv.(map[string]any)
			for _, k := range slices.Sorted(maps.Keys(m)) {
				if err := a.checkRequired(method, ref.Schema, path+"."+f+"."+k, m[k]); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func isEmptyJSON(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	}
	return false
}
