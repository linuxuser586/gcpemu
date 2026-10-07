package ar

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// REST surface: a small transcoder that maps the google.api.http rules of
// a gRPC service onto its implementation (AIP-127), so REST and gRPC share
// one code path.

// restHandler serves artifactregistry v1 REST (gcloud, OpenTofu, the Go
// REST client).
func (s *Service) restHandler() http.Handler {
	t := &transcoder{}
	if err := t.addService(&artifactregistrypb.ArtifactRegistry_ServiceDesc, s.api); err != nil {
		panic(err) // descriptors are compiled in; this cannot fail at runtime
	}
	loc := &locationsServer{s: s}
	t.add(http.MethodGet, "/v1/{name=projects/*}/locations", "", (&location.ListLocationsRequest{}).ProtoReflect().Descriptor(),
		func(ctx context.Context, m proto.Message) (proto.Message, error) {
			return loc.ListLocations(ctx, m.(*location.ListLocationsRequest))
		})
	t.add(http.MethodGet, "/v1/{name=projects/*/locations/*}", "", (&location.GetLocationRequest{}).ProtoReflect().Descriptor(),
		func(ctx context.Context, m proto.Message) (proto.Message, error) {
			return loc.GetLocation(ctx, m.(*location.GetLocationRequest))
		})
	t.sort()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rest, ok := strings.CutPrefix(r.URL.Path, "/v1/"); ok && s.ops.ServeREST(w, r, rest) {
			return
		}
		t.ServeHTTP(w, r)
	})
}

type callFunc func(ctx context.Context, req proto.Message) (proto.Message, error)

type route struct {
	method string
	tmpl   *pathTemplate
	body   string
	in     protoreflect.MessageDescriptor
	call   callFunc
}

type transcoder struct{ routes []*route }

// addService registers every method of sd that carries a google.api.http
// annotation, dispatching to impl through the generated handlers.
func (t *transcoder) addService(sd *grpc.ServiceDesc, impl any) error {
	d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(sd.ServiceName))
	if err != nil {
		return err
	}
	svc := d.(protoreflect.ServiceDescriptor)
	for _, md := range sd.Methods {
		m := svc.Methods().ByName(protoreflect.Name(md.MethodName))
		if m == nil {
			continue
		}
		rule, _ := proto.GetExtension(m.Options(), annotations.E_Http).(*annotations.HttpRule)
		if rule == nil {
			continue
		}
		h := md.Handler
		call := func(ctx context.Context, req proto.Message) (proto.Message, error) {
			out, err := h(impl, ctx, func(v any) error { proto.Merge(v.(proto.Message), req); return nil }, nil)
			if err != nil {
				return nil, err
			}
			return out.(proto.Message), nil
		}
		for _, r := range append([]*annotations.HttpRule{rule}, rule.GetAdditionalBindings()...) {
			verb, pat := ruleMethod(r)
			if pat == "" {
				continue
			}
			if err := t.add(verb, pat, r.GetBody(), m.Input(), call); err != nil {
				return err
			}
		}
	}
	return nil
}

func ruleMethod(r *annotations.HttpRule) (string, string) {
	switch p := r.GetPattern().(type) {
	case *annotations.HttpRule_Get:
		return http.MethodGet, p.Get
	case *annotations.HttpRule_Post:
		return http.MethodPost, p.Post
	case *annotations.HttpRule_Put:
		return http.MethodPut, p.Put
	case *annotations.HttpRule_Patch:
		return http.MethodPatch, p.Patch
	case *annotations.HttpRule_Delete:
		return http.MethodDelete, p.Delete
	case *annotations.HttpRule_Custom:
		return p.Custom.GetKind(), p.Custom.GetPath()
	}
	return "", ""
}

func (t *transcoder) add(method, pattern, body string, in protoreflect.MessageDescriptor, call callFunc) error {
	tmpl, err := parseTemplate(pattern)
	if err != nil {
		return err
	}
	t.routes = append(t.routes, &route{method: method, tmpl: tmpl, body: body, in: in, call: call})
	return nil
}

// sort orders routes so custom verbs and literal-rich templates match
// first and '**' templates last.
func (t *transcoder) sort() {
	score := func(r *route) int {
		n := 0
		if r.tmpl.verb != "" {
			n += 1000
		}
		for _, s := range r.tmpl.segs {
			switch s {
			case "**":
				n -= 100
			case "*":
			default:
				n += 10
			}
		}
		return n
	}
	sort.SliceStable(t.routes, func(i, j int) bool { return score(t.routes[i]) > score(t.routes[j]) })
}

func (t *transcoder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.EscapedPath()
	var matched bool
	for _, rt := range t.routes {
		vars, ok := rt.tmpl.match(path)
		if !ok {
			continue
		}
		matched = true
		if rt.method != r.Method {
			continue
		}
		rt.serve(w, r, vars)
		return
	}
	if matched {
		apierr.Write(w, apierr.New(codes.Unimplemented, "Method %s is not allowed for %s.", r.Method, r.URL.Path).WithHTTP(http.StatusMethodNotAllowed))
		return
	}
	apierr.Write(w, apierr.NotFound("The requested URL %s was not found on this server.", r.URL.Path))
}

func (rt *route) serve(w http.ResponseWriter, r *http.Request, vars [][2]string) {
	mt, err := protoregistry.GlobalTypes.FindMessageByName(rt.in.FullName())
	if err != nil {
		apierr.Write(w, err)
		return
	}
	req := mt.New().Interface()
	if err := rt.decode(r, req, vars); err != nil {
		apierr.Write(w, err)
		return
	}
	resp, err := rt.call(r.Context(), req)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	b, err := protojson.Marshal(resp)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_, _ = w.Write(b)
}

// systemParams are standard query parameters that never map to fields.
var systemParams = map[string]bool{
	"alt": true, "$alt": true, "prettyPrint": true, "fields": true, "$fields": true, "key": true,
	"access_token": true, "quotaUser": true, "callback": true, "upload_protocol": true,
	"uploadType": true, "$.xgafv": true, "$httpVersion": true, "oauth_token": true, "userIp": true,
}

// decode fills req from the body, query parameters and path variables.
func (rt *route) decode(r *http.Request, req proto.Message, vars [][2]string) error {
	if rt.body != "" {
		b, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
		if err != nil {
			return apierr.InvalidArgument("read body: %v", err)
		}
		if len(strings.TrimSpace(string(b))) > 0 {
			target := req
			if rt.body != "*" {
				fd := findField(req.ProtoReflect().Descriptor(), rt.body)
				if fd == nil || fd.Kind() != protoreflect.MessageKind {
					return apierr.Internal("bad body field %q", rt.body)
				}
				target = req.ProtoReflect().Mutable(fd).Message().Interface()
			}
			if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, target); err != nil {
				return apierr.InvalidArgument("Invalid JSON payload received. %v", err)
			}
		}
	}
	if rt.body != "*" {
		for k, vs := range r.URL.Query() {
			if systemParams[k] {
				continue
			}
			for _, v := range vs {
				if err := setPath(req.ProtoReflect(), k, v, false); err != nil {
					return err
				}
			}
		}
	}
	for _, kv := range vars {
		if err := setPath(req.ProtoReflect(), kv[0], kv[1], true); err != nil {
			return err
		}
	}
	return nil
}

func findField(md protoreflect.MessageDescriptor, name string) protoreflect.FieldDescriptor {
	fs := md.Fields()
	if fd := fs.ByName(protoreflect.Name(name)); fd != nil {
		return fd
	}
	if fd := fs.ByJSONName(name); fd != nil {
		return fd
	}
	return fs.ByTextName(name)
}

// setPath assigns a string value to a dotted field path. Unknown query
// parameters are ignored; unknown path variables are an error.
func setPath(m protoreflect.Message, path, val string, strict bool) error {
	parts := strings.Split(path, ".")
	for i, p := range parts {
		fd := findField(m.Descriptor(), p)
		if fd == nil {
			if strict {
				return apierr.Internal("unknown field %q", path)
			}
			return nil
		}
		if i < len(parts)-1 {
			if fd.Kind() != protoreflect.MessageKind || fd.IsList() || fd.IsMap() {
				return apierr.InvalidArgument("Invalid parameter %q.", path)
			}
			m = m.Mutable(fd).Message()
			continue
		}
		if fd.IsMap() {
			return apierr.InvalidArgument("Invalid parameter %q: map fields cannot be set from the query.", path)
		}
		v, err := parseValue(m, fd, val)
		if err != nil {
			return apierr.InvalidArgument("Invalid value at '%s' (%s), %q", path, fd.Kind(), val)
		}
		if fd.IsList() {
			m.Mutable(fd).List().Append(v)
		} else {
			m.Set(fd, v)
		}
	}
	return nil
}

func parseValue(m protoreflect.Message, fd protoreflect.FieldDescriptor, s string) (protoreflect.Value, error) {
	switch fd.Kind() {
	case protoreflect.StringKind:
		return protoreflect.ValueOfString(s), nil
	case protoreflect.BoolKind:
		b, err := strconv.ParseBool(s)
		return protoreflect.ValueOfBool(b), err
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		n, err := strconv.ParseInt(s, 10, 32)
		return protoreflect.ValueOfInt32(int32(n)), err
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		n, err := strconv.ParseInt(s, 10, 64)
		return protoreflect.ValueOfInt64(n), err
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		n, err := strconv.ParseUint(s, 10, 32)
		return protoreflect.ValueOfUint32(uint32(n)), err
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		n, err := strconv.ParseUint(s, 10, 64)
		return protoreflect.ValueOfUint64(n), err
	case protoreflect.FloatKind:
		f, err := strconv.ParseFloat(s, 32)
		return protoreflect.ValueOfFloat32(float32(f)), err
	case protoreflect.DoubleKind:
		f, err := strconv.ParseFloat(s, 64)
		return protoreflect.ValueOfFloat64(f), err
	case protoreflect.BytesKind:
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			b, err = base64.URLEncoding.DecodeString(s)
		}
		return protoreflect.ValueOfBytes(b), err
	case protoreflect.EnumKind:
		if ev := fd.Enum().Values().ByName(protoreflect.Name(s)); ev != nil {
			return protoreflect.ValueOfEnum(ev.Number()), nil
		}
		n, err := strconv.Atoi(s)
		return protoreflect.ValueOfEnum(protoreflect.EnumNumber(n)), err
	case protoreflect.MessageKind:
		var msg proto.Message
		switch fd.Message().FullName() {
		case "google.protobuf.FieldMask":
			fm := &fieldmaskpb.FieldMask{}
			for _, p := range strings.Split(s, ",") {
				if p = strings.TrimSpace(p); p != "" {
					fm.Paths = append(fm.Paths, snakeCase(p))
				}
			}
			msg = fm
		case "google.protobuf.Timestamp":
			t, err := time.Parse(time.RFC3339Nano, s)
			if err != nil {
				return protoreflect.Value{}, err
			}
			msg = timestamppb.New(t)
		case "google.protobuf.Duration":
			d, err := time.ParseDuration(s)
			if err != nil {
				return protoreflect.Value{}, err
			}
			msg = durationpb.New(d)
		default:
			if f := fd.Message().Fields().ByName("value"); f != nil && strings.HasPrefix(string(fd.Message().FullName()), "google.protobuf.") {
				nm := m.NewField(fd).Message()
				v, err := parseValue(nm, f, s)
				if err != nil {
					return protoreflect.Value{}, err
				}
				nm.Set(f, v)
				return protoreflect.ValueOfMessage(nm), nil
			}
			return protoreflect.Value{}, fmt.Errorf("unsupported message type %s", fd.Message().FullName())
		}
		return protoreflect.ValueOfMessage(msg.ProtoReflect()), nil
	}
	return protoreflect.Value{}, fmt.Errorf("unsupported kind %s", fd.Kind())
}

// ---- path templates (google.api.http syntax) ----

// pathTemplate is a parsed template such as
// "/v1/{parent=projects/*/locations/*}/repositories" or "...}:getIamPolicy".
type pathTemplate struct {
	segs []string // literal, "*" or "**"
	vars []tvar
	verb string
}

type tvar struct {
	field      string
	start, end int // segs[start:end]
}

func parseTemplate(s string) (*pathTemplate, error) {
	t := &pathTemplate{}
	s = strings.TrimPrefix(s, "/")
	if i := strings.LastIndexByte(s, ':'); i >= 0 && i > strings.LastIndexByte(s, '/') && i > strings.LastIndexByte(s, '}') {
		s, t.verb = s[:i], s[i+1:]
	}
	for len(s) > 0 {
		if s[0] == '{' {
			end := strings.IndexByte(s, '}')
			if end < 0 {
				return nil, fmt.Errorf("unterminated variable in %q", s)
			}
			field, pat, ok := strings.Cut(s[1:end], "=")
			if !ok {
				pat = "*"
			}
			v := tvar{field: field, start: len(t.segs)}
			t.segs = append(t.segs, strings.Split(pat, "/")...)
			v.end = len(t.segs)
			t.vars = append(t.vars, v)
			s = s[end+1:]
		} else {
			end := strings.IndexByte(s, '/')
			if end < 0 {
				end = len(s)
			}
			t.segs = append(t.segs, s[:end])
			s = s[end:]
		}
		s = strings.TrimPrefix(s, "/")
	}
	return t, nil
}

// match matches an escaped request path and returns the variable values
// (each segment unescaped; a '/' decoded inside a segment is re-encoded as
// %2F, the form AR uses in resource IDs).
func (t *pathTemplate) match(path string) ([][2]string, bool) {
	path = strings.TrimPrefix(path, "/")
	if t.verb != "" {
		p, ok := strings.CutSuffix(path, ":"+t.verb)
		if !ok {
			return nil, false
		}
		path = p
	}
	req := strings.Split(path, "/")
	// Map template segment index → request segment range.
	type span struct{ from, to int }
	spans := make([]span, len(t.segs))
	j := 0
	for i, seg := range t.segs {
		if seg == "**" {
			rest := len(t.segs) - i - 1
			n := len(req) - j - rest
			if n < 1 {
				return nil, false
			}
			spans[i] = span{j, j + n}
			j += n
			continue
		}
		if j >= len(req) {
			return nil, false
		}
		if seg == "*" {
			if req[j] == "" {
				return nil, false
			}
		} else if seg != req[j] {
			return nil, false
		}
		spans[i] = span{j, j + 1}
		j++
	}
	if j != len(req) {
		return nil, false
	}
	vars := make([][2]string, 0, len(t.vars))
	for _, v := range t.vars {
		if v.end <= v.start {
			continue
		}
		parts := req[spans[v.start].from:spans[v.end-1].to]
		dec := make([]string, len(parts))
		for k, p := range parts {
			u, err := url.PathUnescape(p)
			if err != nil {
				return nil, false
			}
			dec[k] = strings.ReplaceAll(u, "/", "%2F")
		}
		vars = append(vars, [2]string{v.field, strings.Join(dec, "/")})
	}
	return vars, true
}
