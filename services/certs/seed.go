package certs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/linuxuser586/gcpemu/internal/store"
)

// ApplySeed implements emu.Seeder (FR-CORE-011) for the "certs" section.
// Each list holds resources in their REST JSON shape plus addressing keys
// (name, or project/location/id; certificateMap for entries). A key
// ending in "File" is replaced by the contents of that file, relative to
// the seed file, e.g. pemCertificateFile → pemCertificate. References
// given as a bare ID are expanded within the resource's project and
// location. Seeding is idempotent: existing resources are updated.
//
//	certs:
//	  trustConfigs:
//	    - {project: p, id: backends, trustStores: [{trustAnchors: [{pemCertificateFile: ca.pem}]}]}
//	  dnsAuthorizations:
//	    - {project: p, id: example, domain: example.test}
//	  certificates:
//	    - project: p
//	      id: web
//	      selfManaged: {pemCertificateFile: web.pem, pemPrivateKeyFile: web-key.pem}
//	    - {project: p, id: managed, managed: {domains: [app.example.test], dnsAuthorizations: [example]}}
//	    - {project: p, id: lb-client, scope: CLIENT_AUTH, selfManaged: {...}}
//	  certificateMaps:
//	    - {project: p, id: web}
//	  certificateMapEntries:
//	    - {project: p, certificateMap: web, id: app, hostname: app.example.test, certificates: [web]}
//	  backendAuthenticationConfigs:
//	    - {project: p, id: mtls, clientCertificate: lb-client, trustConfig: backends}
//	  serverTlsPolicies:
//	    - {project: p, id: frontend, mtlsPolicy: {clientValidationMode: REJECT_INVALID, clientValidationTrustConfig: backends}}
func (s *Service) ApplySeed(ctx context.Context, section *yaml.Node, baseDir string) error {
	var f map[string][]map[string]any
	if err := section.Decode(&f); err != nil {
		return err
	}
	order := []*kind{kTrust, kIssuance, kDNSAuth, kCert, kMap, kEntry, kBAC, kServerTLS, kClientTLS}
	known := map[string]bool{}
	for _, k := range order {
		known[k.coll] = true
		for i, item := range f[k.coll] {
			if err := s.seedOne(ctx, k, item, baseDir); err != nil {
				return fmt.Errorf("certs.%s[%d]: %w", k.coll, i, err)
			}
		}
	}
	for key := range f {
		if !known[key] {
			return fmt.Errorf("certs: unknown seed list %q", key)
		}
	}
	s.kick()
	return nil
}

func (s *Service) seedOne(ctx context.Context, k *kind, item map[string]any, baseDir string) error {
	meta := func(key string) string {
		v, _ := item[key].(string)
		delete(item, key)
		return v
	}
	name, project, loc, id, parent := meta("name"), meta("project"), meta("location"), meta("id"), meta("certificateMap")
	if loc == "" {
		loc = "global"
	}
	var n resName
	if name != "" {
		var err error
		if n, err = parseName(k, name); err != nil {
			return err
		}
	} else {
		n = resName{Project: project, Location: loc, Parent: parent, ID: id}
		if _, err := parseName(k, n.name(k)); err != nil {
			return err
		}
	}
	if err := validID(k, n.ID); err != nil {
		return err
	}
	if err := s.env.EnsureProject(n.Project); err != nil {
		return err
	}
	if err := checkLocation(k, n.Location, false); err != nil {
		return err
	}
	v, err := readFiles(item, baseDir)
	if err != nil {
		return err
	}
	item = v.(map[string]any)
	expand := func(target *kind, ref string) string {
		if ref == "" || strings.Contains(ref, "/") {
			return ref
		}
		return "projects/" + n.Project + "/locations/" + n.Location + "/" + target.coll + "/" + ref
	}
	switch k {
	case kEntry:
		if l, ok := item["certificates"].([]any); ok {
			for i, c := range l {
				l[i] = expand(kCert, fmt.Sprint(c))
			}
		}
	case kCert:
		if m, ok := item["managed"].(map[string]any); ok {
			if l, ok := m["dnsAuthorizations"].([]any); ok {
				for i, c := range l {
					l[i] = expand(kDNSAuth, fmt.Sprint(c))
				}
			}
			if ic, ok := m["issuanceConfig"].(string); ok {
				m["issuanceConfig"] = expand(kIssuance, ic)
			}
		}
	case kBAC:
		for key, target := range map[string]*kind{"clientCertificate": kCert, "trustConfig": kTrust} {
			if r, ok := item[key].(string); ok {
				item[key] = expand(target, r)
			}
		}
	case kServerTLS:
		if m, ok := item["mtlsPolicy"].(map[string]any); ok {
			if r, ok := m["clientValidationTrustConfig"].(string); ok {
				m["clientValidationTrustConfig"] = expand(kTrust, r)
			}
		}
	}
	body, err := json.Marshal(item)
	if err != nil {
		return err
	}
	in, err := decodeREST(k, body)
	if err != nil {
		return err
	}
	return s.upsert(ctx, k, n, in)
}

// readFiles replaces every "<key>File" entry with "<key>" set to the
// file's contents.
func readFiles(v any, baseDir string) (any, error) {
	switch x := v.(type) {
	case map[string]any:
		for key, val := range x {
			if base, ok := strings.CutSuffix(key, "File"); ok && base != "" {
				p, _ := val.(string)
				if p == "" {
					return nil, fmt.Errorf("%s must be a file path", key)
				}
				if !filepath.IsAbs(p) {
					p = filepath.Join(baseDir, p)
				}
				b, err := os.ReadFile(p)
				if err != nil {
					return nil, err
				}
				delete(x, key)
				x[base] = string(b)
				continue
			}
			nv, err := readFiles(val, baseDir)
			if err != nil {
				return nil, err
			}
			x[key] = nv
		}
	case []any:
		for i, e := range x {
			nv, err := readFiles(e, baseDir)
			if err != nil {
				return nil, err
			}
			x[i] = nv
		}
	}
	return v, nil
}

// upsert creates or updates a resource synchronously, without IAM checks
// or operations (seeding).
func (s *Service) upsert(ctx context.Context, k *kind, n resName, in obj) error {
	name := n.name(k)
	var old obj
	_ = s.env.Store.View(func(tx store.Tx) error { old = load(tx, name); return nil })
	res := clone(in)
	for _, f := range append([]string{"name", "createTime", "updateTime", "etag"}, k.output...) {
		delete(res, f)
	}
	if old != nil {
		merged := clone(old)
		for key, v := range res {
			merged[key] = v
		}
		res = merged
	}
	normalize(k, res)
	commit, err := s.prepare(ctx, k, n, res, old)
	if err != nil {
		return err
	}
	for _, f := range k.inputOnly {
		delete(res, f)
	}
	res["name"] = name
	err = s.env.Store.Update(func(tx store.Tx) error {
		now := s.now()
		res["updateTime"] = now
		if old == nil {
			res["createTime"] = now
		}
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
	if err == nil {
		s.afterWrite(k, name)
	}
	return err
}
