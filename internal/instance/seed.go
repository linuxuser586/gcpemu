package instance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/fault"
	"github.com/linuxuser586/gcpemu/internal/lbenv"
)

// ApplySeed applies a seed file (FR-CORE-011). Top-level keys are service
// names; each section is handed to that service's Seeder.
func (in *Instance) ApplySeed(ctx context.Context, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := in.ApplySeedData(ctx, b, filepath.Dir(path)); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// ApplySeedData applies a seed file's contents (an uploaded seed file);
// relative paths in it resolve against base.
func (in *Instance) ApplySeedData(ctx context.Context, b []byte, base string) error {
	var doc map[string]yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return err
	}
	if err := in.applyCoreSeed(doc); err != nil {
		return err
	}
	// Apply in service start order so dependencies (IAM, topics) exist first.
	seen := map[string]bool{}
	for _, s := range in.services {
		node, ok := doc[s.Name()]
		if !ok {
			continue
		}
		seen[s.Name()] = true
		sd, ok := s.(emu.Seeder)
		if !ok {
			return fmt.Errorf("service %q does not accept seed data", s.Name())
		}
		if err := sd.ApplySeed(ctx, &node, base); err != nil {
			return fmt.Errorf("%s: %w", s.Name(), err)
		}
	}
	for k := range doc {
		if !seen[k] && !coreSeedKeys[k] {
			return fmt.Errorf("seed section %q: service not running", k)
		}
	}
	return nil
}

// EnvVars collects client environment variables from all services, and
// the Load balancing frontends of the Linked Instances it delegates to.
func (in *Instance) EnvVars() map[string]string {
	vars, _ := in.envVars(context.Background())
	return vars
}

// envVars is EnvVars, with notes on the Linked Instances' load balancing
// endpoints that got no variable, which `gcpemu env` prints on stderr.
func (in *Instance) envVars(ctx context.Context) (map[string]string, []string) {
	eps := in.Env.Endpoints.All()
	gw := eps["gateway"]
	out := map[string]string{
		"GCPEMU_GATEWAY":  gw,
		"GCPEMU_INSTANCE": in.Config.Instance,
		// gcloud and the client libraries send an API without an endpoint
		// override to the Invalid universe, and refuse credentials stored
		// for Google.
		"CLOUDSDK_CORE_UNIVERSE_DOMAIN": emu.InvalidUniverse,
		"GOOGLE_CLOUD_UNIVERSE_DOMAIN":  emu.InvalidUniverse,
		// Go clients dial a hard-coded *.mtls.googleapis.com, whatever the
		// universe, when the mode is "always" or a client certificate is
		// found (#243). No endpoint override is an mTLS endpoint.
		"GOOGLE_API_USE_MTLS_ENDPOINT": "never",
	}
	for _, s := range in.services {
		if ev, ok := s.(emu.EnvVarer); ok {
			for k, v := range ev.EnvVars(gw, eps) {
				out[k] = v
			}
		}
	}
	// Variables are derived over the Linked Instances' endpoints too, so
	// that uniqueness and withholding span Instances.
	linked, notes, unreachable := in.linkedFrontends(ctx)
	maps.Copy(linked, eps)
	lbVars, withheld := lbenv.Vars(linked)
	if unreachable {
		withoutShort(lbVars, linked)
	}
	maps.Copy(out, lbVars)
	notes = append(notes, withheldNotes(withheld)...)
	in.hostNameEnvVars(out)
	in.externalEnvVars(out, gw)
	if in.adminToken != "" {
		out["GCPEMU_ADMIN_TOKEN"] = in.adminToken
	}
	return out, notes
}

var coreSeedKeys = map[string]bool{"projects": true, "faults": true}

// applyCoreSeed handles the sections owned by the core: projects (list of
// IDs) and faults (list of fault rules, FR-CORE-060).
func (in *Instance) applyCoreSeed(doc map[string]yaml.Node) error {
	if n, ok := doc["projects"]; ok {
		var ids []string
		if err := n.Decode(&ids); err != nil {
			return fmt.Errorf("projects: %w", err)
		}
		for _, id := range ids {
			if err := in.Env.DeclareProject(id); err != nil {
				return fmt.Errorf("projects: %w", err)
			}
		}
	}
	if n, ok := doc["faults"]; ok {
		var rules []fault.Rule
		if err := n.Decode(&rules); err != nil {
			return fmt.Errorf("faults: %w", err)
		}
		existing := map[string]bool{}
		for _, r := range in.Faults().List() {
			existing[r.ID] = true
		}
		for _, r := range rules {
			if r.ID != "" && existing[r.ID] {
				continue // idempotent re-seed
			}
			if _, err := in.Faults().Add(r); err != nil {
				return fmt.Errorf("faults: %w", err)
			}
		}
	}
	return nil
}

// ExportSeed writes the Instance's resources as a seed file (FR-CORE-013):
// its Projects, every SeedExporter's section in Service start order, and
// its Fault rules. Applying it to an empty Instance recreates them.
func (in *Instance) ExportSeed(ctx context.Context) ([]byte, error) {
	doc := &yaml.Node{Kind: yaml.MappingNode}
	add := func(key string, v any) error {
		var n yaml.Node
		if err := n.Encode(v); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		doc.Content = append(doc.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, &n)
		return nil
	}
	if ids := in.Env.Projects(); len(ids) > 0 {
		if err := add("projects", ids); err != nil {
			return nil, err
		}
	}
	for _, s := range in.services {
		ex, ok := s.(emu.SeedExporter)
		if !ok {
			continue
		}
		section, err := ex.ExportSeed(ctx)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.Name(), err)
		}
		if section == nil {
			continue
		}
		if err := add(s.Name(), section); err != nil {
			return nil, err
		}
	}
	if rules := in.Faults().List(); len(rules) > 0 {
		// Through JSON, whose tags leave out unset fields.
		var out []map[string]any
		b, err := json.Marshal(rules)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, &out); err != nil {
			return nil, err
		}
		for _, r := range out {
			delete(r, "fired")
		}
		if err := add("faults", out); err != nil {
			return nil, err
		}
	}
	if len(doc.Content) == 0 {
		return []byte("{}\n"), nil
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), enc.Close()
}
