package lb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
	"github.com/linuxuser586/gcpemu/services/compute"
)

var (
	_ emu.Seeder       = (*Service)(nil)
	_ emu.SeedExporter = (*Service)(nil)
)

// Seed file section (FR-CORE-011). Entries use the compute v1 JSON field
// names, are replayed through the REST handlers (same validation and
// defaults) in dependency order, and are skipped when they already exist,
// so applying a seed is idempotent. An entry with region is regional.
// SSL certificates may name PEM files relative to the seed file.
//
//	lb:
//	  project: my-project
//	  healthChecks:
//	    - {name: hc, type: HTTP, httpHealthCheck: {portSpecification: USE_SERVING_PORT, requestPath: /healthz}}
//	  sslCertificates:
//	    - {name: app, certificateFile: certs/app.pem, privateKeyFile: certs/app-key.pem}
//	    - {name: managed, type: MANAGED, managed: {domains: [app.example.test]}}
//	  sslPolicies: [{name: modern, profile: MODERN, minTlsVersion: TLS_1_2}]
//	  backendServices:
//	    - name: api
//	      loadBalancingScheme: EXTERNAL_MANAGED
//	      healthChecks: [global/healthChecks/hc]
//	      backends: [{group: zones/us-central1-a/networkEndpointGroups/api-neg}]
//	  backendBuckets: [{name: static, bucketName: my-assets, enableCdn: true}]
//	  urlMaps:
//	    - name: web
//	      defaultService: global/backendBuckets/static
//	      pathMatchers: [...]
//	  targetHttpsProxies: [{name: web, urlMap: global/urlMaps/web, sslCertificates: [global/sslCertificates/app]}]
//	  forwardingRules:
//	    - {name: web, loadBalancingScheme: EXTERNAL_MANAGED, portRange: "443", target: global/targetHttpsProxies/web}

// ApplySeed implements emu.Seeder.
func (s *Service) ApplySeed(ctx context.Context, section *yaml.Node, baseDir string) error {
	var sec map[string]any
	if err := section.Decode(&sec); err != nil {
		return err
	}
	defProject, _ := sec["project"].(string)
	known := map[string]bool{"project": true}
	for _, k := range allKinds() {
		known[k.coll] = true
	}
	for key := range sec {
		if !known[key] {
			return fmt.Errorf("lb: unknown seed key %q", key)
		}
	}
	for _, k := range allKinds() {
		raw, _ := sec[k.coll].([]any)
		for i, it := range raw {
			item, ok := it.(map[string]any)
			if !ok {
				return fmt.Errorf("lb.%s[%d]: not a mapping", k.coll, i)
			}
			if err := s.seedOne(ctx, k, item, defProject, baseDir); err != nil {
				return fmt.Errorf("lb.%s[%d] %v: %w", k.coll, i, item["name"], err)
			}
		}
	}
	return nil
}

func (s *Service) seedOne(ctx context.Context, k *kind, item map[string]any, defProject, baseDir string) error {
	p, _ := item["project"].(string)
	if p == "" {
		p = defProject
	}
	if p == "" {
		return fmt.Errorf("project is required")
	}
	region, _ := item["region"].(string)
	name, _ := item["name"].(string)
	body := map[string]any{}
	for kk, v := range item {
		switch kk {
		case "project", "region":
		case "certificateFile", "privateKeyFile":
			path, _ := v.(string)
			if !filepath.IsAbs(path) {
				path = filepath.Join(baseDir, path)
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if kk == "certificateFile" {
				body["certificate"] = string(b)
			} else {
				body["privateKey"] = string(b)
			}
		default:
			body[kk] = v
		}
	}
	sc := scope{project: p, region: region}
	if sc.region != "" && k.rperm == "" || sc.region == "" && k.gperm == "" {
		return fmt.Errorf("%s does not support this scope", k.coll)
	}
	path := sc.coll(k.coll) + "/" + name
	if _, exists := s.load(k, path); exists {
		return nil
	}
	b, _ := json.Marshal(body)
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/compute/v1/"+sc.coll(k.coll), bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.local.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		var env struct {
			Error struct{ Message string } `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		return apierr.InvalidArgument("%s", env.Error.Message)
	}
	var op struct {
		Status string
		Error  *struct{ Errors []struct{ Message string } }
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &op)
	if op.Error != nil && len(op.Error.Errors) > 0 {
		return fmt.Errorf("%s", op.Error.Errors[0].Message)
	}
	// With --lro-latency the insert completes later.
	deadline := time.Now().Add(s.env.Config.LRO("lb") + 10*time.Second)
	for {
		if _, ok := s.load(k, path); ok {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("insert of %s did not complete", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// seedItem is one exported entry: its addressing keys, then its compute
// v1 JSON fields.
type seedItem struct {
	Name    string         `yaml:"name"`
	Project string         `yaml:"project,omitempty"`
	Region  string         `yaml:"region,omitempty"`
	Fields  map[string]any `yaml:",inline"`
}

// serverFields are output-only or server-assigned top-level fields, left
// out of the export.
var serverFields = []string{"kind", "id", "creationTimestamp", "selfLink", "region", "fingerprint", "labelFingerprint"}

// ExportSeed implements emu.SeedExporter (FR-CORE-013). The section is a
// yaml.Node so that its lists keep the seed (dependency) order. SSL
// certificates are written with their private key inline; managed ones
// are provisioned again when applied. A forwarding rule's IP is written
// as the reserved address holding it, and left out when ephemeral. Signed
// URL keys have no seed entries and are not exported.
func (s *Service) ExportSeed(ctx context.Context) (any, error) {
	type stored struct {
		k    *kind
		path string
		obj  any
	}
	var all []stored
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, k := range allKinds() {
			tx.Scan(k.ns(), "projects/", func(path string, b []byte) bool {
				obj := k.newObj()
				if json.Unmarshal(b, obj) == nil {
					all = append(all, stored{k, path, obj})
				}
				return true
			})
		}
		return nil
	})
	if len(all) == 0 {
		return nil, nil
	}
	lists := map[*kind][]seedItem{}
	projects := map[string]bool{}
	for _, st := range all {
		it, err := s.exportItem(ctx, st.path, st.obj)
		if err != nil {
			return nil, err
		}
		projects[it.Project] = true
		lists[st.k] = append(lists[st.k], it)
	}
	sec := &yaml.Node{Kind: yaml.MappingNode}
	add := func(key string, v any) error {
		var n yaml.Node
		if err := n.Encode(v); err != nil {
			return err
		}
		sec.Content = append(sec.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, &n)
		return nil
	}
	if len(projects) == 1 {
		for p := range projects {
			if err := add("project", p); err != nil {
				return nil, err
			}
		}
	}
	for _, k := range allKinds() {
		l := lists[k]
		if len(l) == 0 {
			continue
		}
		sort.SliceStable(l, func(i, j int) bool {
			if l[i].Project != l[j].Project {
				return l[i].Project < l[j].Project
			}
			if l[i].Name != l[j].Name {
				return l[i].Name < l[j].Name
			}
			return l[i].Region < l[j].Region
		})
		if len(projects) == 1 {
			for i := range l {
				l[i].Project = ""
			}
		}
		if err := add(k.coll, l); err != nil {
			return nil, err
		}
	}
	return sec, nil
}

// exportItem converts a stored resource of kind k into its seed entry.
func (s *Service) exportItem(ctx context.Context, path string, obj any) (seedItem, error) {
	sc := scopeOfPath(path)
	switch o := obj.(type) {
	case *computev1.SslCertificate:
		if o.Type == "MANAGED" {
			o.Certificate, o.ExpireTime, o.SubjectAlternativeNames = "", "", nil
			if o.Managed != nil {
				o.Managed.Status, o.Managed.DomainStatus = "", nil
			}
		} else {
			if err := s.env.Store.View(func(tx store.Tx) error { return store.GetJSON(tx, nsSSLKeys, path, &o.PrivateKey) }); err != nil {
				return seedItem{}, fmt.Errorf("%s: private key: %w", path, err)
			}
			o.ExpireTime, o.SubjectAlternativeNames = "", nil
		}
	case *computev1.ForwardingRule:
		ip := o.IPAddress
		o.IPAddress = ""
		if ap, ok := s.cmp.AddressByIP(ctx, sc.project, sc.region, ip); ok {
			o.IPAddress = compute.SelfLink(ap)
		}
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return seedItem{}, err
	}
	// Numbers are decoded exactly (integers stay integers in YAML).
	m := map[string]any{}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return seedItem{}, err
	}
	for _, f := range serverFields {
		delete(m, f)
	}
	for _, f := range seedOutput[collOf(path)] {
		dropPath(m, f, nil)
	}
	for f, def := range seedDefaults[collOf(path)] {
		dropPath(m, f, def)
	}
	for k, v := range m {
		if sub, ok := v.(map[string]any); ok && len(sub) == 0 {
			delete(m, k)
		}
	}
	name, _ := m["name"].(string)
	delete(m, "name")
	return seedItem{Name: name, Project: sc.project, Region: sc.region, Fields: shortenRefs(m, sc.project).(map[string]any)}, nil
}

// seedOutput lists the output-only fields (dotted paths) of each
// collection, and fields the API derives from others.
var seedOutput = map[string][]string{
	"sslCertificates": {"type"},
	"sslPolicies":     {"enabledFeatures"},
	"backendServices": {"cdnPolicy.signedUrlKeyNames"},
	"backendBuckets":  {"cdnPolicy.signedUrlKeyNames"},
}

// seedDefaults are the values the API fills in for absent fields (dotted
// paths), per collection.
var seedDefaults = map[string]map[string]any{
	"healthChecks": {
		"checkIntervalSec": 5, "timeoutSec": 5, "healthyThreshold": 2, "unhealthyThreshold": 2,
		"httpHealthCheck.proxyHeader": "NONE", "httpsHealthCheck.proxyHeader": "NONE", "http2HealthCheck.proxyHeader": "NONE",
		"tcpHealthCheck.proxyHeader": "NONE", "sslHealthCheck.proxyHeader": "NONE",
	},
	"backendServices": {
		"protocol": "HTTP", "loadBalancingScheme": "EXTERNAL", "timeoutSec": 30, "port": 80, "portName": "http", "sessionAffinity": "NONE",
	},
	"sslPolicies":        {"profile": "COMPATIBLE", "minTlsVersion": "TLS_1_0"},
	"securityPolicies":   {"type": "CLOUD_ARMOR"},
	"targetHttpsProxies": {"quicOverride": "NONE"},
	"targetSslProxies":   {"proxyHeader": "NONE"},
	"targetTcpProxies":   {"proxyHeader": "NONE"},
	"forwardingRules":    {"IPProtocol": "TCP", "loadBalancingScheme": "EXTERNAL", "networkTier": "PREMIUM"},
}

// dropPath deletes the field at a dotted path of m, when it equals def
// (always when def is nil).
func dropPath(m map[string]any, path string, def any) {
	parts := strings.Split(path, ".")
	for _, p := range parts[:len(parts)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			return
		}
		m = next
	}
	last := parts[len(parts)-1]
	if v, ok := m[last]; ok && (def == nil || fmt.Sprint(v) == fmt.Sprint(def)) {
		delete(m, last)
	}
}

// shortenRefs rewrites the compute URLs in v within project proj to
// partial paths ("global/backendServices/api"), drops nested kinds and
// converts numbers to int64 or float64.
func shortenRefs(v any, proj string) any {
	switch x := v.(type) {
	case map[string]any:
		delete(x, "kind") // output only, also on nested types
		for k, e := range x {
			x[k] = shortenRefs(e, proj)
		}
	case []any:
		for i, e := range x {
			x[i] = shortenRefs(e, proj)
		}
	case string:
		if p, ok := strings.CutPrefix(x, compute.SelfLink("projects/"+proj)+"/"); ok {
			return p
		}
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		f, _ := x.Float64()
		return f
	}
	return v
}
