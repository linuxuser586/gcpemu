package secrets_test

import (
	"hash/crc32"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

var automatic = &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}

// Secrets and versions over gRPC: create, versions, access by number,
// latest and alias, state changes, etags, list filters and delete.
func TestSecretsAndVersions(t *testing.T) {
	e := start(t, nil)
	_, err := e.sm.CreateSecret(e.ctx, &secretmanagerpb.CreateSecretRequest{Parent: parent, SecretId: "db", Secret: &secretmanagerpb.Secret{}})
	wantCode(t, "create without replication", err, codes.InvalidArgument)
	_, err = e.sm.CreateSecret(e.ctx, &secretmanagerpb.CreateSecretRequest{Parent: parent, SecretId: "bad id", Secret: &secretmanagerpb.Secret{Replication: automatic}})
	wantCode(t, "create bad id", err, codes.InvalidArgument)

	sec := must(e.sm.CreateSecret(e.ctx, &secretmanagerpb.CreateSecretRequest{Parent: parent, SecretId: "db",
		Secret: &secretmanagerpb.Secret{Replication: automatic, Labels: map[string]string{"team": "web"}}}))
	if sec.GetName() != numbered+"/secrets/db" || sec.GetEtag() == "" || sec.GetCreateTime() == nil {
		t.Fatalf("created %v", sec)
	}
	_, err = e.sm.CreateSecret(e.ctx, &secretmanagerpb.CreateSecretRequest{Parent: parent, SecretId: "db", Secret: &secretmanagerpb.Secret{Replication: automatic}})
	wantCode(t, "create again", err, codes.AlreadyExists)

	data := []byte("hunter2")
	crc := int64(crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli)))
	bad := crc + 1
	_, err = e.sm.AddSecretVersion(e.ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: sec.GetName(), Payload: &secretmanagerpb.SecretPayload{Data: data, DataCrc32C: &bad}})
	wantCode(t, "bad checksum", err, codes.InvalidArgument)
	v1 := must(e.sm.AddSecretVersion(e.ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: parent + "/secrets/db", Payload: &secretmanagerpb.SecretPayload{Data: data, DataCrc32C: &crc}}))
	if v1.GetName() != numbered+"/secrets/db/versions/1" || v1.GetState() != secretmanagerpb.SecretVersion_ENABLED || !v1.GetClientSpecifiedPayloadChecksum() || v1.GetReplicationStatus().GetAutomatic() == nil {
		t.Fatalf("v1 %v", v1)
	}
	v2 := must(e.sm.AddSecretVersion(e.ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: sec.GetName(), Payload: &secretmanagerpb.SecretPayload{Data: []byte("v2")}}))

	acc := must(e.sm.AccessSecretVersion(e.ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: parent + "/secrets/db/versions/latest"}))
	if string(acc.GetPayload().GetData()) != "v2" || acc.GetName() != v2.GetName() || acc.GetPayload().GetDataCrc32C() == 0 {
		t.Fatalf("access latest %v", acc)
	}
	acc = must(e.sm.AccessSecretVersion(e.ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: parent + "/secrets/db/versions/1"}))
	if string(acc.GetPayload().GetData()) != "hunter2" || acc.GetPayload().GetDataCrc32C() != crc {
		t.Fatalf("access 1 %v", acc)
	}

	// Aliases must name existing versions and resolve on access.
	_, err = e.sm.UpdateSecret(e.ctx, &secretmanagerpb.UpdateSecretRequest{Secret: &secretmanagerpb.Secret{Name: sec.GetName(), VersionAliases: map[string]int64{"prod": 9}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"version_aliases"}}})
	wantCode(t, "alias to missing version", err, codes.InvalidArgument)
	_, err = e.sm.UpdateSecret(e.ctx, &secretmanagerpb.UpdateSecretRequest{Secret: &secretmanagerpb.Secret{Name: sec.GetName(), Etag: sec.GetEtag(), Labels: map[string]string{"x": "y"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}})
	wantCode(t, "stale etag", err, codes.FailedPrecondition)
	upd := must(e.sm.UpdateSecret(e.ctx, &secretmanagerpb.UpdateSecretRequest{Secret: &secretmanagerpb.Secret{Name: sec.GetName(), VersionAliases: map[string]int64{"prod": 1}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"versionAliases"}}}))
	if upd.GetVersionAliases()["prod"] != 1 || upd.GetLabels()["team"] != "web" || upd.GetEtag() == sec.GetEtag() {
		t.Fatalf("update %v", upd)
	}
	_, err = e.sm.UpdateSecret(e.ctx, &secretmanagerpb.UpdateSecretRequest{Secret: &secretmanagerpb.Secret{Name: sec.GetName()}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"replication"}}})
	wantCode(t, "update immutable", err, codes.InvalidArgument)
	acc = must(e.sm.AccessSecretVersion(e.ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: parent + "/secrets/db/versions/prod"}))
	if string(acc.GetPayload().GetData()) != "hunter2" {
		t.Fatalf("access alias %v", acc)
	}

	// Disable, enable and destroy.
	_, err = e.sm.DisableSecretVersion(e.ctx, &secretmanagerpb.DisableSecretVersionRequest{Name: v2.GetName(), Etag: `"0000000000000"`})
	wantCode(t, "disable stale etag", err, codes.FailedPrecondition)
	dis := must(e.sm.DisableSecretVersion(e.ctx, &secretmanagerpb.DisableSecretVersionRequest{Name: v2.GetName(), Etag: v2.GetEtag()}))
	if dis.GetState() != secretmanagerpb.SecretVersion_DISABLED {
		t.Fatalf("disable %v", dis)
	}
	_, err = e.sm.AccessSecretVersion(e.ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: v2.GetName()})
	wantCode(t, "access disabled", err, codes.FailedPrecondition)
	must(e.sm.EnableSecretVersion(e.ctx, &secretmanagerpb.EnableSecretVersionRequest{Name: v2.GetName()}))
	des := must(e.sm.DestroySecretVersion(e.ctx, &secretmanagerpb.DestroySecretVersionRequest{Name: v1.GetName()}))
	if des.GetState() != secretmanagerpb.SecretVersion_DESTROYED || des.GetDestroyTime() == nil {
		t.Fatalf("destroy %v", des)
	}
	_, err = e.sm.EnableSecretVersion(e.ctx, &secretmanagerpb.EnableSecretVersionRequest{Name: v1.GetName()})
	wantCode(t, "enable destroyed", err, codes.FailedPrecondition)
	got := must(e.sm.GetSecret(e.ctx, &secretmanagerpb.GetSecretRequest{Name: sec.GetName()}))
	if _, ok := got.GetVersionAliases()["prod"]; ok {
		t.Fatalf("alias to a destroyed version kept: %v", got)
	}

	// Lists: versions newest first, filtered by state; secrets by label.
	var states []string
	it := e.sm.ListSecretVersions(e.ctx, &secretmanagerpb.ListSecretVersionsRequest{Parent: sec.GetName()})
	for v, err := it.Next(); err != iterator.Done; v, err = it.Next() {
		if err != nil {
			t.Fatal(err)
		}
		states = append(states, v.GetState().String())
	}
	if strings.Join(states, ",") != "ENABLED,DESTROYED" {
		t.Fatalf("versions %v", states)
	}
	it = e.sm.ListSecretVersions(e.ctx, &secretmanagerpb.ListSecretVersionsRequest{Parent: sec.GetName(), Filter: "state:DESTROYED"})
	if v, err := it.Next(); err != nil || v.GetName() != v1.GetName() {
		t.Fatalf("filtered versions %v %v", v, err)
	}
	must(e.sm.CreateSecret(e.ctx, &secretmanagerpb.CreateSecretRequest{Parent: parent, SecretId: "other", Secret: &secretmanagerpb.Secret{Replication: automatic}}))
	var names []string
	sit := e.sm.ListSecrets(e.ctx, &secretmanagerpb.ListSecretsRequest{Parent: parent, Filter: "labels.team=web"})
	for s, err := sit.Next(); err != iterator.Done; s, err = sit.Next() {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, s.GetName())
	}
	if len(names) != 1 || names[0] != sec.GetName() {
		t.Fatalf("filtered secrets %v", names)
	}
	_, err = e.sm.ListSecrets(e.ctx, &secretmanagerpb.ListSecretsRequest{Parent: parent, Filter: "nope:x"}).Next()
	wantCode(t, "unknown filter field", err, codes.InvalidArgument)

	if err := e.sm.DeleteSecret(e.ctx, &secretmanagerpb.DeleteSecretRequest{Name: sec.GetName()}); err != nil {
		t.Fatal(err)
	}
	_, err = e.sm.AccessSecretVersion(e.ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: v2.GetName()})
	wantCode(t, "access after delete", err, codes.NotFound)
}

// Regional secrets over REST, on the path and on the regional hostname;
// they reject a replication policy.
func TestRegionalREST(t *testing.T) {
	e := start(t, nil)
	base := "/secretmanager/v1/" + parent + "/locations/us-central1/secrets"
	if c := e.rest("POST", base+"?secretId=r", map[string]any{"replication": map[string]any{"automatic": map[string]any{}}}, nil); c != http.StatusBadRequest {
		t.Fatalf("regional with replication: %d", c)
	}
	var sec map[string]any
	if c := e.rest("POST", base+"?secretId=r", map[string]any{"labels": map[string]string{"a": "b"}}, &sec); c != 200 {
		t.Fatalf("create: %d %v", c, sec)
	}
	if sec["name"] != numbered+"/locations/us-central1/secrets/r" {
		t.Fatalf("name %v", sec["name"])
	}
	var v map[string]any
	if c := e.rest("POST", base+"/r:addVersion", map[string]any{"payload": map[string]any{"data": "cmVnaW9uYWw="}}, &v); c != 200 {
		t.Fatalf("addVersion: %d %v", c, v)
	}
	var acc struct {
		Payload struct{ Data string }
	}
	host := "secretmanager.us-central1.rep.googleapis.com"
	if c := e.restHost("GET", host, "/v1/"+parent+"/locations/us-central1/secrets/r/versions/latest:access", nil, &acc); c != 200 || acc.Payload.Data != "cmVnaW9uYWw=" {
		t.Fatalf("access on the regional host: %d %v", c, acc)
	}
	var list struct{ Secrets []map[string]any }
	if c := e.rest("GET", "/secretmanager/v1/"+parent+"/secrets", nil, &list); c != 200 || len(list.Secrets) != 0 {
		t.Fatalf("global list includes regional: %d %v", c, list)
	}
	var locs struct{ Locations []map[string]any }
	if c := e.rest("GET", "/secretmanager/v1/"+parent+"/locations", nil, &locs); c != 200 || len(locs.Locations) == 0 {
		t.Fatalf("locations: %d %v", c, locs)
	}
}
