package pubsub

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
)

func doStatus(t *testing.T, h http.Handler, method, path, body string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec.Code, rec.Body.String()
}

// TestRetentionAndClock covers message retention, snapshot expiry and
// `gcpemu time advance` (FR-PS-008, ClockObserver).
func TestRetentionAndClock(t *testing.T) {
	svc, h, clk, closeFn := openServiceClock(t, filepath.Join(t.TempDir(), "state.db"))
	defer closeFn()
	const v1 = "/v1/projects/ret-proj"
	do(t, h, http.MethodPut, v1+"/topics/ret-topic", map[string]any{"messageRetentionDuration": "600s"})
	do(t, h, http.MethodPut, v1+"/subscriptions/ret-sub", map[string]any{"topic": "projects/ret-proj/topics/ret-topic", "messageRetentionDuration": "600s", "retainAckedMessages": true})
	do(t, h, http.MethodPost, v1+"/topics/ret-topic:publish", map[string]any{"messages": []map[string]any{{"data": base64.StdEncoding.EncodeToString([]byte("old"))}}})
	do(t, h, http.MethodPut, v1+"/snapshots/ret-snap", map[string]any{"subscription": "projects/ret-proj/subscriptions/ret-sub"})
	snaps := do(t, h, http.MethodGet, v1+"/snapshots", nil)
	if len(snaps["snapshots"].([]any)) != 1 {
		t.Fatalf("snapshots = %v", snaps)
	}
	up := do(t, h, http.MethodPatch, v1+"/snapshots/ret-snap", map[string]any{"snapshot": map[string]any{"labels": map[string]string{"k": "v"}}, "updateMask": "labels"})
	if up["labels"].(map[string]any)["k"] != "v" {
		t.Fatalf("patched snapshot = %v", up)
	}
	do(t, h, http.MethodGet, v1+"/snapshots/ret-snap", nil)

	clk.Advance(8 * 24 * time.Hour)
	if err := svc.ClockAdvanced(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d, _ := pulledData(t, do(t, h, http.MethodPost, v1+"/subscriptions/ret-sub:pull", map[string]any{"maxMessages": 10, "returnImmediately": true})); len(d) != 0 {
		t.Fatalf("expired message delivered: %v", d)
	}
	if code, _ := doStatus(t, h, http.MethodGet, v1+"/snapshots/ret-snap", ""); code != http.StatusNotFound {
		t.Fatalf("expired snapshot: %d", code)
	}
	svc.mu.Lock()
	n := len(svc.msgs)
	svc.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d messages still stored", n)
	}

	if err := svc.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if code, _ := doStatus(t, h, http.MethodGet, v1+"/topics/ret-topic", ""); code != http.StatusNotFound {
		t.Fatalf("topic after reset: %d", code)
	}
}

// TestFallbackIAMAndSchemasREST covers the in-package policy store (no IAM
// service running) and the schema REST bindings.
func TestFallbackIAMAndSchemasREST(t *testing.T) {
	_, h, closeFn := openService(t, filepath.Join(t.TempDir(), "state.db"))
	defer closeFn()
	const v1 = "/v1/projects/sch-proj"
	do(t, h, http.MethodPut, v1+"/topics/iam-topic", nil)
	pol := do(t, h, http.MethodGet, v1+"/topics/iam-topic:getIamPolicy", nil)
	if pol["etag"] != "ACAB" {
		t.Fatalf("empty policy = %v", pol)
	}
	set := do(t, h, http.MethodPost, v1+"/topics/iam-topic:setIamPolicy", map[string]any{"policy": map[string]any{
		"etag": "ACAB", "bindings": []map[string]any{{"role": "roles/pubsub.viewer", "members": []string{"user:a@example.com"}}},
	}})
	if code, body := doStatus(t, h, http.MethodPost, v1+"/topics/iam-topic:setIamPolicy", `{"policy":{"etag":"ACAB"}}`); code != http.StatusConflict {
		t.Fatalf("stale etag: %d %s", code, body)
	}
	got := do(t, h, http.MethodPost, v1+"/topics/iam-topic:getIamPolicy", nil)
	if got["etag"] != set["etag"] || len(got["bindings"].([]any)) != 1 {
		t.Fatalf("policy = %v", got)
	}
	if code, _ := doStatus(t, h, http.MethodPost, v1+"/topics/iam-topic:testIamPermissions", `{"permissions":["storage.objects.get"]}`); code != http.StatusBadRequest {
		t.Fatalf("foreign permission: %d", code)
	}

	def := `{"type":"record","name":"R","fields":[{"name":"a","type":"long"}]}`
	created := do(t, h, http.MethodPost, v1+"/schemas?schemaId=rest-schema", map[string]any{"type": "AVRO", "definition": def})
	name := created["name"].(string)
	if name != "projects/sch-proj/schemas/rest-schema" {
		t.Fatalf("created = %v", created)
	}
	list := do(t, h, http.MethodGet, v1+"/schemas", nil)
	if s := list["schemas"].([]any)[0].(map[string]any); s["definition"] != nil {
		t.Errorf("list default view should be BASIC: %v", s)
	}
	do(t, h, http.MethodPost, "/v1/"+name+":commit", map[string]any{"schema": map[string]any{"type": "AVRO", "definition": `{"type":"record","name":"R","fields":[{"name":"a","type":"string"}]}`}})
	revs := do(t, h, http.MethodGet, "/v1/"+name+":listRevisions?pageSize=1", nil)
	if revs["nextPageToken"] == nil || len(revs["schemas"].([]any)) != 1 {
		t.Fatalf("revisions page = %v", revs)
	}
	rb := do(t, h, http.MethodPost, "/v1/"+name+":rollback", map[string]any{"revisionId": created["revisionId"]})
	if rb["definition"] != def {
		t.Fatalf("rollback = %v", rb)
	}
	do(t, h, http.MethodDelete, "/v1/"+name+"@"+created["revisionId"].(string)+":deleteRevision", nil)
	all := do(t, h, http.MethodGet, "/v1/"+name+":listRevisions", nil)
	if len(all["schemas"].([]any)) != 2 {
		t.Fatalf("after delete revision = %v", all)
	}
	do(t, h, http.MethodPost, v1+"/schemas:validate", map[string]any{"schema": map[string]any{"type": "AVRO", "definition": def}})
	if code, _ := doStatus(t, h, http.MethodPost, v1+"/schemas:validateMessage", `{"name":"rest-schema","message":"`+base64.StdEncoding.EncodeToString([]byte(`{"a":"x"}`))+`","encoding":"JSON"}`); code != http.StatusBadRequest {
		t.Fatalf("validateMessage against latest (long) revision: %d", code)
	}
	do(t, h, http.MethodDelete, "/v1/"+name, nil)
	if code, _ := doStatus(t, h, http.MethodGet, "/v1/"+name, ""); code != http.StatusNotFound {
		t.Fatalf("deleted schema: %d", code)
	}
	if code, _ := doStatus(t, h, http.MethodGet, "/v1/projects/sch-proj/unknown", ""); code != http.StatusNotFound {
		t.Fatalf("unknown route: %d", code)
	}
}

func TestAvroJSONValidation(t *testing.T) {
	def := `{"type":"record","name":"Rec","namespace":"ns","fields":[
	  {"name":"i","type":"int"},
	  {"name":"l","type":"long","default":0},
	  {"name":"d","type":"double"},
	  {"name":"b","type":"boolean"},
	  {"name":"s","type":"string"},
	  {"name":"e","type":{"type":"enum","name":"Color","symbols":["RED","GREEN"]}},
	  {"name":"arr","type":{"type":"array","items":"int"}},
	  {"name":"m","type":{"type":"map","values":"string"}},
	  {"name":"f","type":{"type":"fixed","name":"F2","size":2}},
	  {"name":"u","type":["null","string","ns.Color"]},
	  {"name":"opt","type":["null","int"]}
	]}`
	v, err := compileSchema(pubsubpb.Schema_AVRO, def)
	if err != nil {
		t.Fatal(err)
	}
	good := []string{
		`{"i":1,"d":1.5,"b":true,"s":"x","e":"RED","arr":[1,2],"m":{"k":"v"},"f":"ab","u":null}`,
		`{"i":1,"d":"NaN","b":false,"s":"","e":"GREEN","arr":[],"m":{},"f":"ab","u":{"string":"x"},"opt":{"int":3}}`,
		`{"i":1,"d":2,"b":false,"s":"","e":"GREEN","arr":[],"m":{},"f":"ab","u":{"ns.Color":"RED"}}`,
		`{"i":1,"d":2,"b":false,"s":"","e":"GREEN","arr":[],"m":{},"f":"ab","u":"bare"}`,
	}
	for _, g := range good {
		if err := v.validate([]byte(g), pubsubpb.Encoding_JSON); err != nil {
			t.Errorf("%s: %v", g, err)
		}
	}
	base := `"d":1,"b":true,"s":"x","e":"RED","arr":[1],"m":{},"f":"ab","u":null`
	bad := []string{
		`{"i":3000000000,` + base + `}`,
		`{"i":1.5,` + base + `}`,
		`{` + base + `}`,
		`{"i":1,` + base + `,"extra":1}`,
		`{"i":1,"d":1,"b":"yes","s":"x","e":"RED","arr":[1],"m":{},"f":"ab","u":null}`,
		`{"i":1,"d":1,"b":true,"s":"x","e":"BLUE","arr":[1],"m":{},"f":"ab","u":null}`,
		`{"i":1,"d":1,"b":true,"s":"x","e":"RED","arr":["x"],"m":{},"f":"ab","u":null}`,
		`{"i":1,"d":1,"b":true,"s":"x","e":"RED","arr":[1],"m":{"k":1},"f":"ab","u":null}`,
		`{"i":1,"d":1,"b":true,"s":"x","e":"RED","arr":[1],"m":{},"f":"abc","u":null}`,
		`{"i":1,"d":1,"b":true,"s":"x","e":"RED","arr":[1],"m":{},"f":"ab","u":5}`,
		`not json`,
	}
	for _, b := range bad {
		if err := v.validate([]byte(b), pubsubpb.Encoding_JSON); err == nil {
			t.Errorf("%s: expected error", b)
		}
	}
	// Binary encoding.
	if err := v.validate([]byte{0x01}, pubsubpb.Encoding_BINARY); err == nil {
		t.Error("truncated binary accepted")
	}
	pv, err := compileSchema(pubsubpb.Schema_PROTOCOL_BUFFER, "syntax = \"proto3\";\nmessage M { string a = 1; }")
	if err != nil {
		t.Fatal(err)
	}
	if err := pv.validate([]byte{0x0a, 0x01, 'x'}, pubsubpb.Encoding_BINARY); err != nil {
		t.Errorf("binary proto: %v", err)
	}
	if err := pv.validate([]byte{0x0a, 0x05}, pubsubpb.Encoding_BINARY); err == nil {
		t.Error("truncated proto accepted")
	}
	if _, err := compileSchema(pubsubpb.Schema_TYPE_UNSPECIFIED, "x"); err == nil {
		t.Error("unspecified type accepted")
	}
	if _, err := compileSchema(pubsubpb.Schema_PROTOCOL_BUFFER, `syntax = "proto3"; enum E { A = 0; }`); err == nil {
		t.Error("proto without message accepted")
	}
}
