package certs

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/certificatemanager/apiv1/certificatemanagerpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/networksecurity/apiv1/networksecuritypb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Store namespaces.
const (
	// nsRes holds every resource of both APIs as REST JSON, keyed by name.
	nsRes = "certs/resources"
	// nsKeys holds private keys of certificates (never returned), keyed by
	// certificate name.
	nsKeys = "certs/keys"
)

// The generic resource engine: create/get/list/patch/delete with
// google.longrunning operations, shared by REST and gRPC (AIP-121..135).

// listResult is one page of a list call.
type listResult struct {
	items []obj
	next  string
}

// now returns the current time as an RFC 3339 timestamp.
func (s *Service) now() string { return s.env.Clock.Now().UTC().Format(time.RFC3339Nano) }

// load returns a stored resource (nil when absent).
func load(tx store.Tx, name string) obj {
	b, ok := tx.Get(nsRes, name)
	if !ok {
		return nil
	}
	o := obj{}
	if json.Unmarshal(b, &o) != nil {
		return nil
	}
	return o
}

func put(tx store.Tx, name string, o obj) error { return store.PutJSON(tx, nsRes, name, o) }

// loadKind loads a resource of kind k by (possibly prefixed) name.
func (s *Service) loadKind(k *kind, name string) (obj, resName, error) {
	n, err := parseName(k, name)
	if err != nil {
		return nil, n, err
	}
	var o obj
	_ = s.env.Store.View(func(tx store.Tx) error { o = load(tx, n.name(k)); return nil })
	if o == nil {
		return nil, n, notFound(n.name(k))
	}
	return o, n, nil
}

// scan returns the resources of kind k under parent (location "-" matches
// every location), sorted by name.
func scan(tx store.Tx, k *kind, n resName) []obj {
	prefix := "projects/" + n.Project + "/locations/"
	if n.Location != "-" {
		prefix += n.Location + "/"
	}
	var out []obj
	tx.Scan(nsRes, prefix, func(key string, b []byte) bool {
		rn, err := parseName(k, key)
		if err != nil || (n.Location != "-" && rn.Location != n.Location) || (k.parentColl != "" && rn.Parent != n.Parent) {
			return true
		}
		o := obj{}
		if json.Unmarshal(b, &o) == nil {
			out = append(out, o)
		}
		return true
	})
	return out
}

// scanAll returns every resource of kind k in all projects.
func scanAll(tx store.Tx, k *kind) []obj {
	var out []obj
	tx.Scan(nsRes, "projects/", func(key string, b []byte) bool {
		if _, err := parseName(k, key); err != nil {
			return true
		}
		o := obj{}
		if json.Unmarshal(b, &o) == nil {
			out = append(out, o)
		}
		return true
	})
	return out
}

// operation starts an LRO for a mutation of name with verb.
func (s *Service) operation(ctx context.Context, k *kind, n resName, name, verb string, run func(ctx context.Context) (proto.Message, error)) (*longrunningpb.Operation, error) {
	parent := "projects/" + n.Project + "/locations/" + n.Location
	created := timestamppb.New(s.env.Clock.Now())
	var md proto.Message
	if k.api == "networksecurity" {
		md = &networksecuritypb.OperationMetadata{CreateTime: created, Target: name, Verb: verb, ApiVersion: "v1"}
	} else {
		md = &certificatemanagerpb.OperationMetadata{CreateTime: created, Target: name, Verb: verb, ApiVersion: "v1"}
	}
	return s.ops.Run(ctx, parent, md, func(ctx context.Context) (proto.Message, error) {
		resp, err := run(ctx)
		if err == nil {
			s.invalidate()
		}
		return resp, err
	})
}

// output converts a stored resource to its API form (computed fields
// filled in) as a proto message of kind k.
func (s *Service) outputProto(ctx context.Context, k *kind, o obj) (proto.Message, error) {
	m := k.newProto()
	if err := toProto(s.view(ctx, k, o), m); err != nil {
		return nil, apierr.Internal("encode %s: %v", k.title, err)
	}
	return m, nil
}

// create validates in and starts the create operation (AIP-133).
func (s *Service) create(ctx context.Context, k *kind, parent, id string, in obj) (*longrunningpb.Operation, error) {
	pn, err := parseParent(k, parent)
	if err != nil {
		return nil, err
	}
	pn.ID = id
	authRes := pn.locationFull(k)
	if k.parentColl != "" {
		authRes = "//" + k.host() + "/" + pn.parent(k)
	}
	if err := s.access(ctx, pn, k, k.perm+".create", authRes, false); err != nil {
		return nil, err
	}
	if err := validID(k, id); err != nil {
		return nil, err
	}
	name := pn.name(k)
	if in == nil {
		in = obj{}
	}
	res := clone(in)
	for _, f := range append([]string{"name", "createTime", "updateTime", "etag"}, k.output...) {
		delete(res, f)
	}
	normalize(k, res)
	var exists, parentOK bool
	_ = s.env.Store.View(func(tx store.Tx) error {
		exists = load(tx, name) != nil
		parentOK = k.parentColl == "" || load(tx, pn.parent(k)) != nil
		return nil
	})
	if !parentOK {
		return nil, notFound(pn.parent(k))
	}
	if exists {
		return nil, apierr.AlreadyExists("Resource '%s' already exists", name)
	}
	commit, err := s.prepare(ctx, k, pn, res, nil)
	if err != nil {
		return nil, err
	}
	for _, f := range k.inputOnly {
		delete(res, f)
	}
	res["name"] = name
	return s.operation(ctx, k, pn, name, "create", func(ctx context.Context) (proto.Message, error) {
		err := s.env.Store.Update(func(tx store.Tx) error {
			if load(tx, name) != nil {
				return apierr.AlreadyExists("Resource '%s' already exists", name)
			}
			if k.parentColl != "" && load(tx, pn.parent(k)) == nil {
				return notFound(pn.parent(k))
			}
			now := s.now()
			res["createTime"], res["updateTime"] = now, now
			if k.etag {
				res["etag"] = etagOf(res)
			}
			if commit != nil {
				if err := commit(tx); err != nil {
					return err
				}
			}
			return put(tx, name, res)
		})
		if err != nil {
			return nil, err
		}
		s.afterWrite(k, name)
		return s.outputProto(ctx, k, res)
	})
}

// get returns a resource in API form.
func (s *Service) get(ctx context.Context, k *kind, name string) (obj, error) {
	n, err := parseName(k, name)
	if err != nil {
		return nil, err
	}
	if err := s.access(ctx, n, k, k.perm+".get", n.fullName(k), false); err != nil {
		return nil, err
	}
	if k == kCert {
		s.reconcileCert(ctx, n.name(k))
	}
	o, _, err := s.loadKind(k, n.name(k))
	if err != nil {
		return nil, err
	}
	return s.view(ctx, k, o), nil
}

// list returns a page of resources under parent.
func (s *Service) list(ctx context.Context, k *kind, parent string, size int32, token, filter, orderBy string) (listResult, error) {
	pn, err := parseParent(k, parent)
	if err != nil {
		return listResult{}, err
	}
	authRes := pn.locationFull(k)
	if k.parentColl != "" {
		authRes = "//" + k.host() + "/" + pn.parent(k)
	}
	if err := s.access(ctx, pn, k, k.perm+".list", authRes, true); err != nil {
		return listResult{}, err
	}
	if size < 0 {
		return listResult{}, apierr.InvalidArgument("page_size must not be negative")
	}
	var items []obj
	var parentOK bool
	_ = s.env.Store.View(func(tx store.Tx) error {
		parentOK = k.parentColl == "" || load(tx, pn.parent(k)) != nil
		items = scan(tx, k, pn)
		return nil
	})
	if !parentOK {
		return listResult{}, notFound(pn.parent(k))
	}
	if k == kCert {
		changed := false
		for _, o := range items {
			if st, _ := getPath(o, "managed.state"); st == "PROVISIONING" {
				changed = s.reconcileCert(ctx, str(o["name"])) || changed
			}
		}
		if changed {
			_ = s.env.Store.View(func(tx store.Tx) error { items = scan(tx, k, pn); return nil })
		}
	}
	out := make([]obj, 0, len(items))
	for _, o := range items {
		v := s.view(ctx, k, o)
		if matchFilter(v, filter) {
			out = append(out, v)
		}
	}
	sortItems(out, orderBy)
	pg, next, err := page(out, token, size)
	if err != nil {
		return listResult{}, err
	}
	return listResult{items: pg, next: next}, nil
}

// patch applies the fields of in named by mask (AIP-134).
func (s *Service) patch(ctx context.Context, k *kind, name string, in obj, mask []string) (*longrunningpb.Operation, error) {
	n, err := parseName(k, name)
	if err != nil {
		return nil, err
	}
	if err := s.access(ctx, n, k, k.perm+".update", n.fullName(k), false); err != nil {
		return nil, err
	}
	name = n.name(k)
	old, _, err := s.loadKind(k, name)
	if err != nil {
		return nil, err
	}
	if in == nil {
		in = obj{}
	}
	if k.etag {
		if e := str(in["etag"]); e != "" && e != str(old["etag"]) {
			return nil, apierr.Aborted("The etag %q does not match the current etag of %s.", e, name)
		}
	}
	paths, err := maskPaths(k, in, mask)
	if err != nil {
		return nil, err
	}
	res := clone(old)
	for _, p := range paths {
		v, ok := getPath(in, p)
		setPath(res, p, v, ok)
	}
	normalize(k, res)
	for _, p := range k.immutable {
		a, aok := getPath(old, p)
		b, bok := getPath(res, p)
		if aok != bok || !jsonEqual(a, b) {
			return nil, apierr.InvalidArgument("Field %s is immutable.", p)
		}
	}
	commit, err := s.prepare(ctx, k, n, res, old)
	if err != nil {
		return nil, err
	}
	for _, f := range k.inputOnly {
		delete(res, f)
	}
	return s.operation(ctx, k, n, name, "update", func(ctx context.Context) (proto.Message, error) {
		err := s.env.Store.Update(func(tx store.Tx) error {
			cur := load(tx, name)
			if cur == nil {
				return notFound(name)
			}
			res["createTime"] = cur["createTime"]
			res["updateTime"] = s.now()
			if k.etag {
				delete(res, "etag")
				res["etag"] = etagOf(res)
			}
			if commit != nil {
				if err := commit(tx); err != nil {
					return err
				}
			}
			return put(tx, name, res)
		})
		if err != nil {
			return nil, err
		}
		s.afterWrite(k, name)
		return s.outputProto(ctx, k, res)
	})
}

// maskPaths returns the JSON paths to copy from in: the update mask
// (snake_case or lowerCamel), or every mutable field present when absent.
func maskPaths(k *kind, in obj, mask []string) ([]string, error) {
	known := jsonFields(k.newREST())
	skip := map[string]bool{"name": true, "createTime": true, "updateTime": true, "etag": true}
	for _, f := range k.output {
		skip[f] = true
	}
	var out []string
	if len(mask) == 0 {
		if k.api == "certificatemanager" {
			return nil, apierr.InvalidArgument("Field update_mask is required.")
		}
		for f := range known {
			if !skip[f] {
				out = append(out, f)
			}
		}
		sort.Strings(out)
		return out, nil
	}
	for _, p := range mask {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if p == "*" {
			for f := range known {
				if !skip[f] {
					out = append(out, f)
				}
			}
			continue
		}
		segs := strings.Split(p, ".")
		for i := range segs {
			segs[i] = camel(segs[i])
		}
		if !known[segs[0]] {
			return nil, apierr.InvalidArgument("Invalid update_mask path %q.", p)
		}
		if skip[segs[0]] {
			continue
		}
		// Map-valued fields are replaced whole ("labels.k" addresses a key).
		out = append(out, strings.Join(segs, "."))
	}
	sort.Strings(out)
	return out, nil
}

// jsonFields returns the JSON field names of a REST struct.
func jsonFields(v any) map[string]bool {
	t := reflect.TypeOf(v)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	out := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name != "" && name != "-" {
			out[name] = true
		}
	}
	return out
}

// remove deletes a resource (AIP-135).
func (s *Service) remove(ctx context.Context, k *kind, name, etag string) (*longrunningpb.Operation, error) {
	n, err := parseName(k, name)
	if err != nil {
		return nil, err
	}
	if err := s.access(ctx, n, k, k.perm+".delete", n.fullName(k), false); err != nil {
		return nil, err
	}
	name = n.name(k)
	old, _, err := s.loadKind(k, name)
	if err != nil {
		return nil, err
	}
	if k.etag && etag != "" && etag != str(old["etag"]) {
		return nil, apierr.Aborted("The etag %q does not match the current etag of %s.", etag, name)
	}
	if err := s.inUse(ctx, k, name); err != nil {
		return nil, err
	}
	return s.operation(ctx, k, n, name, "delete", func(ctx context.Context) (proto.Message, error) {
		err := s.env.Store.Update(func(tx store.Tx) error {
			if load(tx, name) == nil {
				return notFound(name)
			}
			_ = tx.Delete(nsKeys, name)
			return tx.Delete(nsRes, name)
		})
		if err != nil {
			return nil, err
		}
		s.afterWrite(k, name)
		return &emptypb.Empty{}, nil
	})
}

// etagOf computes a resource etag from its content.
func etagOf(o obj) string {
	c := clone(o)
	delete(c, "etag")
	delete(c, "updateTime")
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return base64.RawURLEncoding.EncodeToString(sum[:12])
}

// normalize drops enum values the API never echoes (the proto3 default
// and *_UNSPECIFIED values) so reads match GCP.
func normalize(k *kind, o obj) {
	var walk func(m map[string]any)
	walk = func(m map[string]any) {
		for key, v := range m {
			switch x := v.(type) {
			case string:
				if strings.HasSuffix(x, "_UNSPECIFIED") {
					delete(m, key)
				}
			case map[string]any:
				walk(x)
			case []any:
				for _, e := range x {
					if em, ok := e.(map[string]any); ok {
						walk(em)
					}
				}
			}
		}
	}
	walk(o)
	if k == kCert && o["scope"] == "DEFAULT" {
		delete(o, "scope")
	}
	if l, ok := o["labels"].(map[string]any); ok && len(l) == 0 {
		delete(o, "labels")
	}
}

// matchFilter implements the subset of AIP-160 used in practice:
// conjunctions of field=value / field:value comparisons on (dotted)
// fields, including labels.KEY. An empty filter matches everything.
func matchFilter(o obj, filter string) bool {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return true
	}
	for _, term := range strings.Split(filter, " AND ") {
		term = strings.TrimSpace(term)
		op := "="
		i := strings.IndexAny(term, "=:")
		if i < 0 {
			continue
		}
		if term[i] == ':' {
			op = ":"
		}
		field := strings.TrimSpace(strings.TrimSuffix(term[:i], "!"))
		neg := strings.HasSuffix(term[:i], "!")
		val := strings.Trim(strings.TrimSpace(term[i+1:]), `"`)
		v, ok := getPath(o, field)
		var got string
		switch x := v.(type) {
		case string:
			got = x
		case nil:
		default:
			b, _ := json.Marshal(x)
			got = string(b)
		}
		m := ok && (got == val || (op == ":" && (val == "*" || strings.Contains(got, val))))
		if m == neg {
			return false
		}
	}
	return true
}

// sortItems orders by name (default) or by "field [desc]" orderBy.
func sortItems(items []obj, orderBy string) {
	field, desc := "name", false
	if f := strings.Fields(orderBy); len(f) > 0 {
		field = camel(strings.TrimSuffix(f[0], ","))
		desc = len(f) > 1 && strings.EqualFold(f[1], "desc")
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, _ := getPath(items[i], field)
		b, _ := getPath(items[j], field)
		if desc {
			return str(a) > str(b)
		}
		return str(a) < str(b)
	})
}

// str returns v as a string ("" when not a string).
func str(v any) string {
	s, _ := v.(string)
	return s
}

// strs returns v as a string slice.
func strs(v any) []string {
	l, _ := v.([]any)
	out := make([]string, 0, len(l))
	for _, e := range l {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
