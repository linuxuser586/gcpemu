package build

import (
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// triggerEvent is what fires a Build trigger: a run's RepoSource, or the
// JSON body of a webhook or a Pub/Sub push, which the Build trigger's
// substitutions bind to.
type triggerEvent struct {
	repo *cloudbuildpb.RepoSource
	body any
}

// commitSHA is the form of a full commit hash.
var commitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// triggerBuild returns the Build a Build trigger starts for an event, and
// the built-in substitutions it provides: TRIGGER_NAME,
// TRIGGER_BUILD_CONFIG_PATH, and those of the revision it builds. The
// Build's source is the Build trigger's sourceToBuild, a public Git
// repository, or else its build's own; a run's RepoSource names the
// revision of a Git source. The Build template is the Build trigger's build,
// or the Build config file its filename or gitFileSource names, read
// from the repository.
func (s *Service) triggerBuild(ctx context.Context, t *cloudbuildpb.BuildTrigger, ev triggerEvent) (*cloudbuildpb.Build, map[string]string, error) {
	scm := t.GetGithub() != nil || t.GetTriggerTemplate() != nil || t.GetRepositoryEventConfig() != nil
	stb := t.GetSourceToBuild()
	if scm && stb == nil {
		return nil, nil, apierr.Unimplemented("Running a Build trigger on github, triggerTemplate or repositoryEventConfig events is not emulated without sourceToBuild.")
	}
	provided := map[string]string{"TRIGGER_NAME": t.GetName()}
	var git *cloudbuildpb.GitSource
	if stb != nil {
		if stb.GetUri() == "" {
			return nil, nil, apierr.Unimplemented("Build trigger sourceToBuild.repository and githubEnterpriseConfig are not emulated; use sourceToBuild.uri.")
		}
		git = &cloudbuildpb.GitSource{Url: stb.GetUri(), Revision: stb.GetRef()}
	}

	var b *cloudbuildpb.Build
	switch tmpl := t.GetBuildTemplate().(type) {
	case *cloudbuildpb.BuildTrigger_Build:
		b = proto.Clone(tmpl.Build).(*cloudbuildpb.Build)
		if g := b.GetSource().GetGitSource(); git == nil && g != nil {
			git = proto.Clone(g).(*cloudbuildpb.GitSource)
		}
	case *cloudbuildpb.BuildTrigger_Filename:
		if git == nil {
			return nil, nil, apierr.FailedPrecondition("Build trigger %q has a filename but no sourceToBuild to read it from.", t.GetResourceName())
		}
		provided["TRIGGER_BUILD_CONFIG_PATH"] = tmpl.Filename
	case *cloudbuildpb.BuildTrigger_GitFileSource:
		g := tmpl.GitFileSource
		if g.GetUri() == "" || g.GetRepository() != "" || g.GetGithubEnterpriseConfig() != "" {
			return nil, nil, apierr.Unimplemented("Build trigger gitFileSource is only emulated with a public uri.")
		}
		provided["TRIGGER_BUILD_CONFIG_PATH"] = g.GetPath()
	case *cloudbuildpb.BuildTrigger_Autodetect:
		return nil, nil, apierr.Unimplemented("Build trigger autodetect is not emulated; use build or filename.")
	}

	if rs := ev.repo; rs != nil {
		rev := cmp.Or(rs.GetCommitSha(), repoRef(rs))
		if git == nil && (rev != "" || rs.GetRepoName() != "") {
			return nil, nil, apierr.Unimplemented("Running a Build trigger at a RepoSource needs a gitSource or sourceToBuild; Cloud Source Repositories are not emulated.")
		}
		if git != nil && rev != "" {
			git.Revision = rev
		}
		if git != nil && rs.GetDir() != "" {
			git.Dir = rs.GetDir()
		}
	}
	if git != nil {
		maps.Copy(provided, revisionSubstitutions(git))
	}

	switch tmpl := t.GetBuildTemplate().(type) {
	case *cloudbuildpb.BuildTrigger_Filename:
		data, err := s.readGitFile(ctx, git.GetUrl(), git.GetRevision(), tmpl.Filename)
		if err != nil {
			return nil, nil, err
		}
		if b, err = parseBuildConfig(tmpl.Filename, data); err != nil {
			return nil, nil, err
		}
	case *cloudbuildpb.BuildTrigger_GitFileSource:
		g := tmpl.GitFileSource
		data, err := s.readGitFile(ctx, g.GetUri(), g.GetRevision(), g.GetPath())
		if err != nil {
			return nil, nil, err
		}
		if b, err = parseBuildConfig(g.GetPath(), data); err != nil {
			return nil, nil, err
		}
	}
	if git != nil {
		b.Source = &cloudbuildpb.Source{Source: &cloudbuildpb.Source_GitSource{GitSource: git}}
	}

	subs := maps.Clone(b.GetSubstitutions())
	if subs == nil {
		subs = map[string]string{}
	}
	for k, v := range t.GetSubstitutions() {
		subs[k] = bind(v, ev.body)
	}
	maps.Copy(subs, ev.repo.GetSubstitutions())
	b.Substitutions = subs
	b.BuildTriggerId = t.GetId()
	b.Tags = append(b.Tags, "trigger-"+t.GetId())
	if t.GetServiceAccount() != "" {
		b.ServiceAccount = t.GetServiceAccount()
	}
	return b, provided, nil
}

// repoRef returns the Git ref a RepoSource's branch or tag names.
func repoRef(rs *cloudbuildpb.RepoSource) string {
	switch {
	case rs.GetBranchName() != "":
		return "refs/heads/" + rs.GetBranchName()
	case rs.GetTagName() != "":
		return "refs/tags/" + rs.GetTagName()
	}
	return ""
}

// revisionSubstitutions returns the built-in substitutions a Git source
// provides before it is fetched: the repository's names and those of the
// revision, a branch or tag ref or a commit.
func revisionSubstitutions(g *cloudbuildpb.GitSource) map[string]string {
	vars := map[string]string{}
	if repo := strings.TrimSuffix(strings.TrimSuffix(g.GetUrl(), "/"), ".git"); repo != "" {
		if i := strings.Index(repo, "://"); i >= 0 {
			repo = repo[i+3:]
		}
		if _, p, ok := strings.Cut(repo, "/"); ok {
			vars["REPO_FULL_NAME"] = p
		}
		vars["REPO_NAME"] = path.Base(repo)
	}
	rev := g.GetRevision()
	switch {
	case commitSHA.MatchString(rev):
		vars["COMMIT_SHA"], vars["REVISION_ID"] = rev, rev
	case strings.HasPrefix(rev, "refs/heads/"):
		vars["BRANCH_NAME"] = strings.TrimPrefix(rev, "refs/heads/")
		vars["REF_NAME"] = vars["BRANCH_NAME"]
	case strings.HasPrefix(rev, "refs/tags/"):
		vars["TAG_NAME"] = strings.TrimPrefix(rev, "refs/tags/")
		vars["REF_NAME"] = vars["TAG_NAME"]
	case rev != "":
		vars["BRANCH_NAME"], vars["REF_NAME"] = rev, rev
	}
	return vars
}

// jsonText is JSON in an event's body: its value, which a binding walks
// into, and its text, which a binding to it gives.
type jsonText struct {
	text  string
	value any
}

// binding is a payload binding in a Build trigger's substitution value.
var binding = regexp.MustCompile(`\$\(([^()]*)\)`)

// bind expands the payload bindings in v, $(body.a.b), with the values
// at those paths of an event's body: a string as it is, anything else as
// JSON. A path the body lacks, or a binding to anything but the body,
// expands to empty.
func bind(v string, body any) string {
	return binding.ReplaceAllStringFunc(v, func(m string) string {
		p := strings.Split(binding.FindStringSubmatch(m)[1], ".")
		if p[0] != "body" {
			return ""
		}
		cur := body
		for _, k := range p[1:] {
			if j, ok := cur.(jsonText); ok {
				cur = j.value
			}
			switch c := cur.(type) {
			case map[string]any:
				cur = c[k]
			case []any:
				i, err := strconv.Atoi(k)
				if err != nil || i < 0 || i >= len(c) {
					return ""
				}
				cur = c[i]
			default:
				return ""
			}
		}
		switch c := cur.(type) {
		case nil:
			return ""
		case string:
			return c
		case jsonText:
			return c.text
		default:
			b, _ := json.Marshal(c)
			return string(b)
		}
	})
}

// parseBuildConfig parses a Build config file, YAML or JSON.
func parseBuildConfig(name string, data []byte) (*cloudbuildpb.Build, error) {
	var v any
	if err := yaml.Unmarshal(data, &v); err != nil {
		return nil, apierr.InvalidArgument("Build config %s is not valid YAML: %v", name, err)
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, apierr.InvalidArgument("Build config %s is not a mapping.", name)
	}
	j, err := json.Marshal(v)
	if err != nil {
		return nil, apierr.InvalidArgument("Build config %s: %v", name, err)
	}
	b := &cloudbuildpb.Build{}
	if err := protojson.Unmarshal(j, b); err != nil {
		return nil, apierr.InvalidArgument("Build config %s is not a valid Build: %v", name, err)
	}
	return b, nil
}

// configFile is where the Build config fetcher copies the file it reads.
const configFile = "/.gcpemu-config"

// readGitFile reads a file of a public Git repository at revision (the
// default branch when empty), cloning it in a container as fetchGit
// does.
func (s *Service) readGitFile(ctx context.Context, url, revision, file string) ([]byte, error) {
	if s.env.Containers == nil {
		return nil, apierr.FailedPrecondition("Reading a Build config file from Git needs a container runtime.")
	}
	rt, err := s.env.Containers.Runtime(ctx)
	if err != nil {
		return nil, apierr.FailedPrecondition("Reading a Build config file from Git needs a container runtime: %v", err)
	}
	if err := rt.EnsureImage(ctx, gitImage, rt.Offline); err != nil {
		return nil, apierr.New(codes.Unavailable, "Pulling %s: %v", gitImage, err)
	}
	script := strings.Join([]string{
		"set -e",
		"cd /tmp && git init -q src && cd src",
		`git remote add origin "$URL"`,
		`if git fetch -q --depth=1 origin "${REV:-HEAD}"; then git checkout -q FETCH_HEAD`,
		`else git fetch -q origin '+refs/heads/*:refs/remotes/origin/*' '+refs/tags/*:refs/tags/*' && git checkout -q "$REV"; fi`,
		`cp -- "$FILE" ` + configFile,
	}, "\n")
	spec := runtime.ContainerSpec{
		Name:       rt.Name("build", "config", s.env.IDs.UUID()[:8]),
		Labels:     rt.Labels("build", "config", "fetch"),
		Image:      gitImage,
		Entrypoint: []string{"/bin/sh", "-c", script},
		Env:        []string{"URL=" + url, "REV=" + revision, "FILE=" + path.Clean("./"+file), "HOME=/tmp", "GIT_TERMINAL_PROMPT=0"},
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	id, err := rt.CreateContainer(cctx, spec)
	if err != nil {
		return nil, apierr.New(codes.Unavailable, "Reading the Build config file: %v", err)
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		_ = rt.RemoveContainer(rctx, id, true)
	}()
	if err := rt.StartContainer(cctx, id); err != nil {
		return nil, apierr.New(codes.Unavailable, "Reading the Build config file: %v", err)
	}
	code, err := rt.WaitContainer(cctx, id)
	if err != nil {
		return nil, apierr.New(codes.Unavailable, "Reading the Build config file: %v", err)
	}
	if code != 0 {
		return nil, apierr.FailedPrecondition("Build config file %s could not be read from %s at %q.", file, url, revision)
	}
	data, err := rt.CopyFrom(cctx, id, configFile)
	if err != nil {
		return nil, apierr.New(codes.Unavailable, "Reading the Build config file: %v", err)
	}
	return data, nil
}
