package build

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// What a Build leaves once its steps succeed (FR-CB-006): its images,
// pushed to Artifact Registry by the Build daemon as its service account,
// and its artifacts.objects, uploaded to Cloud Storage.

// publish pushes the Build's images and uploads its artifacts. A failure
// is a *stepFailure ending the Build PUSH_FAILED; any other error is the
// Build being stopped.
func (s *Service) publish(ctx context.Context, rt *runtime.Manager, r *liveBuild, b *cloudbuildpb.Build, vols buildVolumes, daemon string) error {
	err := s.pushImages(ctx, rt, r, b, daemon)
	if err == nil {
		err = s.uploadArtifacts(ctx, rt, r, b, vols)
	}
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	detail := "failed to push: " + err.Error()
	s.logs.line(r.ref, "ERROR: "+detail)
	return &stepFailure{outcome{status: cloudbuildpb.Build_FAILURE, detail: detail,
		failure: &cloudbuildpb.Build_FailureInfo{Type: cloudbuildpb.Build_FailureInfo_PUSH_FAILED, Detail: detail}}}
}

// buildImages returns the images a Build pushes: images, then
// artifacts.images, each once.
func buildImages(b *cloudbuildpb.Build) []string {
	var out []string
	for _, img := range append(slices.Clone(b.GetImages()), b.GetArtifacts().GetImages()...) {
		if !slices.Contains(out, img) {
			out = append(out, img)
		}
	}
	return out
}

// pushImages pushes the Build's images from the Build daemon with the
// Build's credentials, and records each one's digest in results.images.
func (s *Service) pushImages(ctx context.Context, rt *runtime.Manager, r *liveBuild, b *cloudbuildpb.Build, daemon string) error {
	imgs := buildImages(b)
	if len(imgs) == 0 {
		return nil
	}
	s.logs.line(r.ref, "PUSH")
	args := []string{"docker"}
	cfg, err := s.dockerConfig(ctx, b)
	if err != nil {
		return err
	}
	if cfg != nil {
		dir := "/gcpemu-auth/" + b.GetId()
		if err := rt.CopyTo(ctx, daemon, "/", []runtime.File{{Name: strings.TrimPrefix(dir, "/") + "/config.json", Mode: 0o600, Data: cfg}}); err != nil {
			return err
		}
		defer func() {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			_, _ = rt.Exec(rctx, daemon, []string{"rm", "-rf", dir}, nil)
		}()
		args = append(args, "--config", dir)
	}
	start := timestamppb.New(s.now())
	for _, img := range imgs {
		s.logs.line(r.ref, "Pushing "+img)
		at := timestamppb.New(s.now())
		res, err := rt.Exec(ctx, daemon, append(slices.Clone(args), "push", img), nil)
		if err != nil {
			return err
		}
		w := s.logs.writer(r.ref, "")
		_, _ = w.Write(res.Stdout)
		_, _ = w.Write(res.Stderr)
		w.flush()
		if res.ExitCode != 0 {
			return fmt.Errorf("%s: %s", img, lastLine(res.Stderr))
		}
		digest := pushedDigest(string(res.Stdout))
		if digest == "" {
			return fmt.Errorf("%s: docker push reported no digest", img)
		}
		pushed := &cloudbuildpb.BuiltImage{Name: img, Digest: digest, PushTiming: &cloudbuildpb.TimeSpan{StartTime: at, EndTime: timestamppb.New(s.now())}}
		s.setBuild(r, func(b *cloudbuildpb.Build) {
			if b.Results == nil {
				b.Results = &cloudbuildpb.Results{}
			}
			b.Results.Images = append(b.Results.Images, pushed)
		})
	}
	end := timestamppb.New(s.now())
	s.setBuild(r, func(b *cloudbuildpb.Build) {
		if b.Timing == nil {
			b.Timing = map[string]*cloudbuildpb.TimeSpan{}
		}
		b.Timing["PUSH"] = &cloudbuildpb.TimeSpan{StartTime: start, EndTime: end}
	})
	return nil
}

// digestLine is docker push's report of the manifest it pushed, and
// partialPush the digest it pushed in place of a multi-platform image's
// whose other platforms it does not have.
var (
	digestLine  = regexp.MustCompile(`digest: (sha256:[0-9a-f]{64})`)
	partialPush = regexp.MustCompile(`-> (sha256:[0-9a-f]{64})`)
)

// pushedDigest returns the digest docker push's output reports.
func pushedDigest(out string) string {
	for _, re := range []*regexp.Regexp{digestLine, partialPush} {
		if m := re.FindAllStringSubmatch(out, -1); len(m) > 0 {
			return m[len(m)-1][1]
		}
	}
	return ""
}

// lastLine returns the last non-empty line of out.
func lastLine(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// setBuild changes a WORKING Build, unless it was halted.
func (s *Service) setBuild(r *liveBuild, fn func(*cloudbuildpb.Build)) {
	if r.halted() {
		return
	}
	if err := s.update(r.ref, func(st *stored) { fn(st.build) }); err != nil {
		s.env.Log.Error("build: results", "build", r.ref.name(), "err", err)
	}
}

// splitLocation splits artifacts.objects.location, gs://BUCKET[/DIR], into
// the bucket and the object name prefix, "DIR/" or "".
func splitLocation(loc string) (bucket, prefix string) {
	rest, ok := strings.CutPrefix(loc, "gs://")
	if !ok {
		return "", ""
	}
	bucket, dir, _ := strings.Cut(rest, "/")
	if dir = strings.Trim(dir, "/"); dir != "" {
		dir += "/"
	}
	return bucket, dir
}

// artifactMatches reports whether rel, a file's path in /workspace,
// matches one of the artifacts.objects paths.
func artifactMatches(paths []string, rel string) bool {
	for _, p := range paths {
		p = strings.TrimPrefix(path.Clean(p), workspace+"/")
		if ok, _ := path.Match(p, rel); ok {
			return true
		}
	}
	return false
}

// manifestEntry is a line of an artifact manifest, as Cloud Build writes
// it: the object's location with its generation, and its MD5 (type 2).
type manifestEntry struct {
	Location string       `json:"location"`
	FileHash []fileHashes `json:"file_hash"`
}

type fileHashes struct {
	FileHash []fileHash `json:"file_hash"`
}

type fileHash struct {
	Type  int    `json:"type"`
	Value string `json:"value"`
}

// uploadArtifacts uploads the workspace files artifacts.objects names to
// its location, each by its base name, then the manifest listing them,
// artifacts-BUILD_ID.json, and records them in results.
func (s *Service) uploadArtifacts(ctx context.Context, rt *runtime.Manager, r *liveBuild, b *cloudbuildpb.Build, vols buildVolumes) error {
	objs := b.GetArtifacts().GetObjects()
	if objs == nil {
		return nil
	}
	store := s.objects()
	if store == nil {
		return fmt.Errorf("the gcs Service is not running")
	}
	bucket, prefix := splitLocation(objs.GetLocation())
	s.logs.line(r.ref, "Uploading artifacts to "+objs.GetLocation())
	start := timestamppb.New(s.now())
	var manifest bytes.Buffer
	n := 0
	err := s.withFetcher(ctx, rt, b, vols, runtime.ContainerSpec{Image: agentImage, Entrypoint: []string{"/.gcpemu-unused"}}, func(id string) error {
		pr, pw := io.Pipe()
		go func() { pw.CloseWithError(rt.ArchiveFrom(ctx, id, workspace, pw)) }()
		defer pr.Close()
		tr := tar.NewReader(pr)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			// Entries are named workspace/...
			_, rel, _ := strings.Cut(h.Name, "/")
			if h.Typeflag != tar.TypeReg || !artifactMatches(objs.GetPaths(), rel) {
				continue
			}
			name := prefix + path.Base(rel)
			gen, sum, err := store.WriteObject(ctx, bucket, name, "", tr)
			if err != nil {
				return fmt.Errorf("uploading %s: %w", rel, err)
			}
			at := fmt.Sprintf("gs://%s/%s#%d", bucket, name, gen)
			line, _ := json.Marshal(manifestEntry{Location: at, FileHash: []fileHashes{{[]fileHash{{Type: 2, Value: base64.StdEncoding.EncodeToString(sum)}}}}})
			manifest.Write(append(line, '\n'))
			s.logs.line(r.ref, "Uploaded "+rel+" to "+at)
			n++
		}
	})
	if err != nil {
		return err
	}
	name := prefix + "artifacts-" + b.GetId() + ".json"
	if _, _, err := store.WriteObject(ctx, bucket, name, "application/json", &manifest); err != nil {
		return fmt.Errorf("uploading the artifact manifest: %w", err)
	}
	timing := &cloudbuildpb.TimeSpan{StartTime: start, EndTime: timestamppb.New(s.now())}
	s.setBuild(r, func(b *cloudbuildpb.Build) {
		if b.Results == nil {
			b.Results = &cloudbuildpb.Results{}
		}
		b.Results.ArtifactManifest = "gs://" + bucket + "/" + name
		b.Results.NumArtifacts = int64(n)
		b.Results.ArtifactTiming = timing
		b.Artifacts.Objects.Timing = timing
	})
	return nil
}
