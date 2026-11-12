package ar_test

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// #1062: an update_mask path naming one map key (AIP-161), such as
// labels.env or cleanup_policies.`delete-old`, changes that key only.

// keyEnv starts the emulator with an "images" repository that holds
// cleanup policies a and b and labels env and team.
func keyEnv(t *testing.T) *env {
	t.Helper()
	e := start(t)
	_, err := e.createModeRepo("images", &artifactregistrypb.Repository{
		Labels: map[string]string{"env": "dev", "team": "core"},
		CleanupPolicies: map[string]*artifactregistrypb.CleanupPolicy{
			"a": deletePolicy("a", &artifactregistrypb.CleanupPolicyCondition{PackageNamePrefixes: []string{"a"}}),
			"b": deletePolicy("b", &artifactregistrypb.CleanupPolicyCondition{PackageNamePrefixes: []string{"b"}}),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// update sends UpdateRepository for "images" with the given mask paths.
func (e *env) update(in *artifactregistrypb.Repository, paths ...string) error {
	e.t.Helper()
	in.Name = repoName(testLoc, "images")
	_, err := e.c.UpdateRepository(e.ctx, &artifactregistrypb.UpdateRepositoryRequest{
		Repository: in,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: paths},
	})
	return err
}

// stored returns the "images" repository as GetRepository sees it.
func (e *env) stored() *artifactregistrypb.Repository {
	e.t.Helper()
	r, err := e.c.GetRepository(e.ctx, &artifactregistrypb.GetRepositoryRequest{Name: repoName(testLoc, "images")})
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

// A mask path naming one policy changes that policy and keeps the others.
func TestMaskKeySetsOnePolicy(t *testing.T) {
	e := keyEnv(t)
	changed := deletePolicy("a", &artifactregistrypb.CleanupPolicyCondition{PackageNamePrefixes: []string{"changed"}})
	if err := e.update(&artifactregistrypb.Repository{CleanupPolicies: map[string]*artifactregistrypb.CleanupPolicy{"a": changed}}, "cleanup_policies.a"); err != nil {
		t.Fatal(err)
	}
	ps := e.stored().GetCleanupPolicies()
	if len(ps) != 2 || ps["b"] == nil || ps["a"].GetCondition().GetPackageNamePrefixes()[0] != "changed" {
		t.Fatalf("policies = %v, want a changed and b kept", ps)
	}
}

// A mask path naming a policy the request lacks removes that policy.
func TestMaskKeyRemovesOnePolicy(t *testing.T) {
	e := keyEnv(t)
	if err := e.update(&artifactregistrypb.Repository{}, "cleanup_policies.b"); err != nil {
		t.Fatal(err)
	}
	if ps := e.stored().GetCleanupPolicies(); len(ps) != 1 || ps["a"] == nil {
		t.Fatalf("policies = %v, want only a", ps)
	}
}

// A mask path naming one label sets it, keeps the others, and is a no-op
// for a key the request and the repository both lack.
func TestMaskKeyLabels(t *testing.T) {
	e := keyEnv(t)
	if err := e.update(&artifactregistrypb.Repository{Labels: map[string]string{"env": "prod", "other": "x"}}, "labels.env", "labels.gone"); err != nil {
		t.Fatal(err)
	}
	if l := e.stored().GetLabels(); len(l) != 2 || l["env"] != "prod" || l["team"] != "core" {
		t.Fatalf("labels = %v, want env=prod team=core", l)
	}
}

// When a mask names a whole map and one of its keys, the whole map is
// replaced.
func TestMaskKeyWholeFieldWins(t *testing.T) {
	e := keyEnv(t)
	if err := e.update(&artifactregistrypb.Repository{Labels: map[string]string{"new": "1"}}, "labels.env", "labels"); err != nil {
		t.Fatal(err)
	}
	if l := e.stored().GetLabels(); len(l) != 1 || l["new"] != "1" {
		t.Fatalf("labels = %v, want only new=1", l)
	}
}

// A key may be bare or backtick-quoted (AIP-161), and is used exactly as
// written: only the field name is converted from lowerCamel.
func TestMaskKeySpelling(t *testing.T) {
	for _, tc := range []struct{ path, key string }{
		{"cleanup_policies.`delete-old`", "delete-old"},
		{"cleanupPolicies.delete-old", "delete-old"},
		{"cleanup_policies.MyPolicy", "MyPolicy"},
		{"cleanupPolicies.`MyPolicy`", "MyPolicy"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			e := keyEnv(t)
			p := deletePolicy(tc.key, &artifactregistrypb.CleanupPolicyCondition{})
			if err := e.update(&artifactregistrypb.Repository{CleanupPolicies: map[string]*artifactregistrypb.CleanupPolicy{tc.key: p}}, tc.path); err != nil {
				t.Fatal(err)
			}
			if ps := e.stored().GetCleanupPolicies(); len(ps) != 3 || ps[tc.key] == nil {
				t.Fatalf("policies = %v, want a, b and %s", ps, tc.key)
			}
		})
	}
}

// A path with more than one segment after a map key, or a key path on a
// field that is not a map, is INVALID_ARGUMENT and changes nothing.
func TestMaskKeyBadPaths(t *testing.T) {
	for _, path := range []string{"labels.env.x", "description.x", "docker_config.immutable_tags", "labels.", "labels.`env", "labels.`env`.x"} {
		t.Run(path, func(t *testing.T) {
			e := keyEnv(t)
			err := e.update(&artifactregistrypb.Repository{Description: "changed", Labels: map[string]string{"env": "prod"}}, path)
			if code(err) != codes.InvalidArgument {
				t.Fatalf("UpdateRepository = %v, want INVALID_ARGUMENT", err)
			}
			if r := e.stored(); r.GetDescription() != "" || r.GetLabels()["env"] != "dev" {
				t.Fatalf("repository changed: %v", r)
			}
		})
	}
}

// The cleanup-policy checks see the merged map: an 11th policy added
// through one key is INVALID_ARGUMENT and the 10 stored policies remain.
func TestMaskKeyPolicyLimit(t *testing.T) {
	e := start(t)
	ten := map[string]*artifactregistrypb.CleanupPolicy{}
	for i := range 10 {
		id := fmt.Sprintf("p%d", i)
		ten[id] = deletePolicy(id, &artifactregistrypb.CleanupPolicyCondition{})
	}
	if _, err := e.createModeRepo("images", &artifactregistrypb.Repository{CleanupPolicies: ten}); err != nil {
		t.Fatal(err)
	}
	extra := map[string]*artifactregistrypb.CleanupPolicy{"p10": deletePolicy("p10", &artifactregistrypb.CleanupPolicyCondition{})}
	if err := e.update(&artifactregistrypb.Repository{CleanupPolicies: extra}, "cleanup_policies.p10"); code(err) != codes.InvalidArgument {
		t.Fatalf("UpdateRepository = %v, want INVALID_ARGUMENT", err)
	}
	if ps := e.stored().GetCleanupPolicies(); len(ps) != 10 || ps["p10"] != nil {
		t.Fatalf("policies = %v, want the 10 stored ones", ps)
	}
}

// An invalid policy sent through a key path is refused like a whole-map
// update.
func TestMaskKeyInvalidPolicy(t *testing.T) {
	for name, p := range map[string]*artifactregistrypb.CleanupPolicy{
		"key disagrees with id": deletePolicy("other", &artifactregistrypb.CleanupPolicyCondition{}),
		"no condition":          {Id: "a", Action: artifactregistrypb.CleanupPolicy_DELETE},
		"action unset":          {Id: "a", ConditionType: &artifactregistrypb.CleanupPolicy_Condition{Condition: &artifactregistrypb.CleanupPolicyCondition{}}},
	} {
		t.Run(name, func(t *testing.T) {
			e := keyEnv(t)
			bad := map[string]*artifactregistrypb.CleanupPolicy{"a": p}
			whole := e.update(&artifactregistrypb.Repository{CleanupPolicies: bad}, "cleanup_policies")
			err := e.update(&artifactregistrypb.Repository{CleanupPolicies: bad}, "cleanup_policies.a")
			if code(err) != codes.InvalidArgument || whole == nil || err.Error() != whole.Error() {
				t.Fatalf("key-path UpdateRepository = %v, want INVALID_ARGUMENT like the whole-map %v", err, whole)
			}
			if ps := e.stored().GetCleanupPolicies(); ps["a"].GetCondition().GetPackageNamePrefixes()[0] != "a" {
				t.Fatalf("policy a changed: %v", ps["a"])
			}
		})
	}
}

// The REST gateway applies a key path the same way as gRPC.
func TestMaskKeyREST(t *testing.T) {
	for _, mask := range []string{"cleanupPolicies.a", url.QueryEscape("cleanupPolicies.`a`")} {
		t.Run(mask, func(t *testing.T) {
			e := keyEnv(t)
			st, body := rest(t, e, "PATCH", "/v1/"+repoName(testLoc, "images")+"?updateMask="+mask,
				`{"cleanupPolicies":{"a":{"id":"a","action":"DELETE","condition":{"packageNamePrefixes":["changed"]}}}}`)
			if st != http.StatusOK {
				t.Fatalf("REST PATCH = %d %v", st, body)
			}
			ps := e.stored().GetCleanupPolicies()
			if len(ps) != 2 || ps["b"] == nil || ps["a"].GetCondition().GetPackageNamePrefixes()[0] != "changed" {
				t.Fatalf("policies = %v, want a changed and b kept", ps)
			}
		})
	}
}

// A whole-map update with an invalid policy is INVALID_ARGUMENT even when
// the repository does not exist: the request is checked before the store.
func TestMaskInvalidPoliciesBeforeNotFound(t *testing.T) {
	e := start(t)
	bad := map[string]*artifactregistrypb.CleanupPolicy{"a": deletePolicy("other", &artifactregistrypb.CleanupPolicyCondition{})}
	if err := e.update(&artifactregistrypb.Repository{CleanupPolicies: bad}, "cleanup_policies"); code(err) != codes.InvalidArgument {
		t.Fatalf("UpdateRepository = %v, want INVALID_ARGUMENT", err)
	}
}
