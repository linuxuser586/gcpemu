package instance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/fault"
)

// ApplySeed applies a seed file (FR-CORE-011). Top-level keys are service
// names; each section is handed to that service's Seeder.
func (in *Instance) ApplySeed(ctx context.Context, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc map[string]yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	base := filepath.Dir(path)
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

// EnvVars collects client environment variables from all services.
func (in *Instance) EnvVars() map[string]string {
	eps := in.Env.Endpoints.All()
	gw := eps["gateway"]
	out := map[string]string{
		"GCPEMU_GATEWAY":  gw,
		"GCPEMU_INSTANCE": in.Config.Instance,
	}
	for _, s := range in.services {
		if ev, ok := s.(emu.EnvVarer); ok {
			for k, v := range ev.EnvVars(gw, eps) {
				out[k] = v
			}
		}
	}
	in.hostNameEnvVars(out)
	return out
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
