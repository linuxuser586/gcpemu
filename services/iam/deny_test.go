package iam_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	iamv2 "cloud.google.com/go/iam/apiv2"
	iampb "cloud.google.com/go/iam/apiv2/iampb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/linuxuser586/gcpemu/internal/project"
)

// denyRule is a deny policy rule as REST clients send it.
func denyRule(permission string, principals, exceptions []string) map[string]any {
	return map[string]any{
		"description": "no destroying",
		"denyRule": map[string]any{
			"deniedPrincipals":    principals,
			"exceptionPrincipals": exceptions,
			"deniedPermissions":   []string{permission},
		},
	}
}

// TestDenyPolicyLifecycle drives IAM v2 deny policies over REST the way
// the OpenTofu provider's google_iam_deny_policy does (#184): create with
// a URL-encoded attachment point and poll the operation, read back, list,
// update with and without the current etag, and delete.
func TestDenyPolicyLifecycle(t *testing.T) {
	inst := startIAM(t)
	base := inst.GatewayURL() + "/iam/v2/"
	attach := url.PathEscape("cloudresourcemanager.googleapis.com/projects/" + testProject)
	parent := "policies/" + attach + "/denypolicies"
	// GCP names the attachment point by project number.
	name := "policies/" + url.PathEscape("cloudresourcemanager.googleapis.com/projects/"+project.NumberString(testProject)) + "/denypolicies/no-destroy"

	code, op := call(t, http.MethodPost, base+parent+"?policyId=no-destroy", "", map[string]any{
		"displayName": "No destroy",
		"rules":       []any{denyRule("secretmanager.googleapis.com/versions.destroy", []string{"principalSet://goog/public:all"}, nil)},
	})
	if code != 200 || op["done"] != true || !strings.HasPrefix(op["name"].(string), name+"/operations/") {
		t.Fatalf("create: %d %v", code, op)
	}
	created := op["response"].(map[string]any)
	if created["@type"] != "type.googleapis.com/google.iam.v2.Policy" || created["name"] != name ||
		created["kind"] != "DenyPolicy" || created["uid"] == "" || created["etag"] == "" || created["createTime"] == nil {
		t.Errorf("created policy: %v", created)
	}
	if code, again := call(t, http.MethodGet, base+op["name"].(string), "", nil); code != 200 || again["done"] != true {
		t.Errorf("poll operation: %d %v", code, again)
	}
	if code, out := call(t, http.MethodPost, base+parent+"?policyId=no-destroy", "", map[string]any{
		"rules": []any{denyRule("secretmanager.googleapis.com/versions.destroy", []string{"principalSet://goog/public:all"}, nil)},
	}); code != 409 {
		t.Errorf("create again: %d %v", code, out)
	}

	// Read back by the name the policy reports, and by project ID.
	for _, u := range []string{base + name, base + parent + "/no-destroy"} {
		code, got := call(t, http.MethodGet, u, "", nil)
		if code != 200 || got["displayName"] != "No destroy" || got["etag"] != created["etag"] {
			t.Fatalf("get %s: %d %v", u, code, got)
		}
		rule := got["rules"].([]any)[0].(map[string]any)["denyRule"].(map[string]any)
		if rule["deniedPermissions"].([]any)[0] != "secretmanager.googleapis.com/versions.destroy" {
			t.Errorf("rule read back: %v", rule)
		}
	}
	// An unencoded attachment point names the same policy.
	if code, got := call(t, http.MethodGet, base+"policies/cloudresourcemanager.googleapis.com/projects/"+testProject+"/denypolicies/no-destroy", "", nil); code != 200 || got["name"] != name {
		t.Errorf("get unencoded: %d %v", code, got)
	}

	// List omits rules.
	code, list := call(t, http.MethodGet, base+parent, "", nil)
	pols, _ := list["policies"].([]any)
	if code != 200 || len(pols) != 1 || pols[0].(map[string]any)["rules"] != nil {
		t.Fatalf("list: %d %v", code, list)
	}

	// Update: a stale etag is refused; the current one or none is not.
	upd := map[string]any{
		"displayName": "No destroy, ever",
		"etag":        "c3RhbGU=",
		"rules":       []any{denyRule("secretmanager.googleapis.com/versions.destroy", []string{"principalSet://goog/public:all"}, []string{"principal://goog/subject/breakglass@example.com"})},
	}
	if code, out := call(t, http.MethodPut, base+name, "", upd); code != 409 {
		t.Errorf("update with a stale etag: %d %v", code, out)
	}
	upd["etag"] = created["etag"]
	code, op = call(t, http.MethodPut, base+name, "", upd)
	updated, _ := op["response"].(map[string]any)
	if code != 200 || updated["displayName"] != "No destroy, ever" || updated["etag"] == created["etag"] || updated["createTime"] != created["createTime"] {
		t.Fatalf("update: %d %v", code, op)
	}
	delete(upd, "etag")
	if code, out := call(t, http.MethodPut, base+name, "", upd); code != 200 {
		t.Errorf("update without an etag: %d %v", code, out)
	}

	// Delete answers with the policy as deleted, after which it is gone.
	code, op = call(t, http.MethodDelete, base+name, "", nil)
	if deleted, _ := op["response"].(map[string]any); code != 200 || deleted["name"] != name || deleted["deleteTime"] == nil {
		t.Fatalf("delete: %d %v", code, op)
	}
	if code, out := call(t, http.MethodGet, base+name, "", nil); code != 404 {
		t.Errorf("get after delete: %d %v", code, out)
	}
	if code, out := call(t, http.MethodDelete, base+name, "", nil); code != 404 {
		t.Errorf("delete again: %d %v", code, out)
	}
}

// TestDenyPolicyValidation: malformed policies are refused with
// INVALID_ARGUMENT, including denial conditions on resource tags, which
// the emulator does not model.
func TestDenyPolicyValidation(t *testing.T) {
	inst := startIAM(t)
	parent := inst.GatewayURL() + "/iam/v2/policies/" + url.PathEscape("cloudresourcemanager.googleapis.com/projects/"+testProject) + "/denypolicies"
	public := []string{"principalSet://goog/public:all"}
	withCondition := func(expr string) map[string]any {
		r := denyRule("storage.googleapis.com/buckets.delete", public, nil)
		r["denyRule"].(map[string]any)["denialCondition"] = map[string]any{"expression": expr}
		return r
	}
	for _, tc := range []struct {
		name, id, want string
		rule           map[string]any
	}{
		{"bad id", "Bad_ID", "policy ID", denyRule("storage.googleapis.com/buckets.delete", public, nil)},
		{"v1 permission", "pol-1", "invalid permission", denyRule("storage.buckets.delete", public, nil)},
		{"v1 member", "pol-2", "invalid principal", denyRule("storage.googleapis.com/buckets.delete", []string{"user:a@example.com"}, nil)},
		{"no principals", "pol-3", "denied principal", denyRule("storage.googleapis.com/buckets.delete", nil, nil)},
		{"tag condition", "pol-4", "resource tags", withCondition(`resource.matchTag("123/env", "prod")`)},
		{"bad condition", "pol-5", "invalid", withCondition(`resource.name.`)},
	} {
		code, out := call(t, http.MethodPost, parent+"?policyId="+tc.id, "", map[string]any{"rules": []any{tc.rule}})
		msg, _ := out["error"].(map[string]any)["message"].(string)
		if code != 400 || !strings.Contains(msg, tc.want) {
			t.Errorf("%s: %d %v", tc.name, code, out)
		}
	}
	if code, out := call(t, http.MethodPost, inst.GatewayURL()+"/iam/v2/policies/"+url.PathEscape("storage.googleapis.com/projects/_/buckets/b")+"/denypolicies?policyId=p", "", map[string]any{}); code != 400 {
		t.Errorf("bucket attachment point: %d %v", code, out)
	}
	// A condition without tags is accepted.
	if code, out := call(t, http.MethodPost, parent+"?policyId=cond", "", map[string]any{"rules": []any{withCondition(`resource.name.startsWith("projects/_/buckets/prod-")`)}}); code != 200 {
		t.Errorf("tag-free condition: %d %v", code, out)
	}
}

// TestDenyPolicyGRPC is google.iam.v2.Policies through the Go client.
func TestDenyPolicyGRPC(t *testing.T) {
	inst := startIAM(t)
	ctx := context.Background()
	c, err := iamv2.NewPoliciesClient(ctx,
		option.WithEndpoint(inst.Endpoint("gateway")),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	parent := "policies/" + url.PathEscape("cloudresourcemanager.googleapis.com/projects/"+testProject) + "/denypolicies"
	op, err := c.CreatePolicy(ctx, &iampb.CreatePolicyRequest{Parent: parent, PolicyId: "grpc", Policy: &iampb.Policy{
		Rules: []*iampb.PolicyRule{{Kind: &iampb.PolicyRule_DenyRule{DenyRule: &iampb.DenyRule{
			DeniedPrincipals:  []string{"principalSet://goog/public:all"},
			DeniedPermissions: []string{"iam.googleapis.com/serviceAccounts.delete"},
		}}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	pol, err := op.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.GetPolicy(ctx, &iampb.GetPolicyRequest{Name: pol.Name})
	if err != nil || got.Etag != pol.Etag {
		t.Fatalf("get: %v %v", got, err)
	}
	it := c.ListPolicies(ctx, &iampb.ListPoliciesRequest{Parent: parent})
	n := 0
	for {
		if _, err := it.Next(); err == iterator.Done {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 1 {
		t.Errorf("listed %d policies", n)
	}
	dop, err := c.DeletePolicy(ctx, &iampb.DeletePolicyRequest{Name: pol.Name})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetPolicy(ctx, &iampb.GetPolicyRequest{Name: pol.Name}); status.Code(err) != codes.NotFound {
		t.Errorf("get after delete: %v", err)
	}
}

// TestDenyPolicyEnforced: under enforce, a deny policy on the project
// refuses its permission to the principals it names, the Default
// principal (owner of every project) included, but not to its exceptions
// or for other permissions.
func TestDenyPolicyEnforced(t *testing.T) {
	inst := startIAM(t, enforce)
	base := inst.GatewayURL()
	admin := iamClient(t, inst)
	createSA(t, admin, "keep-me")
	createSA(t, admin, "drop-me")
	parent := base + "/iam/v2/policies/" + url.PathEscape("cloudresourcemanager.googleapis.com/projects/"+testProject) + "/denypolicies"
	if code, out := call(t, http.MethodPost, parent+"?policyId=keep-accounts", "", map[string]any{
		"rules": []any{denyRule("iam.googleapis.com/serviceAccounts.*",
			[]string{"principalSet://goog/public:all"}, []string{"principal://goog/subject/breakglass@example.com"})},
	}); code != 200 {
		t.Fatalf("create: %d %v", code, out)
	}
	// Grant breakglass owner, so only the deny policy stands in its way.
	pol := base + "/cloudresourcemanager/v1/projects/" + testProject
	_, cur := call(t, http.MethodPost, pol+":getIamPolicy", "", map[string]any{})
	bindings, _ := cur["bindings"].([]any)
	bindings = append(bindings, map[string]any{"role": "roles/owner", "members": []string{"user:breakglass@example.com"}})
	if code, out := call(t, http.MethodPost, pol+":setIamPolicy", "", map[string]any{"policy": map[string]any{"bindings": bindings, "etag": cur["etag"]}}); code != 200 {
		t.Fatalf("setIamPolicy: %d %v", code, out)
	}

	sa := base + "/iam/v1/projects/" + testProject + "/serviceAccounts/"
	code, out := call(t, http.MethodDelete, sa+"keep-me@"+testProject+".iam.gserviceaccount.com", "", nil)
	if code != 403 || !strings.Contains(out["error"].(map[string]any)["message"].(string), "keep-accounts") {
		t.Fatalf("Default principal delete: %d %v", code, out)
	}
	// A permission the policy does not name is still allowed.
	if code, out := call(t, http.MethodPost, pol+":getIamPolicy", "", map[string]any{}); code != 200 {
		t.Errorf("getIamPolicy: %d %v", code, out)
	}
	_, tok := refreshToken(t, inst, "user:breakglass@example.com")
	if code, out := call(t, http.MethodDelete, sa+"drop-me@"+testProject+".iam.gserviceaccount.com", tok, nil); code != 200 {
		t.Errorf("exception principal delete: %d %v", code, out)
	}
}
