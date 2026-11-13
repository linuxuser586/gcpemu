package certs_test

import (
	"net/http"
	"testing"

	"cloud.google.com/go/certificatemanager/apiv1/certificatemanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// #1087: an update_mask path naming one map key (AIP-161, ADR 0075), such
// as labels.my_key, changes that key only and uses the key as written.

const keyCertMapName = globalLoc + "/certificateMaps/km"

// keyCertMap creates certificate map "km" with the given labels.
func keyCertMap(t *testing.T, labels map[string]string) *env {
	t.Helper()
	e := start(t)
	op, err := e.cm.CreateCertificateMap(e.ctx, &certificatemanagerpb.CreateCertificateMapRequest{
		Parent: globalLoc, CertificateMapId: "km", CertificateMap: &certificatemanagerpb.CertificateMap{Labels: labels},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Wait(e.ctx); err != nil {
		t.Fatal(err)
	}
	return e
}

// updateCertMap sends UpdateCertificateMap for "km" with labels and mask paths.
func (e *env) updateCertMap(labels map[string]string, paths ...string) error {
	e.t.Helper()
	op, err := e.cm.UpdateCertificateMap(e.ctx, &certificatemanagerpb.UpdateCertificateMapRequest{
		CertificateMap: &certificatemanagerpb.CertificateMap{Name: keyCertMapName, Labels: labels},
		UpdateMask:     &fieldmaskpb.FieldMask{Paths: paths},
	})
	if err != nil {
		return err
	}
	_, err = op.Wait(e.ctx)
	return err
}

// certMapLabels returns the stored labels of certificate map "km".
func (e *env) certMapLabels() map[string]string {
	e.t.Helper()
	m, err := e.cm.GetCertificateMap(e.ctx, &certificatemanagerpb.GetCertificateMapRequest{Name: keyCertMapName})
	if err != nil {
		e.t.Fatal(err)
	}
	return m.GetLabels()
}

// A key with an underscore is used as written: labels.my_key changes
// my_key, keeps team, and leaves a myKey label alone.
func TestMaskKeySnakeCaseKey(t *testing.T) {
	e := keyCertMap(t, map[string]string{"my_key": "old", "myKey": "camel", "team": "core"})
	if err := e.updateCertMap(map[string]string{"my_key": "new"}, "labels.my_key"); err != nil {
		t.Fatal(err)
	}
	if l := e.certMapLabels(); len(l) != 3 || l["my_key"] != "new" || l["myKey"] != "camel" || l["team"] != "core" {
		t.Fatalf("labels = %v, want my_key=new myKey=camel team=core", l)
	}
}

// A backtick-quoted key is used as written and may hold dots.
func TestMaskKeyQuoted(t *testing.T) {
	e := keyCertMap(t, map[string]string{"a-b": "old", "team": "core"})
	if err := e.updateCertMap(map[string]string{"a-b": "new", "a.b": "dot"}, "labels.`a-b`", "labels.`a.b`"); err != nil {
		t.Fatal(err)
	}
	if l := e.certMapLabels(); len(l) != 3 || l["a-b"] != "new" || l["a.b"] != "dot" || l["team"] != "core" {
		t.Fatalf("labels = %v, want a-b=new a.b=dot team=core", l)
	}
}

// A key the request lacks is removed; a key neither side has is a no-op.
func TestMaskKeyRemove(t *testing.T) {
	e := keyCertMap(t, map[string]string{"env": "dev", "team": "core"})
	if err := e.updateCertMap(map[string]string{"other": "x"}, "labels.env", "labels.gone"); err != nil {
		t.Fatal(err)
	}
	if l := e.certMapLabels(); len(l) != 1 || l["team"] != "core" {
		t.Fatalf("labels = %v, want only team=core", l)
	}
	if err := e.updateCertMap(nil, "labels.team"); err != nil {
		t.Fatal(err)
	}
	if l := e.certMapLabels(); len(l) != 0 {
		t.Fatalf("labels = %v, want none", l)
	}
}

// A whole-field path replaces the map and wins over key paths of it.
func TestMaskKeyWholeFieldWins(t *testing.T) {
	e := keyCertMap(t, map[string]string{"env": "dev", "team": "core"})
	if err := e.updateCertMap(map[string]string{"x": "1"}, "labels.env", "labels"); err != nil {
		t.Fatal(err)
	}
	if l := e.certMapLabels(); len(l) != 1 || l["x"] != "1" {
		t.Fatalf("labels = %v, want only x=1", l)
	}
}

// A key path with extra segments, or on a field that is not a map (an
// output-only one included), is
// INVALID_ARGUMENT and changes nothing.
func TestMaskKeyInvalidPath(t *testing.T) {
	e := keyCertMap(t, map[string]string{"env": "dev"})
	for _, p := range []string{"labels.env.x", "description.x", "labels.`a", "labels.", "name.x", "createTime.x", "gclbTargets.x"} {
		err := e.updateCertMap(map[string]string{"env": "prod"}, p, "labels.env")
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("path %q: %v, want INVALID_ARGUMENT", p, err)
		}
	}
	if l := e.certMapLabels(); len(l) != 1 || l["env"] != "dev" {
		t.Fatalf("labels = %v, want env=dev unchanged", l)
	}
}

// REST PATCH on a Network Security resource follows the same rule, and a
// path into a nested message still works next to a key path.
func TestMaskKeyREST(t *testing.T) {
	e := start(t)
	base := nsBase + "/serverTlsPolicies"
	e.restOp("POST", base+"?serverTlsPolicyId=p", map[string]any{
		"labels": map[string]any{"my_key": "old", "myKey": "camel", "a.b": "dot"},
	})
	e.restOp("PATCH", base+"/p?updateMask=labels.my_key,labels.`a.b`,mtls_policy.client_validation_mode", map[string]any{
		"labels":     map[string]any{"my_key": "new"},
		"mtlsPolicy": map[string]any{"clientValidationMode": "ALLOW_INVALID_OR_MISSING_CLIENT_CERT"},
	})
	var got map[string]any
	e.rest("GET", base+"/p", nil, &got)
	l, _ := got["labels"].(map[string]any)
	if len(l) != 2 || l["my_key"] != "new" || l["myKey"] != "camel" {
		t.Fatalf("labels = %v, want my_key=new myKey=camel", l)
	}
	if mtls, _ := got["mtlsPolicy"].(map[string]any); mtls["clientValidationMode"] != "ALLOW_INVALID_OR_MISSING_CLIENT_CERT" {
		t.Fatalf("mtlsPolicy = %v, want clientValidationMode ALLOW_INVALID_OR_MISSING_CLIENT_CERT", mtls)
	}

	var errResp map[string]any
	if code := e.rest("PATCH", base+"/p?updateMask=labels.my_key.x", map[string]any{}, &errResp); code != http.StatusBadRequest {
		t.Fatalf("labels.my_key.x: %d %v", code, errResp)
	}
}
