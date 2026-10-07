package ar

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// FR-AR-004: packages, versions, tags and docker images are views over the
// registry contents (one package per image path, one version per manifest).

func entityNotFound() error {
	return apierr.NotFound(notFound).WithReason(errDom, "RESOURCE_NOT_FOUND")
}

// viewRepo runs fn in a read transaction after checking the repository
// exists and the caller holds perm on it.
func (s *Service) viewRepo(ctx context.Context, ref repoRef, perm string, fn func(tx store.Tx) error) error {
	if err := s.repoAccess(ctx, ref, perm); err != nil {
		return err
	}
	return s.env.Store.View(func(tx store.Tx) error {
		if _, err := getRepo(tx, ref); err != nil {
			return err
		}
		return fn(tx)
	})
}

func ts(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

func dockerImage(ref repoRef, img string, m *manifestRec, tags []string) *artifactregistrypb.DockerImage {
	d := &artifactregistrypb.DockerImage{
		Name:           dockerImageName(ref, img, m.Digest),
		Uri:            ref.imagePrefix() + img + "@" + m.Digest,
		Tags:           tags,
		ImageSizeBytes: m.ContentSize,
		UploadTime:     ts(m.Created),
		MediaType:      m.MediaType,
		UpdateTime:     ts(m.Updated),
		ArtifactType:   m.ArtifactType,
	}
	if m.BuildTime != nil {
		d.BuildTime = ts(*m.BuildTime)
	}
	for _, c := range m.Children {
		d.ImageManifests = append(d.ImageManifests, &artifactregistrypb.ImageManifest{
			Architecture: c.Architecture, Os: c.OS, Digest: c.Digest, MediaType: c.MediaType,
			OsVersion: c.OSVersion, OsFeatures: c.OSFeatures, Variant: c.Variant,
		})
	}
	return d
}

// ListDockerImages implements FR-AR-004.
func (a *api) ListDockerImages(ctx context.Context, req *artifactregistrypb.ListDockerImagesRequest) (*artifactregistrypb.ListDockerImagesResponse, error) {
	ref, rest, err := parseRepoName(req.GetParent())
	if err != nil || len(rest) != 0 {
		return nil, invalidName(req.GetParent())
	}
	var out []*artifactregistrypb.DockerImage
	err = a.s.viewRepo(ctx, ref, "artifactregistry.dockerimages.list", func(tx store.Tx) error {
		imgs, ms := listManifests(tx, ref, "")
		for i, m := range ms {
			out = append(out, dockerImage(ref, imgs[i], m, tagsFor(tx, ref, imgs[i], m.Digest)))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := orderImages(out, req.GetOrderBy()); err != nil {
		return nil, err
	}
	pg, next, err := page(out, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &artifactregistrypb.ListDockerImagesResponse{DockerImages: pg, NextPageToken: next}, nil
}

// orderImages applies a ListDockerImages order_by ("field [desc]").
func orderImages(imgs []*artifactregistrypb.DockerImage, orderBy string) error {
	f := strings.Fields(strings.TrimSpace(orderBy))
	if len(f) == 0 {
		return nil
	}
	desc := len(f) > 1 && strings.EqualFold(f[1], "desc")
	var less func(a, b *artifactregistrypb.DockerImage) bool
	switch snakeCase(f[0]) {
	case "name":
		less = func(a, b *artifactregistrypb.DockerImage) bool { return a.Name < b.Name }
	case "upload_time":
		less = func(a, b *artifactregistrypb.DockerImage) bool {
			return a.UploadTime.AsTime().Before(b.UploadTime.AsTime())
		}
	case "update_time":
		less = func(a, b *artifactregistrypb.DockerImage) bool {
			return a.UpdateTime.AsTime().Before(b.UpdateTime.AsTime())
		}
	case "build_time":
		less = func(a, b *artifactregistrypb.DockerImage) bool {
			return a.BuildTime.AsTime().Before(b.BuildTime.AsTime())
		}
	case "image_size_bytes":
		less = func(a, b *artifactregistrypb.DockerImage) bool { return a.ImageSizeBytes < b.ImageSizeBytes }
	default:
		return apierr.InvalidArgument("Invalid order_by field %q.", f[0])
	}
	sort.SliceStable(imgs, func(i, j int) bool {
		if desc {
			return less(imgs[j], imgs[i])
		}
		return less(imgs[i], imgs[j])
	})
	return nil
}

// GetDockerImage implements FR-AR-004.
func (a *api) GetDockerImage(ctx context.Context, req *artifactregistrypb.GetDockerImageRequest) (*artifactregistrypb.DockerImage, error) {
	ref, img, digest, err := parseDockerImageName(req.GetName())
	if err != nil {
		return nil, err
	}
	var out *artifactregistrypb.DockerImage
	err = a.s.viewRepo(ctx, ref, "artifactregistry.dockerimages.get", func(tx store.Tx) error {
		m, ok := getManifest(tx, ref, img, digest)
		if !ok {
			return entityNotFound()
		}
		out = dockerImage(ref, img, m, tagsFor(tx, ref, img, digest))
		return nil
	})
	return out, err
}

func pkgProto(ref repoRef, e imageEntry) *artifactregistrypb.Package {
	return &artifactregistrypb.Package{Name: packageName(ref, e.Image), CreateTime: ts(e.Created), UpdateTime: ts(e.Updated)}
}

// ListPackages implements FR-AR-004.
func (a *api) ListPackages(ctx context.Context, req *artifactregistrypb.ListPackagesRequest) (*artifactregistrypb.ListPackagesResponse, error) {
	ref, rest, err := parseRepoName(req.GetParent())
	if err != nil || len(rest) != 0 {
		return nil, invalidName(req.GetParent())
	}
	match, err := parseNameFilter(req.GetFilter())
	if err != nil {
		return nil, err
	}
	var out []*artifactregistrypb.Package
	err = a.s.viewRepo(ctx, ref, "artifactregistry.packages.list", func(tx store.Tx) error {
		for _, e := range listImages(tx, ref) {
			if p := pkgProto(ref, e); match(p.Name) {
				out = append(out, p)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortByName(out, req.GetOrderBy(), func(p *artifactregistrypb.Package) string { return p.Name })
	pg, next, err := page(out, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &artifactregistrypb.ListPackagesResponse{Packages: pg, NextPageToken: next}, nil
}

// GetPackage implements FR-AR-004.
func (a *api) GetPackage(ctx context.Context, req *artifactregistrypb.GetPackageRequest) (*artifactregistrypb.Package, error) {
	ref, img, err := parsePackageName(req.GetName())
	if err != nil {
		return nil, err
	}
	var out *artifactregistrypb.Package
	err = a.s.viewRepo(ctx, ref, "artifactregistry.packages.get", func(tx store.Tx) error {
		for _, e := range listImages(tx, ref) {
			if e.Image == img {
				out = pkgProto(ref, e)
				return nil
			}
		}
		return entityNotFound()
	})
	return out, err
}

// DeletePackage implements FR-AR-004 (long-running): every manifest, tag
// and blob link of the image is removed.
func (a *api) DeletePackage(ctx context.Context, req *artifactregistrypb.DeletePackageRequest) (*longrunningpb.Operation, error) {
	s := a.s
	ref, img, err := parsePackageName(req.GetName())
	if err != nil {
		return nil, err
	}
	err = s.viewRepo(ctx, ref, "artifactregistry.packages.delete", func(tx store.Tx) error {
		if !store.HasPrefix(tx, nsManifests, imgPrefix(ref, img)) {
			return entityNotFound()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.ops.Run(ctx, "projects/"+ref.Project+"/locations/"+ref.Location, &artifactregistrypb.OperationMetadata{}, func(ctx context.Context) (proto.Message, error) {
		var digests []string
		err := s.env.Store.Update(func(tx store.Tx) error {
			var err error
			digests, err = deleteImageData(tx, imgPrefix(ref, img))
			return err
		})
		if err != nil {
			return nil, err
		}
		s.collect(digests)
		return &emptypb.Empty{}, nil
	})
}

func versionProto(tx store.Tx, ref repoRef, img string, m *manifestRec, full bool) *artifactregistrypb.Version {
	md, _ := structpb.NewStruct(map[string]any{
		"name":           dockerImageName(ref, img, m.Digest),
		"mediaType":      m.MediaType,
		"imageSizeBytes": strconv.FormatInt(m.ContentSize, 10),
	})
	if m.BuildTime != nil {
		md.Fields["buildTime"] = structpb.NewStringValue(m.BuildTime.UTC().Format(time.RFC3339Nano))
	}
	v := &artifactregistrypb.Version{
		Name:        versionName(ref, img, m.Digest),
		CreateTime:  ts(m.Created),
		UpdateTime:  ts(m.Updated),
		Metadata:    md,
		Annotations: m.Annotations,
	}
	if full {
		for _, t := range tagsFor(tx, ref, img, m.Digest) {
			v.RelatedTags = append(v.RelatedTags, &artifactregistrypb.Tag{Name: tagName(ref, img, t), Version: v.Name})
		}
	}
	return v
}

// ListVersions implements FR-AR-004.
func (a *api) ListVersions(ctx context.Context, req *artifactregistrypb.ListVersionsRequest) (*artifactregistrypb.ListVersionsResponse, error) {
	ref, img, err := parsePackageName(req.GetParent())
	if err != nil {
		return nil, err
	}
	match, err := parseNameFilter(req.GetFilter())
	if err != nil {
		return nil, err
	}
	full := req.GetView() == artifactregistrypb.VersionView_FULL
	var out []*artifactregistrypb.Version
	err = a.s.viewRepo(ctx, ref, "artifactregistry.versions.list", func(tx store.Tx) error {
		_, ms := listManifests(tx, ref, img)
		for _, m := range ms {
			if v := versionProto(tx, ref, img, m, full); match(v.Name) {
				out = append(out, v)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := orderVersions(out, req.GetOrderBy()); err != nil {
		return nil, err
	}
	pg, next, err := page(out, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &artifactregistrypb.ListVersionsResponse{Versions: pg, NextPageToken: next}, nil
}

func orderVersions(vs []*artifactregistrypb.Version, orderBy string) error {
	f := strings.Fields(orderBy)
	if len(f) == 0 {
		return nil
	}
	desc := len(f) > 1 && strings.EqualFold(f[1], "desc")
	var less func(a, b *artifactregistrypb.Version) bool
	switch snakeCase(f[0]) {
	case "name":
		less = func(a, b *artifactregistrypb.Version) bool { return a.Name < b.Name }
	case "create_time":
		less = func(a, b *artifactregistrypb.Version) bool {
			return a.CreateTime.AsTime().Before(b.CreateTime.AsTime())
		}
	case "update_time":
		less = func(a, b *artifactregistrypb.Version) bool {
			return a.UpdateTime.AsTime().Before(b.UpdateTime.AsTime())
		}
	default:
		return apierr.InvalidArgument("Invalid order_by field %q.", f[0])
	}
	sort.SliceStable(vs, func(i, j int) bool {
		if desc {
			return less(vs[j], vs[i])
		}
		return less(vs[i], vs[j])
	})
	return nil
}

// GetVersion implements FR-AR-004.
func (a *api) GetVersion(ctx context.Context, req *artifactregistrypb.GetVersionRequest) (*artifactregistrypb.Version, error) {
	ref, img, digest, err := parsePackageSub(req.GetName(), "versions")
	if err != nil {
		return nil, err
	}
	var out *artifactregistrypb.Version
	err = a.s.viewRepo(ctx, ref, "artifactregistry.versions.get", func(tx store.Tx) error {
		m, ok := getManifest(tx, ref, img, digest)
		if !ok {
			return entityNotFound()
		}
		out = versionProto(tx, ref, img, m, req.GetView() == artifactregistrypb.VersionView_FULL)
		return nil
	})
	return out, err
}

// DeleteVersion implements FR-AR-004 (long-running). A tagged version is
// only deleted with force, which also deletes its tags.
func (a *api) DeleteVersion(ctx context.Context, req *artifactregistrypb.DeleteVersionRequest) (*longrunningpb.Operation, error) {
	s := a.s
	ref, img, digest, err := parsePackageSub(req.GetName(), "versions")
	if err != nil {
		return nil, err
	}
	err = s.viewRepo(ctx, ref, "artifactregistry.versions.delete", func(tx store.Tx) error {
		if _, ok := getManifest(tx, ref, img, digest); !ok {
			return entityNotFound()
		}
		if len(tagsFor(tx, ref, img, digest)) > 0 && !req.GetForce() {
			return apierr.FailedPrecondition("Version %s has tags; delete the tags first or set force=true.", req.GetName())
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.ops.Run(ctx, "projects/"+ref.Project+"/locations/"+ref.Location, &artifactregistrypb.OperationMetadata{}, func(ctx context.Context) (proto.Message, error) {
		if err := s.removeManifest(ref, img, digest); err != nil {
			return nil, err
		}
		return &emptypb.Empty{}, nil
	})
}

// removeManifest deletes a manifest and its tags and collects its blob.
func (s *Service) removeManifest(ref repoRef, img, digest string) error {
	err := s.env.Store.Update(func(tx store.Tx) error {
		if _, ok := getManifest(tx, ref, img, digest); !ok {
			return entityNotFound()
		}
		return deleteManifest(tx, ref, img, digest)
	})
	if err == nil {
		s.collect([]string{digest})
	}
	return err
}

var (
	versionFilterRe = regexp.MustCompile(`^\s*version\s*=\s*"?([^"]+?)"?\s*$`)
)

// ListTags implements FR-AR-004; filter supports version="<version name>"
// and name="<tag name>".
func (a *api) ListTags(ctx context.Context, req *artifactregistrypb.ListTagsRequest) (*artifactregistrypb.ListTagsResponse, error) {
	ref, img, err := parsePackageName(req.GetParent())
	if err != nil {
		return nil, err
	}
	matchVersion := func(string) bool { return true }
	matchName := func(string) bool { return true }
	if m := versionFilterRe.FindStringSubmatch(req.GetFilter()); m != nil {
		matchVersion = func(v string) bool { return v == m[1] || strings.HasSuffix(v, "/versions/"+m[1]) }
	} else if matchName, err = parseNameFilter(req.GetFilter()); err != nil {
		return nil, err
	}
	var out []*artifactregistrypb.Tag
	err = a.s.viewRepo(ctx, ref, "artifactregistry.tags.list", func(tx store.Tx) error {
		pre := imgPrefix(ref, img)
		tx.Scan(nsTags, pre, func(k string, b []byte) bool {
			var t tagRec
			if json.Unmarshal(b, &t) != nil {
				return true
			}
			tag := &artifactregistrypb.Tag{Name: tagName(ref, img, strings.TrimPrefix(k, pre)), Version: versionName(ref, img, t.Digest)}
			if matchVersion(tag.Version) && matchName(tag.Name) {
				out = append(out, tag)
			}
			return true
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	pg, next, err := page(out, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &artifactregistrypb.ListTagsResponse{Tags: pg, NextPageToken: next}, nil
}

// GetTag implements FR-AR-004.
func (a *api) GetTag(ctx context.Context, req *artifactregistrypb.GetTagRequest) (*artifactregistrypb.Tag, error) {
	ref, img, tag, err := parsePackageSub(req.GetName(), "tags")
	if err != nil {
		return nil, err
	}
	var out *artifactregistrypb.Tag
	err = a.s.viewRepo(ctx, ref, "artifactregistry.tags.get", func(tx store.Tx) error {
		t, ok := getTag(tx, ref, img, tag)
		if !ok {
			return entityNotFound()
		}
		out = &artifactregistrypb.Tag{Name: tagName(ref, img, tag), Version: versionName(ref, img, t.Digest)}
		return nil
	})
	return out, err
}

// setTag points tag at the version named by version (create or move).
func (s *Service) setTag(ctx context.Context, ref repoRef, img, tag, version, perm string, create bool) (*artifactregistrypb.Tag, error) {
	if !tagRe.MatchString(tag) {
		return nil, apierr.InvalidArgument("Invalid tag %q.", tag)
	}
	vref, vimg, digest, err := parsePackageSub(version, "versions")
	if err != nil || vref != ref || vimg != img {
		return nil, apierr.InvalidArgument("Invalid version %q: must be a version of package %s.", version, packageName(ref, img))
	}
	if err := s.repoAccess(ctx, ref, perm); err != nil {
		return nil, err
	}
	err = s.env.Store.Update(func(tx store.Tx) error {
		repo, err := getRepo(tx, ref)
		if err != nil {
			return err
		}
		if _, ok := getManifest(tx, ref, img, digest); !ok {
			return entityNotFound()
		}
		now := s.env.Clock.Now()
		old, exists := getTag(tx, ref, img, tag)
		switch {
		case create && exists:
			return apierr.AlreadyExists("Tag %s already exists.", tagName(ref, img, tag))
		case !create && !exists:
			return entityNotFound()
		case exists && old.Digest != digest && repo.GetDockerConfig().GetImmutableTags():
			return apierr.FailedPrecondition("Tag %s is immutable.", tag)
		}
		rec := tagRec{Digest: digest, Created: now, Updated: now}
		if exists {
			rec.Created = old.Created
		}
		return store.PutJSON(tx, nsTags, itemKey(ref, img, tag), rec)
	})
	if err != nil {
		return nil, err
	}
	return &artifactregistrypb.Tag{Name: tagName(ref, img, tag), Version: versionName(ref, img, digest)}, nil
}

// CreateTag implements FR-AR-004.
func (a *api) CreateTag(ctx context.Context, req *artifactregistrypb.CreateTagRequest) (*artifactregistrypb.Tag, error) {
	ref, img, err := parsePackageName(req.GetParent())
	if err != nil {
		return nil, err
	}
	id := req.GetTagId()
	if id == "" {
		if _, _, t, err := parsePackageSub(req.GetTag().GetName(), "tags"); err == nil {
			id = t
		}
	}
	return a.s.setTag(ctx, ref, img, id, req.GetTag().GetVersion(), "artifactregistry.tags.create", true)
}

// UpdateTag implements FR-AR-004 (only version is mutable).
func (a *api) UpdateTag(ctx context.Context, req *artifactregistrypb.UpdateTagRequest) (*artifactregistrypb.Tag, error) {
	ref, img, tag, err := parsePackageSub(req.GetTag().GetName(), "tags")
	if err != nil {
		return nil, err
	}
	for _, p := range req.GetUpdateMask().GetPaths() {
		if p != "version" {
			return nil, apierr.InvalidArgument("Invalid update_mask path %q.", p)
		}
	}
	return a.s.setTag(ctx, ref, img, tag, req.GetTag().GetVersion(), "artifactregistry.tags.update", false)
}

// DeleteTag implements FR-AR-004.
func (a *api) DeleteTag(ctx context.Context, req *artifactregistrypb.DeleteTagRequest) (*emptypb.Empty, error) {
	ref, img, tag, err := parsePackageSub(req.GetName(), "tags")
	if err != nil {
		return nil, err
	}
	if err := a.s.repoAccess(ctx, ref, "artifactregistry.tags.delete"); err != nil {
		return nil, err
	}
	err = a.s.env.Store.Update(func(tx store.Tx) error {
		if _, err := getRepo(tx, ref); err != nil {
			return err
		}
		if _, ok := getTag(tx, ref, img, tag); !ok {
			return entityNotFound()
		}
		return tx.Delete(nsTags, itemKey(ref, img, tag))
	})
	if err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

var nameFilterRe = regexp.MustCompile(`^\s*name\s*([=:])\s*"?([^"]*?)"?\s*$`)

// parseNameFilter supports the AIP-160 subset AR documents for list
// filters: name="<full name>" with an optional trailing '*' wildcard.
func parseNameFilter(filter string) (func(string) bool, error) {
	if strings.TrimSpace(filter) == "" {
		return func(string) bool { return true }, nil
	}
	m := nameFilterRe.FindStringSubmatch(filter)
	if m == nil {
		return nil, apierr.InvalidArgument("Invalid filter %q.", filter)
	}
	v := m[2]
	if pre, ok := strings.CutSuffix(v, "*"); ok {
		return func(n string) bool { return strings.HasPrefix(n, pre) }, nil
	}
	return func(n string) bool { return n == v || strings.HasSuffix(n, "/"+v) }, nil
}

// sortByName sorts by name, descending when orderBy is "name desc".
func sortByName[T any](items []T, orderBy string, name func(T) string) {
	desc := strings.HasSuffix(strings.ToLower(strings.TrimSpace(orderBy)), " desc")
	sort.SliceStable(items, func(i, j int) bool {
		if desc {
			return name(items[i]) > name(items[j])
		}
		return name(items[i]) < name(items[j])
	})
}
