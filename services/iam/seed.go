package iam

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	iamv1 "google.golang.org/api/iam/v1"
	"google.golang.org/grpc/codes"
	"gopkg.in/yaml.v3"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// seedSection is the "iam" section of a seed file (FR-CORE-011):
//
//	iam:
//	  metadata: {project: my-proj, zone: europe-west1-b, serviceAccount: app@my-proj.iam.gserviceaccount.com}
//	  serviceAccounts:
//	    - {project: my-proj, accountId: app, displayName: App, keyFile: keys/app.json}
//	  customRoles:
//	    - {project: my-proj, roleId: bucketLister, title: Bucket lister, permissions: [storage.buckets.list]}
//	  bindings:
//	    - {project: my-proj, role: roles/storage.objectAdmin, members: [serviceAccount:app@my-proj.iam.gserviceaccount.com]}
//	    - {resource: //storage.googleapis.com/projects/_/buckets/b, role: roles/storage.objectViewer, members: [allUsers]}
//
// keyFile (relative to the seed file) receives a JSON key the first time;
// an existing file whose key is still registered is left alone.
type seedSection struct {
	Metadata *struct {
		Project        string `yaml:"project"`
		Zone           string `yaml:"zone"`
		ServiceAccount string `yaml:"serviceAccount"`
	} `yaml:"metadata"`
	ServiceAccounts []struct {
		Project     string `yaml:"project"`
		AccountID   string `yaml:"accountId"`
		DisplayName string `yaml:"displayName"`
		Description string `yaml:"description"`
		KeyFile     string `yaml:"keyFile"`
	} `yaml:"serviceAccounts"`
	CustomRoles []struct {
		Project     string   `yaml:"project"`
		RoleID      string   `yaml:"roleId"`
		Title       string   `yaml:"title"`
		Description string   `yaml:"description"`
		Stage       string   `yaml:"stage"`
		Permissions []string `yaml:"permissions"`
	} `yaml:"customRoles"`
	Bindings []struct {
		Project  string   `yaml:"project"`
		Resource string   `yaml:"resource"`
		Role     string   `yaml:"role"`
		Members  []string `yaml:"members"`
	} `yaml:"bindings"`
}

// ApplySeed implements emu.Seeder; it is idempotent.
func (s *Service) ApplySeed(ctx context.Context, section *yaml.Node, baseDir string) error {
	var sec seedSection
	if err := section.Decode(&sec); err != nil {
		return err
	}
	if m := sec.Metadata; m != nil {
		s.mu.Lock()
		cur := s.meta
		if cur.Project == "" {
			cur = s.metadataDefaults()
		}
		if m.Project != "" && m.Project != cur.Project {
			cur.Project = m.Project
			if s.env.Config.MetadataServiceAccount == "" {
				cur.Email = defaultComputeEmail(m.Project)
			}
		}
		if m.Zone != "" {
			cur.Zone = m.Zone
		}
		if m.ServiceAccount != "" {
			cur.Email, cur.Explicit = m.ServiceAccount, true
		}
		s.meta = cur
		s.mu.Unlock()
	}
	for _, a := range sec.ServiceAccounts {
		if a.Project == "" || a.AccountID == "" {
			return errors.New("serviceAccounts: project and accountId are required")
		}
		if err := s.env.EnsureProject(a.Project); err != nil {
			return err
		}
		sa, err := s.newAccount(a.Project, a.AccountID, &iamv1.ServiceAccount{DisplayName: a.DisplayName, Description: a.Description})
		if err != nil && apierr.From(err).Code != codes.AlreadyExists {
			return fmt.Errorf("service account %s: %w", a.AccountID, err)
		}
		if sa == nil || err != nil {
			_ = s.env.Store.View(func(tx store.Tx) error { sa, _ = s.getAccount(tx, saEmail(a.Project, a.AccountID)); return nil })
		}
		if a.KeyFile != "" {
			if err := s.seedKeyFile(sa, resolvePath(baseDir, a.KeyFile)); err != nil {
				return fmt.Errorf("service account %s key: %w", a.AccountID, err)
			}
		}
	}
	for _, r := range sec.CustomRoles {
		if err := s.env.EnsureProject(r.Project); err != nil {
			return err
		}
		_, err := s.newCustomRole(r.Project, r.RoleID, &iamv1.Role{Title: r.Title, Description: r.Description, Stage: r.Stage, IncludedPermissions: r.Permissions})
		if err != nil && apierr.From(err).Code != codes.AlreadyExists {
			return fmt.Errorf("custom role %s: %w", r.RoleID, err)
		}
	}
	for _, b := range sec.Bindings {
		res := b.Resource
		if res == "" {
			if b.Project == "" {
				return errors.New("bindings: project or resource is required")
			}
			if err := s.env.EnsureProject(b.Project); err != nil {
				return err
			}
			res = projectResource(b.Project)
		}
		if err := s.addBinding(res, b.Role, b.Members); err != nil {
			return fmt.Errorf("binding %s on %s: %w", b.Role, res, err)
		}
	}
	return nil
}

func resolvePath(base, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

// seedKeyFile writes a new JSON key for sa unless path already holds a key
// that is still registered.
func (s *Service) seedKeyFile(sa *iamv1.ServiceAccount, path string) error {
	if b, err := os.ReadFile(path); err == nil {
		var kf keyFile
		if json.Unmarshal(b, &kf) == nil && kf.ClientEmail == sa.Email {
			var exists bool
			_ = s.env.Store.View(func(tx store.Tx) error {
				exists = store.Exists(tx, nsKeys, keyKey(sa.Email, kf.PrivateKeyID))
				return nil
			})
			if exists {
				return nil
			}
		}
	}
	_, kf, err := s.createKey(sa)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, kf, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// addBinding adds members to role on resource (merging, idempotent).
func (s *Service) addBinding(resource, role string, members []string) error {
	return s.env.Store.Update(func(tx store.Tx) error {
		p := loadPolicy(tx, resource)
		found := false
		for _, b := range p.Bindings {
			if b.Role == role && b.Condition == nil {
				found = true
				for _, m := range members {
					if !slices.Contains(b.Members, m) {
						b.Members = append(b.Members, m)
					}
				}
			}
		}
		if !found {
			p.Bindings = append(p.Bindings, &iamv1.Binding{Role: role, Members: members})
		}
		p.Etag = ""
		_, err := s.storePolicy(tx, resource, p, "")
		return err
	})
}

// EnvVars implements emu.EnvVarer (FR-CORE-004).
func (s *Service) EnvVars(gateway string, endpoints map[string]string) map[string]string {
	out := map[string]string{
		"CLOUDSDK_API_ENDPOINT_OVERRIDES_IAM":                  "http://" + gateway + "/iam/",
		"CLOUDSDK_API_ENDPOINT_OVERRIDES_IAMCREDENTIALS":       "http://" + gateway + "/iamcredentials/",
		"CLOUDSDK_API_ENDPOINT_OVERRIDES_CLOUDRESOURCEMANAGER": "http://" + gateway + "/cloudresourcemanager/",
		"CLOUDSDK_API_ENDPOINT_OVERRIDES_STS":                  "http://" + gateway + "/sts/",
		"CLOUDSDK_AUTH_TOKEN_HOST":                             "http://" + gateway + "/oauth2/token",
	}
	if md := endpoints["metadata"]; md != "" {
		out["GCE_METADATA_HOST"] = md
		out["GCE_METADATA_IP"] = md
	}
	return out
}
