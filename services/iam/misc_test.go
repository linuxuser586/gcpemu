package iam_test

import (
	"context"
	"testing"

	crmv1 "google.golang.org/api/cloudresourcemanager/v1"
	iamv1 "google.golang.org/api/iam/v1"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/project"
)

func TestPredefinedRolesAndProjects(t *testing.T) {
	inst := startIAM(t)
	c := iamClient(t, inst)
	r, err := c.Roles.Get("roles/storage.objectViewer").Do()
	if err != nil || len(r.IncludedPermissions) == 0 || r.Stage != "GA" {
		t.Fatalf("get role: %v %+v", err, r)
	}
	if _, err := c.Roles.Get("roles/nope.nope").Do(); httpCode(err) != 404 {
		t.Fatalf("unknown role: want 404, got %v", err)
	}
	all := 0
	err = c.Roles.List().View("FULL").Pages(context.Background(), func(p *iamv1.ListRolesResponse) error {
		all += len(p.Roles)
		return nil
	})
	if err != nil || all < 50 {
		t.Fatalf("list roles: %v %d", err, all)
	}
	gr, err := c.Roles.QueryGrantableRoles(&iamv1.QueryGrantableRolesRequest{FullResourceName: "//cloudresourcemanager.googleapis.com/projects/" + testProject}).Do()
	if err != nil || len(gr.Roles) != all {
		t.Fatalf("grantable: %v", err)
	}
	crm := crmClient(t, inst)
	if _, err := crm.Projects.Get("listed-project").Do(); err != nil {
		t.Fatal(err)
	}
	pl, err := crm.Projects.List().Do()
	if err != nil || len(pl.Projects) == 0 {
		t.Fatalf("list projects: %v", err)
	}
	disc := fetchJSON[map[string]any](t, inst.GatewayURL()+"/.well-known/openid-configuration")
	if disc["jwks_uri"] != inst.GatewayURL()+"/oauth2/v3/certs" {
		t.Fatalf("discovery: %v", disc)
	}
}

func TestMembersAndAccountVerbs(t *testing.T) {
	inst := startIAM(t, enforce)
	ctx := context.Background()
	c := iamClient(t, inst)
	sa := createSA(t, c, "verbs-sa")
	name := saName(sa)

	// Deprecated iam v1 signBlob/signJwt.
	if _, err := c.Projects.ServiceAccounts.SignBlob(name, &iamv1.SignBlobRequest{BytesToSign: "aGk="}).Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Projects.ServiceAccounts.SignJwt(name, &iamv1.SignJwtRequest{Payload: `{"a":1}`}).Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Projects.ServiceAccounts.Update(name, &iamv1.ServiceAccount{DisplayName: "PUT"}).Do(); err != nil {
		t.Fatal(err)
	}
	key, _ := createKeyFile(t, c, sa)
	if _, err := c.Projects.ServiceAccounts.Keys.Disable(key.Name, &iamv1.DisableServiceAccountKeyRequest{}).Do(); err != nil {
		t.Fatal(err)
	}
	if k, _ := c.Projects.ServiceAccounts.Keys.Get(key.Name).Do(); !k.Disabled {
		t.Fatal("key not disabled")
	}

	tp, err := c.Projects.ServiceAccounts.TestIamPermissions(name, &iamv1.TestIamPermissionsRequest{Permissions: []string{"iam.serviceAccounts.get"}}).Do()
	if err != nil || len(tp.Permissions) != 1 {
		t.Fatalf("testIamPermissions: %v %+v", err, tp)
	}

	svc, _ := inst.Env.Lookup("iam")
	ev := svc.(emu.PolicyEvaluator)
	res := "//pubsub.googleapis.com/projects/" + testProject + "/topics/t"
	crm := crmClient(t, inst)
	pol, _ := crm.Projects.GetIamPolicy(testProject, &crmv1.GetIamPolicyRequest{}).Do()
	pol.Bindings = []*crmv1.Binding{
		{Role: "roles/pubsub.publisher", Members: []string{"domain:corp.example"}},
		{Role: "roles/owner", Members: []string{"user:boss@example.com"}},
		{Role: "roles/pubsub.viewer", Members: []string{"allAuthenticatedUsers"}},
	}
	if _, err := crm.Projects.SetIamPolicy(testProject, &crmv1.SetIamPolicyRequest{Policy: pol}).Do(); err != nil {
		t.Fatal(err)
	}
	check := func(p, perm, res string, want bool) {
		t.Helper()
		if got := ev.Allowed(ctx, emu.Principal(p), perm, res); got != want {
			t.Errorf("%s %s on %s = %v, want %v", p, perm, res, got, want)
		}
	}
	check("user:alice@corp.example", "pubsub.topics.publish", res, true)
	check("user:alice@other.example", "pubsub.topics.publish", res, false)
	check("user:anyone@x.example", "pubsub.topics.get", res, true)
	check("user:anyone@x.example", "pubsub.topics.delete", res, false)
	// projectOwner: convenience members on a resource policy.
	topicPolicy := `{"bindings":[{"role":"roles/pubsub.admin","members":["projectOwner:` + testProject + `"]}]}`
	if _, err := svc.(emu.IAMPolicyStore).SetPolicyJSON(ctx, res, []byte(topicPolicy)); err != nil {
		t.Fatal(err)
	}
	check("user:boss@example.com", "pubsub.topics.delete", res, true)
	// Default compute SA is editor on its own project only.
	compute := "serviceAccount:" + project.NumberString(testProject) + "-compute@developer.gserviceaccount.com"
	check(compute, "pubsub.topics.create", "//cloudresourcemanager.googleapis.com/projects/"+testProject, true)
	check(compute, "pubsub.topics.create", "//cloudresourcemanager.googleapis.com/projects/other-project", false)
}

func TestWorkloadIdentityPoolCRUD(t *testing.T) {
	inst := startIAM(t)
	c := iamClient(t, inst)
	pools := c.Projects.Locations.WorkloadIdentityPools
	parent := "projects/" + testProject + "/locations/global"
	if _, err := pools.Create(parent, &iamv1.WorkloadIdentityPool{DisplayName: "a"}).WorkloadIdentityPoolId("pool-one").Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := pools.Create(parent, &iamv1.WorkloadIdentityPool{}).WorkloadIdentityPoolId("pool-one").Do(); httpCode(err) != 409 {
		t.Fatalf("duplicate pool: want 409, got %v", err)
	}
	name := "projects/" + project.NumberString(testProject) + "/locations/global/workloadIdentityPools/pool-one"
	if _, err := pools.Patch(name, &iamv1.WorkloadIdentityPool{DisplayName: "b"}).UpdateMask("displayName").Do(); err != nil {
		t.Fatal(err)
	}
	if p, _ := pools.Get(name).Do(); p.DisplayName != "b" {
		t.Fatalf("patch: %+v", p)
	}
	if _, err := pools.Delete(name).Do(); err != nil {
		t.Fatal(err)
	}
	l, err := pools.List(parent).Do()
	if err != nil || len(l.WorkloadIdentityPools) != 0 {
		t.Fatalf("list after delete: %v %+v", err, l)
	}
	if _, err := pools.Undelete(name, &iamv1.UndeleteWorkloadIdentityPoolRequest{}).Do(); err != nil {
		t.Fatal(err)
	}
	if l, _ := pools.List(parent).Do(); len(l.WorkloadIdentityPools) != 1 {
		t.Fatalf("list after undelete: %+v", l)
	}
}
