package secrets

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"go.yaml.in/yaml/v3"
	"google.golang.org/protobuf/types/known/timestamppb"

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
//	        - state: DESTROYED          # keeps the version number, no payload
//
// A payload that is not UTF-8 is written as a !!binary (base64) data
// scalar.
type seedFile struct {
	Secrets []seedSecret `yaml:"secrets"`
}

type seedSecret struct {
	Project  string            `yaml:"project"`
	ID       string            `yaml:"id"`
	Location string            `yaml:"location,omitempty"`
	Replicas []string          `yaml:"replicas,omitempty"`
	Labels   map[string]string `yaml:"labels,omitempty"`
	Versions []seedVersion     `yaml:"versions,omitempty"`
}

type seedVersion struct {
	Data     string `yaml:"data,omitempty"`
	DataFile string `yaml:"dataFile,omitempty"`
	State    string `yaml:"state,omitempty"`
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
			if st != "DISABLED" && st != "DESTROYED" {
				return fmt.Errorf("versions[%d]: state must be ENABLED, DISABLED or DESTROYED", i)
			}
			err = s.env.Store.Update(func(tx store.Tx) error {
				n := versionNumber(v.GetName())
				vr, ok := getVersion(tx, ref, n)
				if !ok {
					return fmt.Errorf("version %d vanished", n)
				}
				vr.pb.State = secretmanagerpb.SecretVersion_DISABLED
				if st == "DESTROYED" {
					destroy(vr, now)
				}
				return putVersion(tx, ref, n, vr, now)
			})
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// ExportSeed implements emu.SeedExporter: every secret with its
// replication, labels and versions in order, payloads inline. Version
// aliases, annotations, topics, rotation, expiry, CMEK, destroy TTLs and
// pending scheduled destructions have no seed form and are not exported.
func (s *Service) ExportSeed(ctx context.Context) (any, error) {
	var f seedFile
	err := s.env.Store.View(func(tx store.Tx) error {
		var refs []secretRef
		tx.Scan(nsSecrets, "", func(k string, _ []byte) bool { refs = append(refs, refFromKey(k)); return true })
		for _, ref := range refs {
			rec, err := getSecret(tx, ref)
			if err != nil {
				return err
			}
			ss := seedSecret{Project: ref.Project, ID: ref.ID, Location: ref.Location, Labels: rec.pb.GetLabels()}
			for _, r := range rec.pb.GetReplication().GetUserManaged().GetReplicas() {
				ss.Replicas = append(ss.Replicas, r.GetLocation())
			}
			// Versions are numbered from 1 in the order they are added.
			for n := int64(1); n <= rec.Last; n++ {
				var sv seedVersion
				switch v, ok := getVersion(tx, ref, n); {
				case !ok || v.pb.GetState() == secretmanagerpb.SecretVersion_DESTROYED:
					sv.State = "DESTROYED"
				default:
					sv.Data = string(v.Payload)
					if v.pb.GetState() == secretmanagerpb.SecretVersion_DISABLED {
						sv.State = "DISABLED"
					}
				}
				ss.Versions = append(ss.Versions, sv)
			}
			f.Secrets = append(f.Secrets, ss)
		}
		return nil
	})
	if err != nil || len(f.Secrets) == 0 {
		return nil, err
	}
	slices.SortFunc(f.Secrets, func(a, b seedSecret) int {
		return cmp.Or(cmp.Compare(a.Project, b.Project), cmp.Compare(a.ID, b.ID), cmp.Compare(a.Location, b.Location))
	})
	return &f, nil
}
