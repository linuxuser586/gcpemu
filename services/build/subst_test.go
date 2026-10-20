package build_test

import (
	"strings"
	"testing"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Built-in and user substitutions expand in a step's args and in images,
// in both $_KEY and ${_KEY} forms, and $$ escapes a literal $; the step
// sees the expanded args.
func TestSubstitutionsExpand(t *testing.T) {
	e := start(t)
	op, b := e.create(&cloudbuildpb.Build{
		Substitutions: map[string]string{"_TAG": "v1"},
		Steps:         []*cloudbuildpb.BuildStep{{Name: busybox, Args: []string{"echo", "$PROJECT_ID/$BUILD_ID:${_TAG}-$$HOME"}}},
		Images:        []string{"us-docker.pkg.dev/$PROJECT_ID/repo/app:$_TAG"},
	})
	want := testProject + "/" + b.GetId() + ":v1-$HOME"
	if got := b.GetSteps()[0].GetArgs()[1]; got != want {
		t.Errorf("args[1] = %q, want %q", got, want)
	}
	if got, want := b.GetImages()[0], "us-docker.pkg.dev/"+testProject+"/repo/app:v1"; got != want {
		t.Errorf("images[0] = %q, want %q", got, want)
	}
	if got := b.GetSubstitutions()["_TAG"]; got != "v1" {
		t.Errorf("substitutions = %v, want the user's kept", b.GetSubstitutions())
	}
	// The image is never built, so the push fails; the step ran first.
	_, _ = op.Wait(e.ctx)
	if log := e.log(e.get(b.GetId())); !strings.Contains(log, "Step #0: "+want) {
		t.Errorf("log lacks the expanded args:\n%s", log)
	}
}

// createErr returns the error builds.create answers for b.
func (e *env) createErr(b *cloudbuildpb.Build) error {
	e.t.Helper()
	_, err := e.cb.CreateBuild(e.ctx, &cloudbuildpb.CreateBuildRequest{ProjectId: testProject, Build: b})
	return err
}

// wantInvalid fails unless err is INVALID_ARGUMENT with message msg.
func wantInvalid(t *testing.T, err error, msg string) {
	t.Helper()
	if st, _ := status.FromError(err); st.Code() != codes.InvalidArgument || st.Message() != msg {
		t.Errorf("err = %v, want INVALID_ARGUMENT %q", err, msg)
	}
}

// Under MUST_MATCH, the default, builds.create refuses a user substitution
// the template does not use and a $_KEY in it with no value; ALLOW_LOOSE
// allows both, and the unset $_KEY expands to empty. A key that is neither
// a user substitution nor a built-in is refused either way.
//
// The texts are those of Google's cloud-build-local (validate.go,
// CheckSubstitutionTemplate), which gcloud users report from the real API,
// e.g. `INVALID_ARGUMENT: invalid build: key "_KUBE_CROSS_VERSION" in the
// substitution data is not matched in the template`
// (github.com/kubernetes/release/issues/724) and `key in the template
// "_BRANCH" is not matched in the substitution data; substitutions =
// map[...]` (github.com/GoogleCloudPlatform/cloud-build-local/issues/66).
func TestSubstitutionOption(t *testing.T) {
	e := start(t)
	build := func(opt cloudbuildpb.BuildOptions_SubstitutionOption) *cloudbuildpb.Build {
		return &cloudbuildpb.Build{
			Substitutions: map[string]string{"_UNUSED": "x", "_USED": "y"},
			Options:       &cloudbuildpb.BuildOptions{SubstitutionOption: opt},
			Steps:         []*cloudbuildpb.BuildStep{{Name: busybox, Args: []string{"echo", "$_USED[$_MISSING]"}}},
		}
	}
	wantInvalid(t, e.createErr(build(cloudbuildpb.BuildOptions_MUST_MATCH)),
		`invalid build: key in the template "_MISSING" is not matched in the substitution data; substitutions = map[_UNUSED:x _USED:y];`+
			`key "_UNUSED" in the substitution data is not matched in the template`)

	op, b := e.create(build(cloudbuildpb.BuildOptions_ALLOW_LOOSE))
	if got := b.GetSteps()[0].GetArgs()[1]; got != "y[]" {
		t.Errorf("loose args[1] = %q, want %q", got, "y[]")
	}
	if b, err := op.Wait(e.ctx); err != nil {
		t.Errorf("loose Build: %v (%s)", err, b.GetStatusDetail())
	}

	for _, opt := range []cloudbuildpb.BuildOptions_SubstitutionOption{cloudbuildpb.BuildOptions_MUST_MATCH, cloudbuildpb.BuildOptions_ALLOW_LOOSE} {
		b := build(opt)
		b.Substitutions = nil
		b.Steps[0].Args = []string{"echo", "$HOME"}
		wantInvalid(t, e.createErr(b), `invalid build: key in the template "HOME" is not a valid built-in substitution`)
	}

	b = build(cloudbuildpb.BuildOptions_ALLOW_LOOSE)
	b.Substitutions["lower"] = "z"
	wantInvalid(t, e.createErr(b), `invalid build: invalid .substitutions field: substitution key "lower" does not respect format "^_[A-Z0-9_]+$" and is not an overridable built-in substitutions`)
}

// With dynamicSubstitutions, ${KEY...} takes bash's parameter expansions,
// and a user value may refer to other substitutions; without it, such an
// expression is left as it is.
func TestDynamicSubstitutions(t *testing.T) {
	e := start(t)
	subs := map[string]string{"_SHA": "0123456789abcdef", "_REF": "refs/heads/Main", "_IMAGE": "${_REPO}/app:${_SHA:0:7}", "_REPO": "us-docker.pkg.dev/$PROJECT_ID/r"}
	args := []string{
		"${_SHA:0:7}", "${_SHA: -4}", "${_REF##*/}", "${_REF#refs/}", "${_REF%/*}", "${_REF/heads/tags}",
		"${_REF//e/E}", "${_REF,,}", "${_REF^^}", "${#_SHA}", "$_IMAGE", "${_MISSING:-dflt}",
		"${_MISSING-unset}", "${_MISSING+set}", "${_SHA+set}", "${_REF/#/x}", "${_REF^}",
	}
	want := []string{
		"0123456", "cdef", "Main", "heads/Main", "refs/heads", "refs/tags/Main",
		"rEfs/hEads/Main", "refs/heads/main", "REFS/HEADS/MAIN", "16", "us-docker.pkg.dev/" + testProject + "/r/app:0123456", "dflt",
		"unset", "", "set", "xrefs/heads/Main", "Refs/heads/Main",
	}
	_, b := e.create(&cloudbuildpb.Build{
		Substitutions: subs,
		Options:       &cloudbuildpb.BuildOptions{DynamicSubstitutions: true, SubstitutionOption: cloudbuildpb.BuildOptions_ALLOW_LOOSE},
		Steps:         []*cloudbuildpb.BuildStep{{Name: busybox, Args: append([]string{"echo"}, args...)}},
	})
	for i, w := range want {
		if got := b.GetSteps()[0].GetArgs()[i+1]; got != w {
			t.Errorf("%s = %q, want %q", args[i], got, w)
		}
	}

	_, b = e.create(&cloudbuildpb.Build{
		Substitutions: map[string]string{"_SHA": "0123456789"},
		Steps:         []*cloudbuildpb.BuildStep{{Name: busybox, Args: []string{"echo", "${_SHA:0:7}", "$_SHA"}}},
	})
	if got := b.GetSteps()[0].GetArgs()[1]; got != "${_SHA:0:7}" {
		t.Errorf("without dynamicSubstitutions: %q, want it as it is", got)
	}
}

// A Build is validated as substituted: a step name that comes of an
// unset substitution is refused, and an artifacts location that comes of
// one is accepted.
func TestValidatedAsSubstituted(t *testing.T) {
	e := start(t)
	wantInvalid(t, e.createErr(&cloudbuildpb.Build{
		Options: &cloudbuildpb.BuildOptions{SubstitutionOption: cloudbuildpb.BuildOptions_ALLOW_LOOSE},
		Steps:   []*cloudbuildpb.BuildStep{{Name: "$_IMG"}},
	}), "invalid build: build step 0 must specify name.")
	_, b := e.create(&cloudbuildpb.Build{
		Substitutions: map[string]string{"_LOC": "gs://out/dir"},
		Steps:         []*cloudbuildpb.BuildStep{{Name: busybox, Args: []string{"true"}}},
		Artifacts:     &cloudbuildpb.Artifacts{Objects: &cloudbuildpb.Artifacts_ArtifactObjects{Location: "$_LOC", Paths: []string{"x"}}},
	})
	if got := b.GetArtifacts().GetObjects().GetLocation(); got != "gs://out/dir" {
		t.Errorf("location = %q", got)
	}
}
