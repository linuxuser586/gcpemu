package ar

import (
	"context"
	"strings"
	"unicode"

	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// api implements artifactregistrypb.ArtifactRegistryServer. Methods that are
// not implemented return UNIMPLEMENTED naming the method (SRS 10.1 Absent).
type api struct {
	artifactregistrypb.UnimplementedArtifactRegistryServer
	s *Service
}

// mutableRepoFields are the Repository fields UpdateRepository may change.
var mutableRepoFields = []string{
	"description", "labels", "docker_config", "cleanup_policies",
	"cleanup_policy_dry_run", "vulnerability_scanning_config", "disallow_unspecified_mode",
}

// CreateRepository implements FR-AR-001 (long-running).
func (a *api) CreateRepository(ctx context.Context, req *artifactregistrypb.CreateRepositoryRequest) (*longrunningpb.Operation, error) {
	s := a.s
	p, loc, err := parseLocationName(req.GetParent())
	if err != nil {
		return nil, err
	}
	if err := s.project(ctx, p, "artifactregistry.repositories.create"); err != nil {
		return nil, err
	}
	if err := checkLocation(loc); err != nil {
		return nil, err
	}
	id := req.GetRepositoryId()
	if !repoIDRe.MatchString(id) || len(id) > 63 {
		return nil, apierr.InvalidArgument("Invalid repository ID %q: repository IDs must start with a lowercase letter, contain only lowercase letters, numbers and hyphens, and be at most 63 characters.", id)
	}
	in := req.GetRepository()
	if in == nil {
		in = &artifactregistrypb.Repository{}
	}
	if err := checkSupported(in); err != nil {
		return nil, err
	}
	ref := repoRef{p, loc, id}
	now := timestamppb.New(s.env.Clock.Now())
	repo := proto.Clone(in).(*artifactregistrypb.Repository)
	repo.Name = ref.name()
	repo.Mode = artifactregistrypb.Repository_STANDARD_REPOSITORY
	repo.CreateTime, repo.UpdateTime = now, now
	repo.SizeBytes = 0
	repo.SatisfiesPzs, repo.SatisfiesPzi = false, false
	repo.RegistryUri = strings.TrimSuffix(ref.imagePrefix(), "/")
	normalizeRepo(repo)

	var exists bool
	_ = s.env.Store.View(func(tx store.Tx) error { exists = store.Exists(tx, nsRepos, ref.key()); return nil })
	if exists {
		return nil, apierr.AlreadyExists("the repository already exists").WithReason(errDom, "RESOURCE_ALREADY_EXISTS")
	}
	return s.ops.Run(ctx, req.GetParent(), &artifactregistrypb.OperationMetadata{}, func(ctx context.Context) (proto.Message, error) {
		err := s.env.Store.Update(func(tx store.Tx) error {
			if store.Exists(tx, nsRepos, ref.key()) {
				return apierr.AlreadyExists("the repository already exists").WithReason(errDom, "RESOURCE_ALREADY_EXISTS")
			}
			return putRepo(tx, ref, repo)
		})
		if err != nil {
			return nil, err
		}
		return repo, nil
	})
}

// checkSupported rejects formats and modes outside the emulated subset.
func checkSupported(r *artifactregistrypb.Repository) error {
	switch r.GetFormat() {
	case artifactregistrypb.Repository_FORMAT_UNSPECIFIED:
		return apierr.InvalidArgument("Repository format must be specified.")
	case artifactregistrypb.Repository_DOCKER:
	default:
		return apierr.Unimplemented("Repository format %s is not supported by the emulator (field repository.format).", r.GetFormat())
	}
	switch r.GetMode() {
	case artifactregistrypb.Repository_MODE_UNSPECIFIED, artifactregistrypb.Repository_STANDARD_REPOSITORY:
	default:
		return apierr.Unimplemented("Repository mode %s is not supported by the emulator (field repository.mode).", r.GetMode())
	}
	if r.GetModeConfig() != nil {
		return apierr.Unimplemented("Remote and virtual repository configs are not supported by the emulator (field repository.mode_config).")
	}
	if r.GetMavenConfig() != nil {
		return apierr.InvalidArgument("maven_config is only valid for MAVEN repositories.")
	}
	return nil
}

// normalizeRepo fills output-only sub-fields.
func normalizeRepo(r *artifactregistrypb.Repository) {
	vc := r.GetVulnerabilityScanningConfig()
	if vc == nil {
		vc = &artifactregistrypb.Repository_VulnerabilityScanningConfig{}
		r.VulnerabilityScanningConfig = vc
	}
	vc.LastEnableTime = nil
	vc.EnablementState = artifactregistrypb.Repository_VulnerabilityScanningConfig_SCANNING_DISABLED
	vc.EnablementStateReason = "API containerscanning.googleapis.com is not enabled."
}

// GetRepository implements FR-AR-001.
func (a *api) GetRepository(ctx context.Context, req *artifactregistrypb.GetRepositoryRequest) (*artifactregistrypb.Repository, error) {
	ref, rest, err := parseRepoName(req.GetName())
	if err != nil || len(rest) != 0 {
		return nil, invalidName(req.GetName())
	}
	if err := a.s.repoAccess(ctx, ref, "artifactregistry.repositories.get"); err != nil {
		return nil, err
	}
	var repo *artifactregistrypb.Repository
	err = a.s.env.Store.View(func(tx store.Tx) error {
		var err error
		if repo, err = getRepo(tx, ref); err == nil {
			repo.SizeBytes = repoSize(tx, ref)
		}
		return err
	})
	return repo, err
}

// ListRepositories implements FR-AR-001.
func (a *api) ListRepositories(ctx context.Context, req *artifactregistrypb.ListRepositoriesRequest) (*artifactregistrypb.ListRepositoriesResponse, error) {
	p, loc, err := parseLocationName(req.GetParent())
	if err != nil {
		return nil, err
	}
	if err := a.s.project(ctx, p, "artifactregistry.repositories.list"); err != nil {
		return nil, err
	}
	if err := checkLocation(loc); err != nil {
		return nil, err
	}
	match, err := parseNameFilter(req.GetFilter())
	if err != nil {
		return nil, err
	}
	var repos []*artifactregistrypb.Repository
	err = a.s.env.Store.View(func(tx store.Tx) error {
		refs, all, err := listRepos(tx, p+"/"+loc+"/")
		for i, r := range all {
			if match(r.GetName()) {
				r.SizeBytes = repoSize(tx, refs[i])
				repos = append(repos, r)
			}
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	sortByName(repos, req.GetOrderBy(), func(r *artifactregistrypb.Repository) string { return r.GetName() })
	pg, next, err := page(repos, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &artifactregistrypb.ListRepositoriesResponse{Repositories: pg, NextPageToken: next}, nil
}

// UpdateRepository implements FR-AR-001 (patch with update_mask).
func (a *api) UpdateRepository(ctx context.Context, req *artifactregistrypb.UpdateRepositoryRequest) (*artifactregistrypb.Repository, error) {
	in := req.GetRepository()
	if in == nil {
		return nil, apierr.InvalidArgument("repository is required.")
	}
	ref, rest, err := parseRepoName(in.GetName())
	if err != nil || len(rest) != 0 {
		return nil, invalidName(in.GetName())
	}
	if err := a.s.repoAccess(ctx, ref, "artifactregistry.repositories.update"); err != nil {
		return nil, err
	}
	paths := mutableRepoFields
	if m := req.GetUpdateMask(); m != nil && len(m.GetPaths()) > 0 {
		paths = nil
		for _, p := range m.GetPaths() {
			p = snakeCase(p)
			if i := strings.IndexByte(p, '.'); i >= 0 && (p[:i] == "labels" || p[:i] == "cleanup_policies") {
				p = p[:i]
			}
			if !contains(mutableRepoFields, p) {
				return nil, apierr.InvalidArgument("Invalid update_mask path %q: field is immutable or does not exist.", p)
			}
			paths = append(paths, p)
		}
	}
	if err := checkSupportedUpdate(in, paths); err != nil {
		return nil, err
	}
	var out *artifactregistrypb.Repository
	err = a.s.env.Store.Update(func(tx store.Tx) error {
		repo, err := getRepo(tx, ref)
		if err != nil {
			return err
		}
		src, dst := in.ProtoReflect(), repo.ProtoReflect()
		fields := dst.Descriptor().Fields()
		for _, p := range paths {
			fd := fields.ByName(protoreflect.Name(p))
			if src.Has(fd) {
				dst.Set(fd, src.Get(fd))
			} else {
				dst.Clear(fd)
			}
		}
		normalizeRepo(repo)
		repo.UpdateTime = timestamppb.New(a.s.env.Clock.Now())
		if err := putRepo(tx, ref, repo); err != nil {
			return err
		}
		repo.SizeBytes = repoSize(tx, ref)
		out = repo
		return nil
	})
	return out, err
}

// checkSupportedUpdate rejects updates that would change format or mode.
func checkSupportedUpdate(in *artifactregistrypb.Repository, paths []string) error {
	if contains(paths, "docker_config") && in.GetMavenConfig() != nil {
		return apierr.InvalidArgument("maven_config is only valid for MAVEN repositories.")
	}
	return nil
}

// DeleteRepository implements FR-AR-001 (long-running); every image,
// tag and blob link in the repository is removed.
func (a *api) DeleteRepository(ctx context.Context, req *artifactregistrypb.DeleteRepositoryRequest) (*longrunningpb.Operation, error) {
	s := a.s
	ref, rest, err := parseRepoName(req.GetName())
	if err != nil || len(rest) != 0 {
		return nil, invalidName(req.GetName())
	}
	if err := s.repoAccess(ctx, ref, "artifactregistry.repositories.delete"); err != nil {
		return nil, err
	}
	if err := s.env.Store.View(func(tx store.Tx) error { _, err := getRepo(tx, ref); return err }); err != nil {
		return nil, err
	}
	parent := "projects/" + ref.Project + "/locations/" + ref.Location
	return s.ops.Run(ctx, parent, &artifactregistrypb.OperationMetadata{}, func(ctx context.Context) (proto.Message, error) {
		var digests []string
		err := s.env.Store.Update(func(tx store.Tx) error {
			if _, err := getRepo(tx, ref); err != nil {
				return err
			}
			if err := tx.Delete(nsRepos, ref.key()); err != nil {
				return err
			}
			var err error
			digests, err = deleteImageData(tx, ref.key()+"|")
			if err != nil {
				return err
			}
			return tx.Delete(nsIAM, ref.resource())
		})
		if err != nil {
			return nil, err
		}
		s.collect(digests)
		s.deletePolicy(ctx, ref.resource())
		return &emptypb.Empty{}, nil
	})
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// snakeCase converts a lowerCamel JSON field path to snake_case.
func snakeCase(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsUpper(r) {
			b.WriteByte('_')
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
