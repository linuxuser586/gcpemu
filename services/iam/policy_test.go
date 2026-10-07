package iam_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	crmv1 "google.golang.org/api/cloudresourcemanager/v1"
	crmv3 "google.golang.org/api/cloudresourcemanager/v3"
	iamv1 "google.golang.org/api/iam/v1"
	"google.golang.org/api/option"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/project"
)

func TestProjectPolicy(t *testing.T) {
	inst := startIAM(t)
	c := crmClient(t, inst)
	sa := createSA(t, iamClient(t, inst), "binding-sa")

	p, err := c.Projects.GetIamPolicy(testProject, &crmv1.GetIamPolicyRequest{Options: &crmv1.GetPolicyOptions{RequestedPolicyVersion: 3}}).Do()
	if err != nil || p.Etag != "ACAB" {
		t.Fatalf("initial policy: %v %+v", err, p)
	}
	p.Bindings = []*crmv1.Binding{{Role: "roles/storage.admin", Members: []string{"serviceAccount:" + sa.Email, "user:b@example.com", "user:a@example.com", "user:a@example.com"}}}
	set, err := c.Projects.SetIamPolicy(testProject, &crmv1.SetIamPolicyRequest{Policy: p}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if set.Etag == "ACAB" || len(set.Bindings[0].Members) != 3 || set.Bindings[0].Members[0] != "serviceAccount:"+sa.Email {
		t.Fatalf("normalised policy: %+v", set.Bindings[0])
	}
	// Stale etag → 409 ABORTED (FR-IAM-003).
	if _, err := c.Projects.SetIamPolicy(testProject, &crmv1.SetIamPolicyRequest{Policy: p}).Do(); httpCode(err) != 409 {
		t.Fatalf("stale etag: want 409, got %v", err)
	}
	bad := &crmv1.Policy{Bindings: []*crmv1.Binding{{Role: "roles/storage.objectAdmn", Members: []string{"user:a@example.com"}}}}
	if _, err := c.Projects.SetIamPolicy(testProject, &crmv1.SetIamPolicyRequest{Policy: bad}).Do(); httpCode(err) != 400 {
		t.Fatalf("unknown role: want 400, got %v", err)
	}
	// Roles of services outside the emulator are accepted.
	cur, _ := c.Projects.GetIamPolicy(testProject, &crmv1.GetIamPolicyRequest{}).Do()
	cur.Bindings = append(cur.Bindings, &crmv1.Binding{Role: "roles/logging.logWriter", Members: []string{"user:a@example.com"}})
	if _, err := c.Projects.SetIamPolicy(testProject, &crmv1.SetIamPolicyRequest{Policy: cur}).Do(); err != nil {
		t.Fatalf("foreign role: %v", err)
	}
	bad.Bindings[0] = &crmv1.Binding{Role: "roles/viewer", Members: []string{"serviceAccount:ghost@test-project.iam.gserviceaccount.com"}}
	if _, err := c.Projects.SetIamPolicy(testProject, &crmv1.SetIamPolicyRequest{Policy: bad}).Do(); httpCode(err) != 400 {
		t.Fatalf("unknown SA member: want 400, got %v", err)
	}
	bad.Bindings[0] = &crmv1.Binding{Role: "roles/viewer", Members: []string{"bob"}}
	if _, err := c.Projects.SetIamPolicy(testProject, &crmv1.SetIamPolicyRequest{Policy: bad}).Do(); httpCode(err) != 400 {
		t.Fatalf("bad member: want 400, got %v", err)
	}

	tp, err := c.Projects.TestIamPermissions(testProject, &crmv1.TestIamPermissionsRequest{Permissions: []string{"storage.buckets.create"}}).Do()
	if err != nil || len(tp.Permissions) != 1 {
		t.Fatalf("testIamPermissions (default principal): %v %+v", err, tp)
	}

	proj, err := c.Projects.Get(testProject).Do()
	if err != nil || proj.ProjectNumber != project.Number(testProject) || proj.LifecycleState != "ACTIVE" {
		t.Fatalf("v1 get: %v %+v", err, proj)
	}
	v3, err := crmv3.NewService(context.Background(), option.WithEndpoint(inst.GatewayURL()+"/cloudresourcemanager/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	p3, err := v3.Projects.Get("projects/" + project.NumberString(testProject)).Do()
	if err != nil || p3.ProjectId != testProject || p3.Name != "projects/"+project.NumberString(testProject) {
		t.Fatalf("v3 get by number: %v %+v", err, p3)
	}
}

// TestCustomRolesAndConditions covers FR-IAM-002 custom roles and
// FR-IAM-005 conditions, evaluated in enforce mode.
func TestCustomRolesAndConditions(t *testing.T) {
	inst := startIAM(t, enforce)
	ic := iamClient(t, inst)
	role, err := ic.Projects.Roles.Create("projects/"+testProject, &iamv1.CreateRoleRequest{
		RoleId: "keyLister",
		Role:   &iamv1.Role{Title: "Key lister", IncludedPermissions: []string{"iam.serviceAccountKeys.list"}},
	}).Do()
	if err != nil || role.Name != "projects/test-project/roles/keyLister" || role.Stage != "ALPHA" {
		t.Fatalf("create role: %v %+v", err, role)
	}
	if _, err := ic.Projects.Roles.Create("projects/"+testProject, &iamv1.CreateRoleRequest{RoleId: "keyLister", Role: &iamv1.Role{}}).Do(); httpCode(err) != 409 {
		t.Fatalf("duplicate role: want 409, got %v", err)
	}
	patched, err := ic.Projects.Roles.Patch(role.Name, &iamv1.Role{Title: "Lister", Etag: role.Etag}).UpdateMask("title").Do()
	if err != nil || patched.Title != "Lister" || len(patched.IncludedPermissions) != 1 {
		t.Fatalf("patch role: %v %+v", err, patched)
	}
	if _, err := ic.Projects.Roles.Patch(role.Name, &iamv1.Role{Title: "x", Etag: role.Etag}).UpdateMask("title").Do(); httpCode(err) != 409 {
		t.Fatalf("stale role etag: want 409, got %v", err)
	}
	list, err := ic.Projects.Roles.List("projects/" + testProject).Do()
	if err != nil || len(list.Roles) != 1 {
		t.Fatalf("list roles: %v %+v", err, list)
	}

	sa := createSA(t, ic, "role-user")
	_, kf := createKeyFile(t, ic, sa)
	asSA := iamClient(t, inst, option.WithCredentialsJSON(kf))

	keysOf := func() error {
		_, err := asSA.Projects.ServiceAccounts.Keys.List(saName(sa)).Do()
		return err
	}
	if err := keysOf(); httpCode(err) != 403 {
		t.Fatalf("before binding: want 403, got %v", err)
	}
	crm := crmClient(t, inst)
	pol, _ := crm.Projects.GetIamPolicy(testProject, &crmv1.GetIamPolicyRequest{}).Do()
	pol.Bindings = append(pol.Bindings, &crmv1.Binding{Role: role.Name, Members: []string{"serviceAccount:" + sa.Email}})
	if _, err := crm.Projects.SetIamPolicy(testProject, &crmv1.SetIamPolicyRequest{Policy: pol}).Do(); err != nil {
		t.Fatal(err)
	}
	if err := keysOf(); err != nil {
		t.Fatalf("custom role binding: %v", err)
	}
	// Deleting the role revokes it.
	if _, err := ic.Projects.Roles.Delete(role.Name).Do(); err != nil {
		t.Fatal(err)
	}
	if err := keysOf(); httpCode(err) != 403 {
		t.Fatalf("deleted role: want 403, got %v", err)
	}
	if _, err := ic.Projects.Roles.Undelete(role.Name, &iamv1.UndeleteRoleRequest{}).Do(); err != nil {
		t.Fatal(err)
	}
	if err := keysOf(); err != nil {
		t.Fatalf("undeleted role: %v", err)
	}

	// Conditional binding on the service account itself.
	other := createSA(t, ic, "cond-target")
	setSAPolicy := func(expr string) {
		pol, _ := ic.Projects.ServiceAccounts.GetIamPolicy(saName(other)).Do()
		pol.Bindings = []*iamv1.Binding{{Role: "roles/iam.serviceAccountViewer", Members: []string{"serviceAccount:" + sa.Email},
			Condition: &iamv1.Expr{Title: "t", Expression: expr}}}
		got, err := ic.Projects.ServiceAccounts.SetIamPolicy(saName(other), &iamv1.SetIamPolicyRequest{Policy: pol}).Do()
		if err != nil || got.Version != 3 {
			t.Fatalf("set conditional policy %q: %v", expr, err)
		}
	}
	getOther := func() error { _, err := asSA.Projects.ServiceAccounts.Get(saName(other)).Do(); return err }
	setSAPolicy(`request.time < timestamp("2000-01-01T00:00:00Z")`)
	if err := getOther(); httpCode(err) != 403 {
		t.Fatalf("expired condition: want 403, got %v", err)
	}
	setSAPolicy(`request.time > timestamp("2000-01-01T00:00:00Z") && resource.name.endsWith("cond-target@test-project.iam.gserviceaccount.com")`)
	if err := getOther(); err != nil {
		t.Fatalf("matching condition: %v", err)
	}
	pol2, _ := ic.Projects.ServiceAccounts.GetIamPolicy(saName(other)).Do()
	pol2.Bindings[0].Condition.Expression = "request.foo == 1"
	if _, err := ic.Projects.ServiceAccounts.SetIamPolicy(saName(other), &iamv1.SetIamPolicyRequest{Policy: pol2}).Do(); httpCode(err) != 400 {
		t.Fatalf("invalid condition: want 400, got %v", err)
	}
}

// TestPolicyStoreContract exercises the cross-service APIs other services
// use: IAMPolicyStore, IAMResourceParents, IAMPermissionTester and
// ServiceAccountKeys.
func TestPolicyStoreContract(t *testing.T) {
	inst := startIAM(t, enforce)
	svc, _ := inst.Env.Lookup("iam")
	store := svc.(emu.IAMPolicyStore)
	parents := svc.(emu.IAMResourceParents)
	tester := svc.(emu.IAMPermissionTester)
	keys := svc.(emu.ServiceAccountKeys)
	ctx := context.Background()
	sa := createSA(t, iamClient(t, inst), "bucket-user")
	who := emu.WithPrincipal(ctx, emu.Principal("serviceAccount:"+sa.Email))
	bucket := "//storage.googleapis.com/projects/_/buckets/b1"
	object := bucket + "/objects/dir/file.txt"

	if err := parents.SetResourceParent(ctx, bucket, "projects/"+testProject); err != nil {
		t.Fatal(err)
	}
	if err := inst.Env.Auth.Check(who, "storage.objects.get", object); err == nil {
		t.Fatal("expected denial before any binding")
	}
	// Project-level binding applies through the recorded parent.
	if _, err := svc.(interface {
		SetPolicyJSON(context.Context, string, []byte) ([]byte, error)
	}).SetPolicyJSON(ctx, "//cloudresourcemanager.googleapis.com/projects/"+testProject,
		[]byte(`{"bindings":[{"role":"roles/storage.objectViewer","members":["serviceAccount:`+sa.Email+`"]}]}`)); err != nil {
		t.Fatal(err)
	}
	if err := inst.Env.Auth.Check(who, "storage.objects.get", object); err != nil {
		t.Fatalf("project binding via parent: %v", err)
	}
	if got := tester.TestPermissions(who, object, []string{"storage.objects.get", "storage.objects.delete"}); len(got) != 1 {
		t.Fatalf("TestPermissions: %v", got)
	}

	// Bucket-level policy with etag concurrency.
	raw, err := store.GetPolicyJSON(ctx, bucket)
	if err != nil {
		t.Fatal(err)
	}
	var p iamv1.Policy
	_ = json.Unmarshal(raw, &p)
	p.Bindings = []*iamv1.Binding{{Role: "roles/storage.objectAdmin", Members: []string{"serviceAccount:" + sa.Email}}}
	b, _ := json.Marshal(&p)
	if _, err := store.SetPolicyJSON(ctx, bucket, b); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetPolicyJSON(ctx, bucket, b); err == nil || !strings.Contains(err.Error(), "concurrent") {
		t.Fatalf("stale etag: %v", err)
	}
	if err := inst.Env.Auth.Check(who, "storage.objects.delete", object); err != nil {
		t.Fatalf("bucket binding: %v", err)
	}
	if err := store.DeletePolicy(ctx, bucket); err != nil {
		t.Fatal(err)
	}
	if err := inst.Env.Auth.Check(who, "storage.objects.delete", object); err == nil {
		t.Fatal("expected denial after DeletePolicy")
	}

	// ServiceAccountKeys.
	kid, sig, err := keys.SignBlob(ctx, sa.Email, []byte("data"))
	if err != nil || kid == "" || len(sig) != 256 {
		t.Fatalf("SignBlob: %v", err)
	}
	pubs, err := keys.PublicKeys(ctx, sa.Email)
	if err != nil || len(pubs) != 1 {
		t.Fatalf("PublicKeys: %v %d", err, len(pubs))
	}
	tok, exp, err := keys.AccessToken(ctx, emu.Principal("serviceAccount:"+sa.Email))
	if err != nil || exp < 3500 {
		t.Fatalf("AccessToken: %v %d", err, exp)
	}
	if p, ok := inst.Env.Auth.Authenticate(ctx, tok); !ok || p != emu.Principal("serviceAccount:"+sa.Email) {
		t.Fatalf("Authenticate: %v %v", p, ok)
	}
	idt, err := keys.IDToken(ctx, sa.Email, "https://push.example")
	if err != nil {
		t.Fatal(err)
	}
	if c := verifyIDToken(t, inst, idt); c["email"] != sa.Email {
		t.Fatalf("IDToken claims: %v", c)
	}
}

func TestSeed(t *testing.T) {
	inst := startIAM(t)
	dir := t.TempDir()
	seed := filepath.Join(dir, "seed.yaml")
	_ = os.WriteFile(seed, []byte(`
iam:
  metadata: {project: seeded-project, zone: europe-west1-b}
  serviceAccounts:
    - {project: seeded-project, accountId: app-sa, displayName: App, keyFile: keys/app.json}
  customRoles:
    - {project: seeded-project, roleId: lister, title: Lister, permissions: [storage.buckets.list]}
  bindings:
    - project: seeded-project
      role: projects/seeded-project/roles/lister
      members: [serviceAccount:app-sa@seeded-project.iam.gserviceaccount.com]
`), 0o600)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := inst.ApplySeed(ctx, seed); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
	}
	kf, err := os.ReadFile(filepath.Join(dir, "keys/app.json"))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := iamClient(t, inst).Projects.ServiceAccounts.Keys.List("projects/-/serviceAccounts/app-sa@seeded-project.iam.gserviceaccount.com").KeyTypes("USER_MANAGED").Do()
	if err != nil || len(keys.Keys) != 1 {
		t.Fatalf("seed not idempotent: %v %+v", err, keys)
	}
	var f map[string]string
	_ = json.Unmarshal(kf, &f)
	if f["client_email"] != "app-sa@seeded-project.iam.gserviceaccount.com" {
		t.Fatalf("key file: %v", f)
	}
	pol, _ := crmClient(t, inst).Projects.GetIamPolicy("seeded-project", &crmv1.GetIamPolicyRequest{}).Do()
	if len(pol.Bindings) != 1 || len(pol.Bindings[0].Members) != 1 {
		t.Fatalf("seeded policy: %+v", pol.Bindings)
	}
	env := inst.EnvVars()
	if env["GCE_METADATA_HOST"] != inst.Endpoint("metadata") || env["CLOUDSDK_API_ENDPOINT_OVERRIDES_IAM"] != inst.GatewayURL()+"/iam/" {
		t.Fatalf("env vars: %v", env)
	}
}
