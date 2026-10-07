package ar

import (
	"context"
	"fmt"
	"strings"

	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gopkg.in/yaml.v3"

	"github.com/linuxuser586/gcpemu/internal/store"
)

// seedFile is the "ar" section of a seed file (FR-CORE-011):
//
//	ar:
//	  repositories:
//	    - project: my-project
//	      location: us-central1
//	      id: images            # or name: projects/P/locations/L/repositories/R
//	      format: DOCKER        # default DOCKER
//	      description: App images
//	      labels: {team: web}
//	      immutableTags: false
type seedFile struct {
	Repositories []seedRepo `yaml:"repositories"`
}

type seedRepo struct {
	Name          string            `yaml:"name"`
	Project       string            `yaml:"project"`
	Location      string            `yaml:"location"`
	ID            string            `yaml:"id"`
	Format        string            `yaml:"format"`
	Mode          string            `yaml:"mode"`
	Description   string            `yaml:"description"`
	Labels        map[string]string `yaml:"labels"`
	ImmutableTags bool              `yaml:"immutableTags"`
}

// ApplySeed creates (or updates the description and labels of) the seeded
// repositories. It is idempotent.
func (s *Service) ApplySeed(ctx context.Context, section *yaml.Node, baseDir string) error {
	var f seedFile
	if err := section.Decode(&f); err != nil {
		return err
	}
	for i, sr := range f.Repositories {
		ref := repoRef{sr.Project, sr.Location, sr.ID}
		if sr.Name != "" {
			r, rest, err := parseRepoName(sr.Name)
			if err != nil || len(rest) != 0 {
				return fmt.Errorf("repositories[%d]: invalid name %q", i, sr.Name)
			}
			ref = r
		}
		if err := s.seedRepo(ref, sr); err != nil {
			return fmt.Errorf("repositories[%d] (%s): %w", i, ref.name(), err)
		}
	}
	return nil
}

func (s *Service) seedRepo(ref repoRef, sr seedRepo) error {
	if err := s.env.EnsureProject(ref.Project); err != nil {
		return err
	}
	if err := checkLocation(ref.Location); err != nil {
		return err
	}
	if !repoIDRe.MatchString(ref.Repo) {
		return fmt.Errorf("invalid repository ID %q", ref.Repo)
	}
	format := strings.ToUpper(sr.Format)
	if format == "" {
		format = "DOCKER"
	}
	mode := strings.ToUpper(sr.Mode)
	if mode == "" {
		mode = "STANDARD_REPOSITORY"
	}
	in := &artifactregistrypb.Repository{
		Format:      artifactregistrypb.Repository_Format(artifactregistrypb.Repository_Format_value[format]),
		Mode:        artifactregistrypb.Repository_Mode(artifactregistrypb.Repository_Mode_value[mode]),
		Description: sr.Description,
		Labels:      sr.Labels,
	}
	if sr.ImmutableTags {
		in.FormatConfig = &artifactregistrypb.Repository_DockerConfig{DockerConfig: &artifactregistrypb.Repository_DockerRepositoryConfig{ImmutableTags: true}}
	}
	if err := checkSupported(in); err != nil {
		return err
	}
	return s.env.Store.Update(func(tx store.Tx) error {
		now := timestamppb.New(s.env.Clock.Now())
		repo, err := getRepo(tx, ref)
		if err != nil {
			repo = in
			repo.Name = ref.name()
			repo.Mode = artifactregistrypb.Repository_STANDARD_REPOSITORY
			repo.CreateTime = now
			repo.RegistryUri = strings.TrimSuffix(ref.imagePrefix(), "/")
		} else {
			repo.Description, repo.Labels, repo.FormatConfig = in.Description, in.Labels, in.FormatConfig
		}
		repo.UpdateTime = now
		normalizeRepo(repo)
		return putRepo(tx, ref, repo)
	})
}
