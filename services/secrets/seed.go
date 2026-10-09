package secrets

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gopkg.in/yaml.v3"

	"github.com/linuxuser586/gcpemu/internal/locations"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// seedFile is the "secrets" section of a seed file (FR-CORE-011):
//
//	secrets:
//	  secrets:
//	    - project: my-project
//	      id: db-password
//	      location: us-central1   # optional: a regional secret
//	      replicas: [us-east1]    # optional, global only: user-managed replication (default automatic)
//	      labels: {team: web}
//	      versions:               # added in order until the secret has this many
//	        - data: s3cret
//	        - dataFile: certs/tls.key   # relative to the seed file
//	          state: DISABLED
type seedFile struct {
	Secrets []seedSecret `yaml:"secrets"`
}

type seedSecret struct {
	Project  string            `yaml:"project"`
	ID       string            `yaml:"id"`
	Location string            `yaml:"location"`
	Replicas []string          `yaml:"replicas"`
	Labels   map[string]string `yaml:"labels"`
	Versions []struct {
		Data     string `yaml:"data"`
		DataFile string `yaml:"dataFile"`
		State    string `yaml:"state"`
	} `yaml:"versions"`
}

// ApplySeed creates the seeded secrets (or updates their labels) and adds
// their missing versions. It is idempotent.
func (s *Service) ApplySeed(ctx context.Context, section *yaml.Node, baseDir string) error {
	var f seedFile
	if err := section.Decode(&f); err != nil {
		return err
	}
	for i, ss := range f.Secrets {
		if err := s.seedSecret(ss, baseDir); err != nil {
			return fmt.Errorf("secrets[%d] (%s): %w", i, ss.ID, err)
		}
	}
	return nil
}

func (s *Service) seedSecret(ss seedSecret, baseDir string) error {
	if err := s.env.EnsureProject(ss.Project); err != nil {
		return err
	}
	ref := secretRef{Project: ss.Project, Location: ss.Location, ID: ss.ID}
	if ref.Location != "" && !locations.IsRegion(ref.Location) {
		return fmt.Errorf("invalid location %q", ref.Location)
	}
	if !secretIDRe.MatchString(ref.ID) {
		return fmt.Errorf("invalid secret ID %q", ref.ID)
	}
	if err := validateLabels(ss.Labels); err != nil {
		return err
	}
	payloads := make([][]byte, len(ss.Versions))
	for i, v := range ss.Versions {
		payloads[i] = []byte(v.Data)
		if v.DataFile != "" {
			p := v.DataFile
			if !filepath.IsAbs(p) {
				p = filepath.Join(baseDir, p)
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			payloads[i] = b
		}
		if len(payloads[i]) > maxPayload {
			return fmt.Errorf("versions[%d]: payload exceeds 64KiB", i)
		}
	}
	now := s.env.Clock.Now()
	var have int64
	err := s.env.Store.Update(func(tx store.Tx) error {
		rec, err := getSecret(tx, ref)
		if err != nil {
			sec := &secretmanagerpb.Secret{Name: ref.name(), CreateTime: timestamppb.New(now)}
			if ref.Location == "" {
				sec.Replication = &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}
				if len(ss.Replicas) > 0 {
					um := &secretmanagerpb.Replication_UserManaged{}
					for _, l := range ss.Replicas {
						if !locations.IsRegion(l) {
							return fmt.Errorf("invalid replica location %q", l)
						}
						um.Replicas = append(um.Replicas, &secretmanagerpb.Replication_UserManaged_Replica{Location: l})
					}
					sec.Replication = &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_UserManaged_{UserManaged: um}}
				}
			}
			rec = &secretRec{pb: sec}
		}
		rec.pb.Labels = ss.Labels
		have = rec.Last
		return putSecret(tx, ref, rec, now)
	})
	if err != nil {
		return err
	}
	for i := int(have); i < len(payloads); i++ {
		_, v, err := s.addVersion(ref, payloads[i], false)
		if err != nil {
			return err
		}
		if st := ss.Versions[i].State; st != "" && st != "ENABLED" {
			if st != "DISABLED" {
				return fmt.Errorf("versions[%d]: state must be ENABLED or DISABLED", i)
			}
			err = s.env.Store.Update(func(tx store.Tx) error {
				n := versionNumber(v.GetName())
				vr, ok := getVersion(tx, ref, n)
				if !ok {
					return fmt.Errorf("version %d vanished", n)
				}
				vr.pb.State = secretmanagerpb.SecretVersion_DISABLED
				return putVersion(tx, ref, n, vr, now)
			})
			if err != nil {
				return err
			}
		}
	}
	return nil
}
