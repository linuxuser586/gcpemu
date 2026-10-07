package pubsub_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/emutest"
)

// restCall issues a JSON request and decodes the JSON response.
func restCall(t *testing.T, method, url string, body any, wantStatus int) map[string]any {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, rd)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s %s: status %d, want %d: %s", method, url, resp.StatusCode, wantStatus, b)
	}
	out := map[string]any{}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, url, err, b)
		}
	}
	return out
}

// TestREST drives the REST v1 surface through the gateway the way gcloud
// and OpenTofu do (CLOUDSDK_API_ENDPOINT_OVERRIDES_PUBSUB).
func TestREST(t *testing.T) {
	inst := emutest.Start(t, []string{"pubsub"})
	env := inst.EnvVars()
	base := env["CLOUDSDK_API_ENDPOINT_OVERRIDES_PUBSUB"]
	if base != inst.GatewayURL()+"/pubsub/" || env["PUBSUB_EMULATOR_HOST"] != inst.Endpoint("pubsub") {
		t.Fatalf("env = %v", env)
	}
	v1 := base + "v1/projects/" + project

	tp := restCall(t, http.MethodPut, v1+"/topics/rest-topic", map[string]any{"labels": map[string]string{"a": "b"}}, 200)
	if tp["name"] != topicName("rest-topic") {
		t.Fatalf("topic = %v", tp)
	}
	restCall(t, http.MethodPut, v1+"/topics/rest-topic", map[string]any{}, 409)
	sub := restCall(t, http.MethodPut, v1+"/subscriptions/rest-sub", map[string]any{"topic": topicName("rest-topic"), "ackDeadlineSeconds": 20}, 200)
	if sub["ackDeadlineSeconds"] != float64(20) || sub["state"] != "ACTIVE" {
		t.Fatalf("sub = %v", sub)
	}
	up := restCall(t, http.MethodPatch, v1+"/topics/rest-topic", map[string]any{
		"topic": map[string]any{"labels": map[string]string{"a": "c"}, "messageRetentionDuration": "3600s"}, "updateMask": "labels,messageRetentionDuration",
	}, 200)
	if up["messageRetentionDuration"] != "3600s" || up["labels"].(map[string]any)["a"] != "c" {
		t.Fatalf("patched = %v", up)
	}

	pub := restCall(t, http.MethodPost, v1+"/topics/rest-topic:publish", map[string]any{"messages": []map[string]any{
		{"data": base64.StdEncoding.EncodeToString([]byte("one")), "attributes": map[string]string{"k": "v"}},
		{"data": base64.StdEncoding.EncodeToString([]byte("two"))},
	}}, 200)
	if ids := pub["messageIds"].([]any); len(ids) != 2 {
		t.Fatalf("publish = %v", pub)
	}
	pulled := restCall(t, http.MethodPost, v1+"/subscriptions/rest-sub:pull", map[string]any{"maxMessages": 10, "returnImmediately": true}, 200)
	msgs := pulled["receivedMessages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("pulled %v", pulled)
	}
	var ackIDs []string
	for _, m := range msgs {
		rm := m.(map[string]any)
		ackIDs = append(ackIDs, rm["ackId"].(string))
		msg := rm["message"].(map[string]any)
		if msg["messageId"] == "" || msg["publishTime"] == nil {
			t.Errorf("message = %v", msg)
		}
	}
	restCall(t, http.MethodPost, v1+"/subscriptions/rest-sub:modifyAckDeadline", map[string]any{"ackIds": ackIDs, "ackDeadlineSeconds": 30}, 200)
	restCall(t, http.MethodPost, v1+"/subscriptions/rest-sub:acknowledge", map[string]any{"ackIds": ackIDs}, 200)

	list := restCall(t, http.MethodGet, v1+"/topics?pageSize=10", nil, 200)
	if len(list["topics"].([]any)) != 1 {
		t.Errorf("list = %v", list)
	}
	ts := restCall(t, http.MethodGet, v1+"/topics/rest-topic/subscriptions", nil, 200)
	if ts["subscriptions"].([]any)[0] != subName("rest-sub") {
		t.Errorf("topic subscriptions = %v", ts)
	}
	subs := restCall(t, http.MethodGet, v1+"/subscriptions", nil, 200)
	if len(subs["subscriptions"].([]any)) != 1 {
		t.Errorf("subscriptions = %v", subs)
	}

	// Snapshots and seek.
	restCall(t, http.MethodPut, v1+"/snapshots/rest-snap", map[string]any{"subscription": subName("rest-sub")}, 200)
	restCall(t, http.MethodPost, v1+"/subscriptions/rest-sub:seek", map[string]any{"snapshot": "projects/" + project + "/snapshots/rest-snap"}, 200)
	restCall(t, http.MethodPost, v1+"/subscriptions/rest-sub:seek", map[string]any{"time": "2020-01-01T00:00:00Z"}, 200)

	// IAM.
	pol := restCall(t, http.MethodPost, v1+"/topics/rest-topic:setIamPolicy", map[string]any{"policy": map[string]any{
		"bindings": []map[string]any{{"role": "roles/pubsub.publisher", "members": []string{"user:a@example.com"}}},
	}}, 200)
	if pol["etag"] == nil {
		t.Errorf("policy = %v", pol)
	}
	got := restCall(t, http.MethodGet, v1+"/topics/rest-topic:getIamPolicy", nil, 200)
	if !strings.Contains(mustJSON(got), "roles/pubsub.publisher") {
		t.Errorf("getIamPolicy = %v", got)
	}
	perms := restCall(t, http.MethodPost, v1+"/subscriptions/rest-sub:testIamPermissions", map[string]any{"permissions": []string{"pubsub.subscriptions.consume"}}, 200)
	if len(perms["permissions"].([]any)) != 1 {
		t.Errorf("testIamPermissions = %v", perms)
	}

	// Errors use the GCP JSON envelope.
	e := restCall(t, http.MethodGet, v1+"/topics/missing-topic", nil, 404)
	if errObj := e["error"].(map[string]any); errObj["status"] != "NOT_FOUND" || errObj["code"] != float64(404) {
		t.Errorf("error = %v", e)
	}
	restCall(t, http.MethodPost, v1+"/topics/rest-topic:publish", map[string]any{"messages": []map[string]any{{}}}, 400)

	restCall(t, http.MethodPost, v1+"/subscriptions/rest-sub:modifyPushConfig", map[string]any{"pushConfig": map[string]any{}}, 200)
	restCall(t, http.MethodDelete, v1+"/snapshots/rest-snap", nil, 200)
	restCall(t, http.MethodDelete, v1+"/subscriptions/rest-sub", nil, 200)
	restCall(t, http.MethodDelete, v1+"/topics/rest-topic", nil, 200)
	restCall(t, http.MethodGet, v1+"/topics/rest-topic", nil, 404)

	// The dedicated port serves REST as well.
	restCall(t, http.MethodPut, "http://"+inst.Endpoint("pubsub")+"/v1/projects/"+project+"/topics/direct-topic", nil, 200)
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
