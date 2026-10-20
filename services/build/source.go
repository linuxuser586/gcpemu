package build

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/grpc/codes"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// gitImage clones a gitSource, as Cloud Build's own fetcher runs git.
const gitImage = "alpine/git:v2.49.1"

// revFile is where the git fetcher writes the commit it checked out.
const revFile = "/.gcpemu-revision"

// maxZipSource caps a zip storageSource, which is read into memory.
const maxZipSource = 1 << 30

// validateSource refuses a source without its required fields, and the
// sources this emulator does not have.
func validateSource(src *cloudbuildpb.Source) error {
	switch s := src.GetSource().(type) {
	case nil:
	case *cloudbuildpb.Source_StorageSource:
		if s.StorageSource.GetBucket() == "" || s.StorageSource.GetObject() == "" {
			return apierr.InvalidArgument("invalid build: storageSource needs a bucket and an object.")
		}
	case *cloudbuildpb.Source_GitSource:
		if s.GitSource.GetUrl() == "" {
			return apierr.InvalidArgument("invalid build: gitSource needs a url.")
		}
		if d := path.Clean(s.GitSource.GetDir()); path.IsAbs(d) || d == ".." || strings.HasPrefix(d, "../") {
			return apierr.InvalidArgument("invalid build: gitSource.dir must be a relative path inside the source.")
		}
	case *cloudbuildpb.Source_RepoSource:
		return absentSource("repoSource")
	case *cloudbuildpb.Source_ConnectedRepository:
		return absentSource("connectedRepository")
	case *cloudbuildpb.Source_StorageSourceManifest:
		return absentSource("storageSourceManifest")
	default:
		return apierr.New(codes.Unimplemented, "This Build source is not emulated.")
	}
	return nil
}

func absentSource(field string) error {
	return apierr.New(codes.Unimplemented, "Build source %s is not emulated; use storageSource or gitSource.", field)
}

// sourceDir is the directory under /workspace that a Build's steps run in
// by default: gitSource.dir, or /workspace itself.
func sourceDir(b *cloudbuildpb.Build) string {
	return path.Join(workspace, b.GetSource().GetGitSource().GetDir())
}

// fetchSource fills /workspace with the Build's source. It returns a
// *stepFailure, ending the Build FETCH_SOURCE_FAILED, when the source
// cannot be fetched, and nil when there is none.
func (s *Service) fetchSource(ctx context.Context, rt *runtime.Manager, r *liveBuild, b *cloudbuildpb.Build, vols buildVolumes, network string) error {
	if b.GetSource().GetSource() == nil {
		return nil
	}
	s.logs.line(r.ref, "FETCHSOURCE")
	var err error
	switch src := b.GetSource().GetSource().(type) {
	case *cloudbuildpb.Source_StorageSource:
		err = s.fetchStorage(ctx, rt, r, b, vols, src.StorageSource)
	case *cloudbuildpb.Source_GitSource:
		err = s.fetchGit(ctx, rt, r, b, vols, network, src.GitSource)
	}
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	detail := "failed to fetch source: " + err.Error()
	s.logs.line(r.ref, detail)
	return &stepFailure{outcome{status: cloudbuildpb.Build_FAILURE, detail: detail,
		failure: &cloudbuildpb.Build_FailureInfo{Type: cloudbuildpb.Build_FailureInfo_FETCH_SOURCE_FAILED, Detail: detail}}}
}

// fetchStorage extracts a gzipped tarball, a tarball or a zip archive
// from Cloud Storage into /workspace, and records the generation it read.
func (s *Service) fetchStorage(ctx context.Context, rt *runtime.Manager, r *liveBuild, b *cloudbuildpb.Build, vols buildVolumes, src *cloudbuildpb.StorageSource) error {
	objs := s.objects()
	if objs == nil {
		return fmt.Errorf("the gcs Service is not running")
	}
	at := "gs://" + src.GetBucket() + "/" + src.GetObject()
	if g := src.GetGeneration(); g != 0 {
		at += fmt.Sprintf("#%d", g)
	}
	s.logs.line(r.ref, "Fetching storage object: "+at)
	rc, gen, err := objs.ReadObject(ctx, src.GetBucket(), src.GetObject(), src.GetGeneration())
	if err != nil {
		return fmt.Errorf("reading %s: %w", at, err)
	}
	defer rc.Close()
	archive, err := tarStream(rc)
	if err != nil {
		return fmt.Errorf("%s: %w", at, err)
	}
	image := b.GetSteps()[0].GetName()
	if err := rt.EnsureImage(ctx, image, rt.Offline); err != nil {
		return fmt.Errorf("pulling %s to extract the source into: %w", image, err)
	}
	err = s.withFetcher(ctx, rt, b, vols, runtime.ContainerSpec{Image: image, Entrypoint: []string{"/.gcpemu-unused"}}, func(id string) error {
		return rt.ArchiveTo(ctx, id, workspace, archive)
	})
	if err != nil {
		return fmt.Errorf("extracting %s: %w", at, err)
	}
	s.setSource(r, func(p *cloudbuildpb.SourceProvenance) {
		p.ResolvedStorageSource = &cloudbuildpb.StorageSource{Bucket: src.GetBucket(), Object: src.GetObject(), Generation: gen}
	})
	return nil
}

// tarStream returns an uncompressed tar stream of a source archive: a
// gzipped tarball (what gcloud builds submit uploads), a zip archive or a
// plain tarball.
func tarStream(r io.Reader) (io.Reader, error) {
	br := bufio.NewReader(r)
	magic, _ := br.Peek(4)
	switch {
	case bytes.HasPrefix(magic, []byte{0x1f, 0x8b}):
		zr, err := gzip.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("not a gzipped archive: %w", err)
		}
		return zr, nil
	case bytes.Equal(magic, []byte("PK\x03\x04")):
		return zipToTar(br)
	}
	return br, nil
}

// zipToTar converts a zip archive to a tar stream.
func zipToTar(r io.Reader) (io.Reader, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxZipSource+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxZipSource {
		return nil, fmt.Errorf("zip archive larger than %d bytes", maxZipSource)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("not a zip archive: %w", err)
	}
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		err := func() error {
			for _, f := range zr.File {
				h, err := tar.FileInfoHeader(f.FileInfo(), "")
				if err != nil {
					return err
				}
				h.Name = f.Name
				if err := tw.WriteHeader(h); err != nil {
					return err
				}
				if f.FileInfo().IsDir() {
					continue
				}
				rc, err := f.Open()
				if err != nil {
					return err
				}
				_, err = io.Copy(tw, rc)
				rc.Close()
				if err != nil {
					return err
				}
			}
			return tw.Close()
		}()
		pw.CloseWithError(err)
	}()
	return pr, nil
}

// fetchGit clones a public Git repository into /workspace at revision (a
// branch, tag or commit; the default branch when empty), and records the
// commit it checked out.
func (s *Service) fetchGit(ctx context.Context, rt *runtime.Manager, r *liveBuild, b *cloudbuildpb.Build, vols buildVolumes, network string, src *cloudbuildpb.GitSource) error {
	if err := rt.EnsureImage(ctx, gitImage, rt.Offline); err != nil {
		return fmt.Errorf("pulling %s: %w", gitImage, err)
	}
	script := strings.Join([]string{
		"set -e",
		"git init -q .",
		`git remote add origin "$URL"`,
		`if git fetch -q --depth=1 origin "${REV:-HEAD}"; then git checkout -q FETCH_HEAD`,
		`else git fetch -q origin '+refs/heads/*:refs/remotes/origin/*' '+refs/tags/*:refs/tags/*' && git checkout -q "$REV"; fi`,
		"git rev-parse HEAD > " + revFile,
		`echo "HEAD is now at $(git log -1 --format='%h %s')"`,
	}, "\n")
	spec := runtime.ContainerSpec{
		Image:      gitImage,
		Entrypoint: []string{"/bin/sh", "-c", script},
		Env:        []string{"URL=" + src.GetUrl(), "REV=" + src.GetRevision(), "HOME=/tmp", "GIT_TERMINAL_PROMPT=0"},
		WorkingDir: workspace,
		Networks:   []runtime.Attachment{{Network: network}},
	}
	var rev string
	err := s.withFetcher(ctx, rt, b, vols, spec, func(id string) error {
		if err := rt.StartContainer(ctx, id); err != nil {
			return err
		}
		copied := s.follow(ctx, rt, r.ref, id, "")
		code, err := rt.WaitContainer(ctx, id)
		if err != nil {
			return err
		}
		waitCopied(copied)
		if code != 0 {
			return fmt.Errorf("git exited with status %d fetching %s at %q", code, src.GetUrl(), src.GetRevision())
		}
		out, err := rt.CopyFrom(ctx, id, revFile)
		rev = strings.TrimSpace(string(out))
		return err
	})
	if err != nil {
		return err
	}
	s.setSource(r, func(p *cloudbuildpb.SourceProvenance) {
		p.ResolvedGitSource = &cloudbuildpb.GitSource{Url: src.GetUrl(), Dir: src.GetDir(), Revision: rev}
	})
	return nil
}

// withFetcher runs fn with a container of spec that has the Build's
// /workspace and builder home mounted, removing it afterwards.
func (s *Service) withFetcher(ctx context.Context, rt *runtime.Manager, b *cloudbuildpb.Build, vols buildVolumes, spec runtime.ContainerSpec, fn func(id string) error) error {
	spec.Name = rt.Name("build", b.GetId(), "fetch")
	spec.Labels = rt.Labels("build", b.GetName(), "fetch")
	spec.Mounts = []runtime.Mount{{Type: "volume", Source: vols[workspace], Target: workspace}, {Type: "volume", Source: vols[home], Target: home}}
	// As with a step, the container is created even when the Build is
	// stopped meanwhile, so that its cleanup finds it.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	id, err := rt.CreateContainer(cctx, spec)
	cancel()
	if err != nil {
		return err
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		_ = rt.RemoveContainer(rctx, id, true)
	}()
	return fn(id)
}

// setSource changes a WORKING Build's sourceProvenance, unless it was
// halted.
func (s *Service) setSource(r *liveBuild, fn func(*cloudbuildpb.SourceProvenance)) {
	if r.halted() {
		return
	}
	err := s.update(r.ref, func(st *stored) {
		if st.build.SourceProvenance == nil {
			st.build.SourceProvenance = &cloudbuildpb.SourceProvenance{}
		}
		fn(st.build.SourceProvenance)
	})
	if err != nil {
		s.env.Log.Error("build: source provenance", "build", r.ref.name(), "err", err)
	}
}
