package ar

import (
	"context"
	"fmt"
	"strings"

	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gopkg.in/yaml.v3"

	"github.com/linuxuser586/gcpemu/internal/emu"
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
//	    - project: my-project    # FR-AR-006 remote repository
//	      location: us-central1
//	      id: dockerhub
//	      mode: REMOTE_REPOSITORY
//	      remote: {publicRepository: DOCKER_HUB}   # or {uri: https://quay.io}
//	    - project: my-project    # FR-AR-006 virtual repository
//	      location: us-central1
//	      id: all
//	      mode: VIRTUAL_REPOSITORY
//	      upstreams:
//	        - {id: mine, repository: projects/my-project/locations/us-central1/repositories/images, priority: 100}
//	        - {id: hub, repository: projects/my-project/locations/us-central1/repositories/dockerhub, priority: 10}
type seedFile struct {
	Repositories []seedRepo `yaml:"repositories"`
}

type seedRepo struct {
	Name          string            `yaml:"name,omitempty"`
	Project       string            `yaml:"project,omitempty"`
	Location      string            `yaml:"location,omitempty"`
	ID            string            `yaml:"id,omitempty"`
	Format        string            `yaml:"format,omitempty"`
	Mode          string            `yaml:"mode,omitempty"`
	Description   string            `yaml:"description,omitempty"`
	Labels        map[string]string `yaml:"labels,omitempty"`
	ImmutableTags bool              `yaml:"immutableTags,omitempty"`
	Remote        *seedRemote       `yaml:"remote,omitempty"`
	Upstreams     []seedUpstream    `yaml:"upstreams,omitempty"`
}

type seedRemote struct {
	PublicRepository string `yaml:"publicRepository,omitempty"`
	URI              string `yaml:"uri,omitempty"`
}

type seedUpstream struct {
	ID         string `yaml:"id"`
	Repository string `yaml:"repository"`
	Priority   int32  `yaml:"priority,omitempty"`
}

var _ emu.SeedExporter = (*Service)(nil)

// ExportSeed writes the repositories back as the "ar" section
// (FR-CORE-013), ordered by project, location and ID. Images and their
// manifests and blobs are not exported: the seed file cannot declare
// them. A remote repository with a common_repository upstream is written
// as remote.uri, which serves the same upstream.
func (s *Service) ExportSeed(ctx context.Context) (any, error) {
	var refs []repoRef
	var repos []*artifactregistrypb.Repository
	err := s.env.Store.View(func(tx store.Tx) error {
		var err error
		refs, repos, err = listRepos(tx, "")
		return err
	})
	if err != nil || len(repos) == 0 {
		return nil, err
	}
	f := seedFile{}
	for i, repo := range repos {
		sr := seedRepo{
			Project:       refs[i].Project,
			Location:      refs[i].Location,
			ID:            refs[i].Repo,
			Description:   repo.GetDescription(),
			Labels:        repo.GetLabels(),
			ImmutableTags: repo.GetDockerConfig().GetImmutableTags(),
		}
		if fm := repo.GetFormat(); fm != artifactregistrypb.Repository_DOCKER {
			sr.Format = fm.String()
		}
		switch repo.GetMode() {
		case artifactregistrypb.Repository_REMOTE_REPOSITORY:
			sr.Mode = repo.GetMode().String()
			rc := repo.GetRemoteRepositoryConfig()
			sr.Remote = &seedRemote{URI: rc.GetCommonRepository().GetUri()}
			if dr := rc.GetDockerRepository(); dr != nil {
				if dr.GetPublicRepository() == artifactregistrypb.RemoteRepositoryConfig_DockerRepository_DOCKER_HUB {
					sr.Remote = &seedRemote{PublicRepository: "DOCKER_HUB"}
				} else {
					sr.Remote = &seedRemote{URI: dr.GetCustomRepository().GetUri()}
				}
			}
		case artifactregistrypb.Repository_VIRTUAL_REPOSITORY:
			sr.Mode = repo.GetMode().String()
			for _, u := range repo.GetVirtualRepositoryConfig().GetUpstreamPolicies() {
				sr.Upstreams = append(sr.Upstreams, seedUpstream{ID: u.GetId(), Repository: u.GetRepository(), Priority: u.GetPriority()})
			}
		}
		f.Repositories = append(f.Repositories, sr)
	}
	return f, nil
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
	if rc := sr.Remote; rc != nil {
		dr := &artifactregistrypb.RemoteRepositoryConfig_DockerRepository{}
		if strings.EqualFold(rc.PublicRepository, "DOCKER_HUB") {
			dr.Upstream = &artifactregistrypb.RemoteRepositoryConfig_DockerRepository_PublicRepository_{PublicRepository: artifactregistrypb.RemoteRepositoryConfig_DockerRepository_DOCKER_HUB}
		} else if rc.URI != "" {
			dr.Upstream = &artifactregistrypb.RemoteRepositoryConfig_DockerRepository_CustomRepository_{CustomRepository: &artifactregistrypb.RemoteRepositoryConfig_DockerRepository_CustomRepository{Uri: rc.URI}}
		}
		in.ModeConfig = &artifactregistrypb.Repository_RemoteRepositoryConfig{RemoteRepositoryConfig: &artifactregistrypb.RemoteRepositoryConfig{
			RemoteSource: &artifactregistrypb.RemoteRepositoryConfig_DockerRepository_{DockerRepository: dr},
		}}
	}
	if in.Mode == artifactregistrypb.Repository_VIRTUAL_REPOSITORY {
		vc := &artifactregistrypb.VirtualRepositoryConfig{}
		for _, u := range sr.Upstreams {
			vc.UpstreamPolicies = append(vc.UpstreamPolicies, &artifactregistrypb.UpstreamPolicy{Id: u.ID, Repository: u.Repository, Priority: u.Priority})
		}
		in.ModeConfig = &artifactregistrypb.Repository_VirtualRepositoryConfig{VirtualRepositoryConfig: vc}
	}
	if err := checkSupported(in, ref.Location); err != nil {
		return err
	}
	return s.env.Store.Update(func(tx store.Tx) error {
		now := timestamppb.New(s.env.Clock.Now())
		repo, err := getRepo(tx, ref)
		if err != nil {
			repo = in
			repo.Name = ref.name()
			if repo.Mode == artifactregistrypb.Repository_MODE_UNSPECIFIED {
				repo.Mode = artifactregistrypb.Repository_STANDARD_REPOSITORY
			}
			repo.CreateTime = now
			repo.RegistryUri = strings.TrimSuffix(ref.imagePrefix(), "/")
		} else {
			repo.Description, repo.Labels, repo.FormatConfig = in.Description, in.Labels, in.FormatConfig
			if repo.Mode == in.Mode && in.ModeConfig != nil {
				repo.ModeConfig = in.ModeConfig
			}
		}
		repo.UpdateTime = now
		normalizeRepo(repo)
		return putRepo(tx, ref, repo)
	})
}
