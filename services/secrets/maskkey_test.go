package secrets_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// #1086: an update_mask path naming one map key (AIP-161, ADR 0075), such
// as labels.env or version_aliases.prod, changes that key only.

// keySecret creates secret "db" with labels env and team, annotations a
// and b, versions 1 and 2, and aliases prod (1) and stage (2).
func keySecret(t *testing.T) *env {
	t.Helper()
	e := start(t, nil)
	must(e.sm.CreateSecret(e.ctx, &secretmanagerpb.CreateSecretRequest{Parent: parent, SecretId: "db", Secret: &secretmanagerpb.Secret{
		Replication: automatic,
		Labels:      map[string]string{"env": "dev", "team": "core"},
		Annotations: map[string]string{"a": "1", "b": "2"},
	}}))
	for range 2 {
		must(e.sm.AddSecretVersion(e.ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: parent + "/secrets/db", Payload: &secretmanagerpb.SecretPayload{Data: []byte("x")}}))
	}
	if err := e.update(&secretmanagerpb.Secret{VersionAliases: map[string]int64{"prod": 1, "stage": 2}}, "version_aliases"); err != nil {
		t.Fatal(err)
	}
	return e
}

// update sends UpdateSecret for "db" with the given mask paths.
func (e *env) update(in *secretmanagerpb.Secret, paths ...string) error {
	e.t.Helper()
	in.Name = parent + "/secrets/db"
	_, err := e.sm.UpdateSecret(e.ctx, &secretmanagerpb.UpdateSecretRequest{Secret: in, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
	return err
}

// stored returns secret "db" as GetSecret sees it.
func (e *env) stored() *secretmanagerpb.Secret {
	e.t.Helper()
	return must(e.sm.GetSecret(e.ctx, &secretmanagerpb.GetSecretRequest{Name: parent + "/secrets/db"}))
}

// A mask path naming one label sets it, keeps the others, and is a no-op
// for a key the request and the secret both lack.
func TestMaskKeyLabels(t *testing.T) {
	e := keySecret(t)
	if err := e.update(&secretmanagerpb.Secret{Labels: map[string]string{"env": "prod", "other": "x"}}, "labels.env", "labels.gone"); err != nil {
		t.Fatal(err)
	}
	if l := e.stored().GetLabels(); len(l) != 2 || l["env"] != "prod" || l["team"] != "core" {
		t.Fatalf("labels = %v, want env=prod team=core", l)
	}
}

// Annotation and alias key paths set a key the request holds and remove
// one it lacks, keeping the others.
func TestMaskKeyAnnotationsAndAliases(t *testing.T) {
	e := keySecret(t)
	if err := e.update(&secretmanagerpb.Secret{
		Annotations:    map[string]string{"a": "changed"},
		VersionAliases: map[string]int64{"stage": 1},
	}, "annotations.a", "annotations.b", "versionAliases.stage"); err != nil {
		t.Fatal(err)
	}
	s := e.stored()
	if a := s.GetAnnotations(); len(a) != 1 || a["a"] != "changed" {
		t.Fatalf("annotations = %v, want only a=changed", a)
	}
	if v := s.GetVersionAliases(); len(v) != 2 || v["prod"] != 1 || v["stage"] != 1 {
		t.Fatalf("aliases = %v, want prod=1 stage=1", v)
	}
	if err := e.update(&secretmanagerpb.Secret{}, "version_aliases.prod"); err != nil {
		t.Fatal(err)
	}
	if v := e.stored().GetVersionAliases(); len(v) != 1 || v["stage"] != 1 {
		t.Fatalf("aliases = %v, want only stage=1", v)
	}
}

// A key is bare or backtick-quoted and used exactly as written: only the
// field name is converted from lowerCamel.
func TestMaskKeySpelling(t *testing.T) {
	for _, tc := range []struct{ path, key string }{
		{"labels.`my-key`", "my-key"},
		{"labels.my_key", "my_key"},
		{"annotations.myKey", "myKey"},
		{"annotations.`myKey`", "myKey"},
		{"annotations.`a.b/c`", "a.b/c"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			e := keySecret(t)
			in := &secretmanagerpb.Secret{Labels: map[string]string{tc.key: "v"}, Annotations: map[string]string{tc.key: "v"}}
			if err := e.update(in, tc.path); err != nil {
				t.Fatal(err)
			}
			s := e.stored()
			got := s.GetLabels()
			if strings.HasPrefix(tc.path, "annotations.") {
				got = s.GetAnnotations()
			}
			if len(got) != 3 || got[tc.key] != "v" {
				t.Fatalf("map = %v, want the two stored keys and %s=v", got, tc.key)
			}
		})
	}
}

// labels.myKey addresses the key myKey, not my_key, so the label check
// refuses it as it refuses any uppercase label key.
func TestMaskKeyLabelNotSnakeCased(t *testing.T) {
	e := keySecret(t)
	err := e.update(&secretmanagerpb.Secret{Labels: map[string]string{"myKey": "v", "my_key": "v"}}, "labels.myKey")
	wantCode(t, "UpdateSecret", err, codes.InvalidArgument)
	if !strings.Contains(err.Error(), `"myKey"`) {
		t.Fatalf("UpdateSecret = %v, want it to name myKey", err)
	}
	if l := e.stored().GetLabels(); len(l) != 2 {
		t.Fatalf("labels = %v, want env and team only", l)
	}
}

// When a mask names a whole map and one of its keys, the whole map is
// replaced.
func TestMaskKeyWholeFieldWins(t *testing.T) {
	e := keySecret(t)
	if err := e.update(&secretmanagerpb.Secret{Labels: map[string]string{"new": "1"}}, "labels.env", "labels"); err != nil {
		t.Fatal(err)
	}
	if l := e.stored().GetLabels(); len(l) != 1 || l["new"] != "1" {
		t.Fatalf("labels = %v, want only new=1", l)
	}
}

// A key path on a field that is not a map, or one with more segments after
// the key, is INVALID_ARGUMENT and changes nothing.
func TestMaskKeyBadPaths(t *testing.T) {
	for _, path := range []string{"rotation.x", "labels.env.x", "labels.", "labels.`env", "labels.`env`.x", "replication.x"} {
		t.Run(path, func(t *testing.T) {
			e := keySecret(t)
			err := e.update(&secretmanagerpb.Secret{Labels: map[string]string{"env": "prod"}}, "annotations", path)
			wantCode(t, "UpdateSecret", err, codes.InvalidArgument)
			if s := e.stored(); s.GetLabels()["env"] != "dev" || len(s.GetAnnotations()) != 2 {
				t.Fatalf("secret changed: %v", s)
			}
		})
	}
}

// The alias check sees the merged map: an alias key path naming a missing
// version is INVALID_ARGUMENT and the stored aliases remain.
func TestMaskKeyAliasChecked(t *testing.T) {
	e := keySecret(t)
	err := e.update(&secretmanagerpb.Secret{VersionAliases: map[string]int64{"prod": 9}}, "version_aliases.prod")
	wantCode(t, "UpdateSecret", err, codes.InvalidArgument)
	if v := e.stored().GetVersionAliases(); len(v) != 2 || v["prod"] != 1 {
		t.Fatalf("aliases = %v, want prod=1 stage=2", v)
	}
}

// The REST gateway applies a key path the same way as gRPC.
func TestMaskKeyREST(t *testing.T) {
	for _, mask := range []string{"labels.env", url.QueryEscape("labels.`env`")} {
		t.Run(mask, func(t *testing.T) {
			e := keySecret(t)
			var out map[string]any
			if c := e.rest("PATCH", "/secretmanager/v1/"+parent+"/secrets/db?updateMask="+mask, map[string]any{"labels": map[string]string{"env": "prod"}}, &out); c != http.StatusOK {
				t.Fatalf("REST PATCH = %d %v", c, out)
			}
			if l := e.stored().GetLabels(); len(l) != 2 || l["env"] != "prod" || l["team"] != "core" {
				t.Fatalf("labels = %v, want env=prod team=core", l)
			}
		})
	}
}
