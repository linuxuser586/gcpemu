package build_test

import (
	"fmt"
	"testing"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// #1090: an update_mask path naming one map key (AIP-161, ADR 0075), such
// as substitutions._ENV or annotations.env, changes that key only.

// keyTrigger creates Build trigger "keys" with substitutions _ENV and
// _REGION.
func keyTrigger(e *env) *cloudbuildpb.BuildTrigger {
	e.t.Helper()
	return e.createTrigger(&cloudbuildpb.BuildTrigger{Name: "keys", BuildTemplate: &cloudbuildpb.BuildTrigger_Build{Build: echoBuild("x")},
		Substitutions: map[string]string{"_ENV": "dev", "_REGION": "eu"}})
}

// updateTrigger sends UpdateBuildTrigger for tr with the given mask paths.
func (e *env) updateTrigger(tr, in *cloudbuildpb.BuildTrigger, paths ...string) (*cloudbuildpb.BuildTrigger, error) {
	e.t.Helper()
	return e.cb.UpdateBuildTrigger(e.ctx, &cloudbuildpb.UpdateBuildTriggerRequest{ProjectId: testProject, TriggerId: tr.GetId(),
		Trigger: in, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
}

// storedTrigger returns tr as GetBuildTrigger sees it.
func (e *env) storedTrigger(tr *cloudbuildpb.BuildTrigger) *cloudbuildpb.BuildTrigger {
	e.t.Helper()
	got, err := e.cb.GetBuildTrigger(e.ctx, &cloudbuildpb.GetBuildTriggerRequest{ProjectId: testProject, TriggerId: tr.GetId()})
	if err != nil {
		e.t.Fatalf("GetBuildTrigger: %v", err)
	}
	return got
}

// A mask path naming one substitution sets it and keeps the others; the
// key is used as written, with no case conversion.
func TestMaskKeySubstitution(t *testing.T) {
	e := start(t)
	tr := keyTrigger(e)
	if _, err := e.updateTrigger(tr, &cloudbuildpb.BuildTrigger{Substitutions: map[string]string{"_ENV": "prod", "_OTHER": "x"}}, "substitutions._ENV"); err != nil {
		t.Fatal(err)
	}
	if s := e.storedTrigger(tr).GetSubstitutions(); len(s) != 2 || s["_ENV"] != "prod" || s["_REGION"] != "eu" {
		t.Fatalf("substitutions = %v, want _ENV=prod _REGION=eu", s)
	}
}

// A mask path naming an annotation the request lacks removes it from the
// worker pool and keeps the others.
func TestMaskKeyPoolAnnotation(t *testing.T) {
	e := start(t)
	op, err := e.cb.CreateWorkerPool(e.ctx, &cloudbuildpb.CreateWorkerPoolRequest{Parent: "projects/" + testProject + "/locations/us-central1", WorkerPoolId: "keys",
		WorkerPool: &cloudbuildpb.WorkerPool{Annotations: map[string]string{"a": "1", "b": "2"}}})
	if err != nil {
		t.Fatal(err)
	}
	wp, err := op.Wait(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	up, err := e.cb.UpdateWorkerPool(e.ctx, &cloudbuildpb.UpdateWorkerPoolRequest{WorkerPool: &cloudbuildpb.WorkerPool{Name: wp.GetName()},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"annotations.a"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := up.Wait(e.ctx); err != nil {
		t.Fatal(err)
	}
	got, err := e.cb.GetWorkerPool(e.ctx, &cloudbuildpb.GetWorkerPoolRequest{Name: wp.GetName()})
	if a := got.GetAnnotations(); err != nil || len(a) != 1 || a["b"] != "2" {
		t.Fatalf("annotations = %v, %v; want only b=2", a, err)
	}
}

// A mask naming a whole map and keys of it replaces the whole map, in
// either order.
func TestMaskKeyWholeFieldWins(t *testing.T) {
	e := start(t)
	tr := keyTrigger(e)
	for _, paths := range [][]string{{"substitutions._REGION", "substitutions"}, {"substitutions", "substitutions._REGION"}} {
		if _, err := e.updateTrigger(tr, &cloudbuildpb.BuildTrigger{Substitutions: map[string]string{"_NEW": "n"}}, paths...); err != nil {
			t.Fatal(err)
		}
		if s := e.storedTrigger(tr).GetSubstitutions(); len(s) != 1 || s["_NEW"] != "n" {
			t.Fatalf("%v: substitutions = %v, want only _NEW=n", paths, s)
		}
	}
}

// Substitution checks run on the merged map: a key path cannot add a key
// the trigger refuses, nor push the map past the limit.
func TestMaskKeyValidatesMergedMap(t *testing.T) {
	e := start(t)
	tr := keyTrigger(e)
	if _, err := e.updateTrigger(tr, &cloudbuildpb.BuildTrigger{Substitutions: map[string]string{"lower": "x"}}, "substitutions.lower"); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("substitutions.lower: %v, want InvalidArgument", err)
	}
	// 199 new keys alone are within the limit of 200, but not with the
	// trigger's two.
	in := map[string]string{}
	var paths []string
	for i := range 199 {
		k := fmt.Sprintf("_K%d", i)
		in[k] = "v"
		paths = append(paths, "substitutions."+k)
	}
	if _, err := e.updateTrigger(tr, &cloudbuildpb.BuildTrigger{Substitutions: in}, paths...); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("201 substitutions: %v, want InvalidArgument", err)
	}
	if s := e.storedTrigger(tr).GetSubstitutions(); len(s) != 2 {
		t.Fatalf("substitutions = %v, want the two unchanged", s)
	}
}

// A key path on a field that is not a map, or with segments after the
// key, is InvalidArgument and changes nothing; a backtick-quoted key may
// hold dots.
func TestMaskKeyPaths(t *testing.T) {
	e := start(t)
	tr := keyTrigger(e)
	for _, p := range []string{"description.x", "tags.x", "substitutions._ENV.x", "substitutions.`_ENV", "substitutions."} {
		if _, err := e.updateTrigger(tr, &cloudbuildpb.BuildTrigger{Description: "d", Substitutions: map[string]string{"_ENV": "p"}}, p); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%q: %v, want InvalidArgument", p, err)
		}
	}
	if got := e.storedTrigger(tr); got.GetDescription() != "" || got.GetSubstitutions()["_ENV"] != "dev" {
		t.Fatalf("trigger changed: %v", got)
	}
	if _, err := e.updateTrigger(tr, &cloudbuildpb.BuildTrigger{Tags: []string{"t"}, BuildTemplate: &cloudbuildpb.BuildTrigger_Build{Build: &cloudbuildpb.Build{Substitutions: map[string]string{"_A.B": "q"}}}},
		"build.substitutions.`_A.B`", "build.steps", "tags"); err != nil {
		t.Fatal(err)
	}
	if b := e.storedTrigger(tr).GetBuild(); b.GetSubstitutions()["_A.B"] != "q" || len(b.GetSteps()) != 0 {
		t.Fatalf("build = %v, want substitution _A.B=q and no steps", b)
	}
}
