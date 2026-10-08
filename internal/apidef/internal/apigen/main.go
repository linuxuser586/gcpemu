// Command apigen generates internal/apidef's route and field-behaviour
// tables (NFR-MNT-001) from Google's published API definitions:
//
//	go run ./internal/apidef/internal/apigen [-googleapis latest|SHA] -config apis.yaml -out DIR
//
// The gRPC APIs come from the .proto files of googleapis/googleapis at the
// commit pinned in apis.yaml (fetched from GitHub and cached under the
// user cache directory), compiled with protocompile: each method's
// google.api.http bindings and each reachable message's
// google.api.field_behavior annotations are written to zz_googleapis.go.
//
// The discovery APIs come from the discovery documents shipped in the
// google.golang.org/api module at the version go.mod pins: each method's
// path and required parameters, and the body fields its
// annotations.required names, are written to zz_discovery.go.
//
// -googleapis moves the pin (to a commit, or to the head of master)
// before generating.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"go/format"
	"io"
	"log"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/bufbuild/protocompile"
	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"gopkg.in/yaml.v3"
)

type config struct {
	Googleapis struct {
		Commit string   `yaml:"commit"`
		Files  []string `yaml:"files"`
		// Relax drops field behaviours the real APIs don't enforce:
		// "message.field" → behaviours (REQUIRED, ...).
		Relax map[string][]string `yaml:"relax"`
	} `yaml:"googleapis"`
	Discovery []struct {
		API     string `yaml:"api"`
		Version string `yaml:"version"`
		// Relax drops methods from annotations.required the real API
		// doesn't enforce: "Schema.property" → method IDs.
		Relax map[string][]string `yaml:"relax"`
	} `yaml:"discovery"`
}

var client = &http.Client{Timeout: time.Minute}

func main() {
	cfgPath := flag.String("config", "apis.yaml", "pin file")
	out := flag.String("out", ".", "output directory")
	bump := flag.String("googleapis", "", `move the googleapis pin first: a commit SHA, or "latest"`)
	flag.Parse()
	log.SetFlags(0)
	log.SetPrefix("apigen: ")

	if *bump != "" {
		if err := setCommit(*cfgPath, *bump); err != nil {
			log.Fatal(err)
		}
	}
	raw, err := os.ReadFile(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	var cfg config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		log.Fatalf("%s: %v", *cfgPath, err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(cfg.Googleapis.Commit) {
		log.Fatalf("%s: googleapis.commit must be a full commit SHA, got %q", *cfgPath, cfg.Googleapis.Commit)
	}

	src, err := genProtos(cfg.Googleapis.Commit, cfg.Googleapis.Files, cfg.Googleapis.Relax)
	if err != nil {
		log.Fatal(err)
	}
	if err := write(filepath.Join(*out, "zz_googleapis.go"), src); err != nil {
		log.Fatal(err)
	}
	src, err = genDiscovery(cfg)
	if err != nil {
		log.Fatal(err)
	}
	if err := write(filepath.Join(*out, "zz_discovery.go"), src); err != nil {
		log.Fatal(err)
	}
}

func write(path string, src []byte) error {
	b, err := format.Source(src)
	if err != nil {
		return fmt.Errorf("%s: %v", path, err)
	}
	return os.WriteFile(path, b, 0o644)
}

// setCommit rewrites googleapis.commit in the pin file, keeping comments.
func setCommit(cfgPath, sha string) error {
	if sha == "latest" {
		req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/repos/googleapis/googleapis/commits/master", nil)
		req.Header.Set("Accept", "application/vnd.github.sha")
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("resolve googleapis master: %s: %s", resp.Status, b)
		}
		sha = strings.TrimSpace(string(b))
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return err
	}
	re := regexp.MustCompile(`(?m)^(\s+commit:\s*)\S+`)
	if !re.Match(raw) {
		return fmt.Errorf("%s: no googleapis commit line", cfgPath)
	}
	log.Printf("googleapis pinned to %s", sha)
	return os.WriteFile(cfgPath, re.ReplaceAll(raw, []byte("${1}"+sha)), 0o644)
}

// ---- googleapis ----

// fetcher reads googleapis files at one commit, caching them on disk.
type fetcher struct{ commit, dir string }

func (f *fetcher) open(path string) (io.ReadCloser, error) {
	local := filepath.Join(f.dir, filepath.FromSlash(path))
	if r, err := os.Open(local); err == nil {
		return r, nil
	}
	url := "https://raw.githubusercontent.com/googleapis/googleapis/" + f.commit + "/" + path
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, os.ErrNotExist // let protocompile fall back to its standard imports
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err == nil {
		tmp := local + ".tmp"
		if os.WriteFile(tmp, b, 0o644) == nil {
			_ = os.Rename(tmp, local)
		}
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func genProtos(commit string, files []string, relax map[string][]string) ([]byte, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		cache = os.TempDir()
	}
	f := &fetcher{commit: commit, dir: filepath.Join(cache, "gcpemu", "googleapis", commit)}
	c := protocompile.Compiler{
		Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{Accessor: f.open}),
	}
	compiled, err := c.Compile(context.Background(), files...)
	if err != nil {
		return nil, fmt.Errorf("compile googleapis@%s: %v", commit, err)
	}

	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by apigen from googleapis/googleapis@%s; DO NOT EDIT.\n\npackage apidef\n\n", commit)
	fmt.Fprintf(&b, "const googleapisCommit = %q\n\n", commit)
	b.WriteString("var protoServices = []*Service{\n")
	var roots []protoreflect.MessageDescriptor
	for _, fd := range compiled {
		svcs := fd.Services()
		for i := 0; i < svcs.Len(); i++ {
			sd := svcs.Get(i)
			fmt.Fprintf(&b, "{Name: %q, File: %q, Methods: []*Method{\n", sd.FullName(), fd.Path())
			ms := sd.Methods()
			for j := 0; j < ms.Len(); j++ {
				md := ms.Get(j)
				roots = append(roots, md.Input())
				fmt.Fprintf(&b, "{Name: %q, Input: %q, Output: %q", md.Name(), md.Input().FullName(), md.Output().FullName())
				var rule annotations.HttpRule
				if ok, err := extension(md.Options(), &descriptorpb.MethodOptions{}, annotations.E_Http, &rule); err != nil {
					return nil, fmt.Errorf("%s: %v", md.FullName(), err)
				} else if ok {
					b.WriteString(", HTTP: []Binding{")
					for _, r := range append([]*annotations.HttpRule{&rule}, rule.GetAdditionalBindings()...) {
						verb, path := ruleMethod(r)
						if path != "" {
							fmt.Fprintf(&b, "{%q, %q, %q}, ", verb, path, r.GetBody())
						}
					}
					b.WriteString("}")
				}
				b.WriteString("},\n")
			}
			b.WriteString("}},\n")
		}
	}
	b.WriteString("}\n\n")

	// Field behaviours of every message reachable from a request.
	b.WriteString("var protoFields = map[string]map[string]Behavior{\n")
	seen := map[protoreflect.FullName]bool{}
	var walk func(protoreflect.MessageDescriptor) error
	var msgs []protoreflect.MessageDescriptor
	walk = func(m protoreflect.MessageDescriptor) error {
		if seen[m.FullName()] {
			return nil
		}
		seen[m.FullName()] = true
		msgs = append(msgs, m)
		fs := m.Fields()
		for i := 0; i < fs.Len(); i++ {
			if fm := fs.Get(i).Message(); fm != nil {
				if err := walk(fm); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, m := range roots {
		if err := walk(m); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(msgs, func(a, b protoreflect.MessageDescriptor) int {
		return strings.Compare(string(a.FullName()), string(b.FullName()))
	})
	for _, m := range msgs {
		var lines []string
		fs := m.Fields()
		for i := 0; i < fs.Len(); i++ {
			fd := fs.Get(i)
			var opts descriptorpb.FieldOptions
			if err := remarshal(fd.Options(), &opts); err != nil {
				return nil, fmt.Errorf("%s: %v", fd.FullName(), err)
			}
			bs, _ := proto.GetExtension(&opts, annotations.E_FieldBehavior).([]annotations.FieldBehavior)
			if drop, ok := relax[string(fd.FullName())]; ok {
				delete(relax, string(fd.FullName()))
				bs = slices.DeleteFunc(slices.Clone(bs), func(b annotations.FieldBehavior) bool { return slices.Contains(drop, b.String()) })
			}
			if bits := behaviorExpr(bs); bits != "" {
				lines = append(lines, fmt.Sprintf("%q: %s,\n", fd.Name(), bits))
			}
		}
		if len(lines) > 0 {
			fmt.Fprintf(&b, "%q: {\n%s},\n", m.FullName(), strings.Join(lines, ""))
		}
	}
	b.WriteString("}\n")
	if len(relax) > 0 {
		return nil, fmt.Errorf("relax: no such fields: %v", slices.Sorted(maps.Keys(relax)))
	}
	return b.Bytes(), nil
}

func behaviorExpr(bs []annotations.FieldBehavior) string {
	var names []string
	for _, v := range bs {
		var n string
		switch v {
		case annotations.FieldBehavior_REQUIRED:
			n = "Required"
		case annotations.FieldBehavior_OUTPUT_ONLY:
			n = "OutputOnly"
		case annotations.FieldBehavior_IMMUTABLE:
			n = "Immutable"
		case annotations.FieldBehavior_INPUT_ONLY:
			n = "InputOnly"
		case annotations.FieldBehavior_IDENTIFIER:
			n = "Identifier"
		case annotations.FieldBehavior_OPTIONAL:
			n = "Optional"
		default:
			continue
		}
		if !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	return strings.Join(names, " | ")
}

// remarshal copies an options message compiled by protocompile into a
// generated one so that extensions registered in the Go registry (the
// google.api annotations) can be read with proto.GetExtension.
func remarshal(src proto.Message, dst proto.Message) error {
	b, err := proto.Marshal(src)
	if err != nil {
		return err
	}
	return proto.UnmarshalOptions{Resolver: protoregistry.GlobalTypes}.Unmarshal(b, dst)
}

func extension(src, opts proto.Message, xt protoreflect.ExtensionType, dst proto.Message) (bool, error) {
	if err := remarshal(src, opts); err != nil {
		return false, err
	}
	if !proto.HasExtension(opts, xt) {
		return false, nil
	}
	proto.Merge(dst, proto.GetExtension(opts, xt).(proto.Message))
	return true, nil
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

// ---- discovery ----

type dDoc struct {
	Name        string                `json:"name"`
	Version     string                `json:"version"`
	Revision    string                `json:"revision"`
	ServicePath string                `json:"servicePath"`
	Schemas     map[string]*dSchema   `json:"schemas"`
	Methods     map[string]*dMethod   `json:"methods"`
	Resources   map[string]*dResource `json:"resources"`
}

type dResource struct {
	Methods   map[string]*dMethod   `json:"methods"`
	Resources map[string]*dResource `json:"resources"`
}

type dMethod struct {
	ID         string `json:"id"`
	Path       string `json:"path"`
	FlatPath   string `json:"flatPath"`
	HTTPMethod string `json:"httpMethod"`
	Parameters map[string]struct {
		Location string `json:"location"`
		Required bool   `json:"required"`
	} `json:"parameters"`
	Request *struct {
		Ref           string `json:"$ref"`
		ParameterName string `json:"parameterName"`
	} `json:"request"`
}

type dSchema struct {
	Ref                  string              `json:"$ref"`
	Type                 string              `json:"type"`
	Properties           map[string]*dSchema `json:"properties"`
	Items                *dSchema            `json:"items"`
	AdditionalProperties *dSchema            `json:"additionalProperties"`
	Annotations          struct {
		Required []string `json:"required"`
	} `json:"annotations"`
}

// schema is the generated form of one (possibly inline) schema.
type schema struct {
	required map[string][]string
	refs     map[string][2]string // property → {schema, kind}
}

func genDiscovery(cfg config) ([]byte, error) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Version}} {{.Dir}}", "google.golang.org/api").Output()
	if err != nil {
		return nil, fmt.Errorf("locate google.golang.org/api (run go mod download): %v", err)
	}
	modVersion, modDir, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	if modDir == "" {
		return nil, fmt.Errorf("google.golang.org/api %s is not in the module cache (run go mod download)", modVersion)
	}

	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by apigen from the discovery documents of google.golang.org/api@%s; DO NOT EDIT.\n\npackage apidef\n\n", modVersion)
	fmt.Fprintf(&b, "const discoveryModule = %q\n\n", "google.golang.org/api@"+modVersion)
	b.WriteString("var discoveryAPIs = []*RESTAPI{\n")
	for _, d := range cfg.Discovery {
		path := filepath.Join(modDir, d.API, d.Version, d.API+"-api.json")
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var doc dDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("%s: %v", path, err)
		}
		if err := renderAPI(&b, &doc, d.Relax); err != nil {
			return nil, fmt.Errorf("%s: %v", path, err)
		}
	}
	b.WriteString("}\n")
	return b.Bytes(), nil
}

var plusVar = regexp.MustCompile(`\{\+([^}]+)\}`)

func renderAPI(b *bytes.Buffer, doc *dDoc, relax map[string][]string) error {
	var methods []*dMethod
	var collect func(map[string]*dMethod, map[string]*dResource)
	collect = func(ms map[string]*dMethod, rs map[string]*dResource) {
		for _, m := range ms {
			methods = append(methods, m)
		}
		for _, r := range rs {
			collect(r.Methods, r.Resources)
		}
	}
	collect(doc.Methods, doc.Resources)
	slices.SortFunc(methods, func(a, b *dMethod) int { return strings.Compare(a.ID, b.ID) })

	// Flatten the schemas: inline objects get "Parent.prop" names.
	schemas := map[string]*schema{}
	var flatten func(name string, s *dSchema)
	ref := func(parent, prop string, s *dSchema) (string, string) {
		switch {
		case s.Ref != "":
			return s.Ref, "RefObject"
		case s.Type == "array" && s.Items != nil:
			if s.Items.Ref != "" {
				return s.Items.Ref, "RefList"
			}
			if s.Items.Properties != nil {
				n := parent + "." + prop + "[]"
				flatten(n, s.Items)
				return n, "RefList"
			}
		case s.Type == "object" && s.AdditionalProperties != nil:
			if s.AdditionalProperties.Ref != "" {
				return s.AdditionalProperties.Ref, "RefMap"
			}
			if s.AdditionalProperties.Properties != nil {
				n := parent + "." + prop + "{}"
				flatten(n, s.AdditionalProperties)
				return n, "RefMap"
			}
		case s.Properties != nil:
			n := parent + "." + prop
			flatten(n, s)
			return n, "RefObject"
		}
		return "", ""
	}
	flatten = func(name string, s *dSchema) {
		sc := &schema{required: map[string][]string{}, refs: map[string][2]string{}}
		schemas[name] = sc
		for prop, p := range s.Properties {
			if len(p.Annotations.Required) > 0 {
				sc.required[prop] = slices.Sorted(slices.Values(p.Annotations.Required))
			}
			if n, kind := ref(name, prop, p); n != "" {
				sc.refs[prop] = [2]string{n, kind}
			}
		}
	}
	for name, s := range doc.Schemas {
		flatten(name, s)
	}
	for field, drop := range relax {
		name, prop, _ := strings.Cut(field, ".")
		sc := schemas[name]
		if sc == nil || sc.required[prop] == nil {
			return fmt.Errorf("relax: %s is not a required field", field)
		}
		sc.required[prop] = slices.DeleteFunc(sc.required[prop], func(m string) bool { return slices.Contains(drop, m) })
		if len(sc.required[prop]) == 0 {
			delete(sc.required, prop)
		}
	}
	// Keep the schemas that (transitively) require a field.
	keep := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for name, sc := range schemas {
			if keep[name] {
				continue
			}
			ok := len(sc.required) > 0
			for _, r := range sc.refs {
				ok = ok || keep[r[0]]
			}
			if ok {
				keep[name], changed = true, true
			}
		}
	}

	fmt.Fprintf(b, "{Name: %q, Version: %q, Revision: %q, ServicePath: %q,\nMethods: []*RESTMethod{\n", doc.Name, doc.Version, doc.Revision, doc.ServicePath)
	for _, m := range methods {
		p := m.FlatPath
		if p == "" {
			p = m.Path
		}
		p = plusVar.ReplaceAllString(p, "{$1}")
		fmt.Fprintf(b, "{ID: %q, HTTPMethod: %q, Path: %q", m.ID, m.HTTPMethod, p)
		if m.Request != nil && m.Request.Ref != "" {
			fmt.Fprintf(b, ", Request: %q", m.Request.Ref)
			if m.Request.ParameterName != "" {
				fmt.Fprintf(b, ", RequestParam: %q", m.Request.ParameterName)
			}
		}
		var params []string
		for name, prm := range m.Parameters {
			if prm.Required && prm.Location == "query" {
				params = append(params, name)
			}
		}
		if len(params) > 0 {
			slices.Sort(params)
			fmt.Fprintf(b, ", Params: %#v", params)
		}
		b.WriteString("},\n")
	}
	b.WriteString("},\nSchemas: map[string]*Schema{\n")
	for _, name := range slices.Sorted(maps.Keys(keep)) {
		sc := schemas[name]
		fmt.Fprintf(b, "%q: {", name)
		if len(sc.required) > 0 {
			b.WriteString("Required: map[string][]string{")
			for _, prop := range slices.Sorted(maps.Keys(sc.required)) {
				fmt.Fprintf(b, "%q: %#v, ", prop, sc.required[prop])
			}
			b.WriteString("}, ")
		}
		var refs []string
		for _, prop := range slices.Sorted(maps.Keys(sc.refs)) {
			if r := sc.refs[prop]; keep[r[0]] {
				refs = append(refs, fmt.Sprintf("%q: {%q, %s}, ", prop, r[0], r[1]))
			}
		}
		if len(refs) > 0 {
			fmt.Fprintf(b, "Refs: map[string]Ref{%s}", strings.Join(refs, ""))
		}
		b.WriteString("},\n")
	}
	b.WriteString("}},\n")
	return nil
}
