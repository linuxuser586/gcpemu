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
	"time"

	"gopkg.in/yaml.v3"

	"github.com/linuxuser586/gcpemu/internal/apierr"
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
	deadline := time.Now().Add(s.env.Config.LRO("compute") + 10*time.Second)
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
