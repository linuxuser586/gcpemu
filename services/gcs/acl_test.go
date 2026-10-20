package gcs_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"cloud.google.com/go/storage"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/project"
)

// aclStrings renders ACL rules as sorted "ROLE:entity" pairs.
func aclStrings(rules []storage.ACLRule) []string {
	var out []string
	for _, r := range rules {
		out = append(out, string(r.Role)+":"+string(r.Entity))
	}
	slices.Sort(out)
	return out
}

// TestACLs covers bucket, default object and object access controls
// (FR-GCS-009) through the Go client.
func TestACLs(t *testing.T) {
	clientModes(t, func(t *testing.T, inst *emutest.Instance, c *storage.Client) {
		ctx := context.Background()
		num := project.NumberString(testProject)
		team := []string{"OWNER:project-editors-" + num, "OWNER:project-owners-" + num, "READER:project-viewers-" + num}
		b := c.Bucket("acl-bucket")
		if err := b.Create(ctx, testProject, nil); err != nil {
			t.Fatal(err)
		}

		// A new bucket's ACL and default object ACL are projectPrivate.
		rules, err := b.ACL().List(ctx)
		if err != nil || !slices.Equal(aclStrings(rules), team) {
			t.Fatalf("bucket ACL = %v %v", aclStrings(rules), err)
		}
		if rules, err := b.DefaultObjectACL().List(ctx); err != nil || !slices.Equal(aclStrings(rules), team) {
			t.Fatalf("default object ACL = %v %v", aclStrings(rules), err)
		}
		if err := b.ACL().Set(ctx, storage.AllUsers, storage.RoleReader); err != nil {
			t.Fatal(err)
		}
		if err := b.ACL().Set(ctx, "group-devs@example.com", storage.RoleWriter); err != nil {
			t.Fatal(err)
		}
		if err := b.ACL().Delete(ctx, "group-devs@example.com"); err != nil {
			t.Fatal(err)
		}
		rules, _ = b.ACL().List(ctx)
		if want := sorted(append([]string{"READER:allUsers"}, team...)); !slices.Equal(aclStrings(rules), want) {
			t.Errorf("bucket ACL after set/delete = %v, want %v", aclStrings(rules), want)
		}
		if err := b.ACL().Delete(ctx, "group-devs@example.com"); httpCode(err) != 404 {
			t.Errorf("delete missing entry = %v", err)
		}
		// The client reads attributes with projection=full.
		attrs, err := b.Attrs(ctx)
		if err != nil || len(attrs.ACL) != 4 || len(attrs.DefaultObjectACL) != 3 || attrs.MetaGeneration != 4 {
			t.Errorf("bucket attrs: acl %v default %v metageneration %d %v", attrs.ACL, attrs.DefaultObjectACL, attrs.MetaGeneration, err)
		}

		// New objects take the default object ACL plus their uploader as
		// owner; predefinedAcl replaces it.
		if err := b.DefaultObjectACL().Set(ctx, storage.AllAuthenticatedUsers, storage.RoleReader); err != nil {
			t.Fatal(err)
		}
		o := b.Object("dir/a.txt")
		write(t, ctx, o, []byte("a"), 0)
		owner := "OWNER:user-" + emu.Principal(inst.Internal().Config.DefaultPrincipal).Email()
		rules, err = o.ACL().List(ctx)
		if want := append([]string{owner, "READER:allAuthenticatedUsers"}, team...); err != nil || !slices.Equal(aclStrings(rules), sorted(want)) {
			t.Fatalf("object ACL = %v %v, want %v", aclStrings(rules), err, sorted(want))
		}
		if err := o.ACL().Set(ctx, storage.AllUsers, storage.RoleReader); err != nil {
			t.Fatal(err)
		}
		if err := o.ACL().Set(ctx, storage.AllUsers, storage.RoleWriter); httpCode(err) != 400 {
			t.Errorf("object WRITER = %v", err)
		}
		if oa, err := o.Attrs(ctx); err != nil || oa.Metageneration != 2 {
			t.Errorf("object metageneration after ACL set = %v %v", oa, err)
		}
		w := b.Object("public.txt").NewWriter(ctx)
		w.PredefinedACL = "publicRead"
		closeWriter(t, w, "p")
		rules, _ = b.Object("public.txt").ACL().List(ctx)
		if want := sorted([]string{owner, "READER:allUsers"}); !slices.Equal(aclStrings(rules), want) {
			t.Errorf("publicRead object ACL = %v, want %v", aclStrings(rules), want)
		}
		if _, err := b.Update(ctx, storage.BucketAttrsToUpdate{PredefinedDefaultObjectACL: "private"}); err != nil {
			t.Fatal(err)
		}
		if rules, err := b.DefaultObjectACL().List(ctx); err != nil || len(rules) != 0 {
			t.Errorf("private default object ACL = %v %v", aclStrings(rules), err)
		}
		write(t, ctx, b.Object("private.txt"), []byte("x"), 0)
		if rules, _ := b.Object("private.txt").ACL().List(ctx); !slices.Equal(aclStrings(rules), []string{owner}) {
			t.Errorf("object under private default = %v", aclStrings(rules))
		}

		// Uniform bucket-level access hides ACLs and refuses changes.
		u := c.Bucket("acl-ubla")
		if err := u.Create(ctx, testProject, &storage.BucketAttrs{UniformBucketLevelAccess: storage.UniformBucketLevelAccess{Enabled: true}}); err != nil {
			t.Fatal(err)
		}
		if _, err := u.ACL().List(ctx); httpCode(err) != 400 {
			t.Errorf("UBLA bucket ACL list = %v", err)
		}
		if err := u.DefaultObjectACL().Set(ctx, storage.AllUsers, storage.RoleReader); httpCode(err) != 400 {
			t.Errorf("UBLA default object ACL set = %v", err)
		}
		write(t, ctx, u.Object("o"), []byte("o"), 0)
		if _, err := u.Object("o").ACL().List(ctx); httpCode(err) != 400 {
			t.Errorf("UBLA object ACL list = %v", err)
		}
		w = u.Object("p").NewWriter(ctx)
		w.PredefinedACL = "publicRead"
		w.ChunkSize = 0
		_, _ = w.Write([]byte("p"))
		if err := w.Close(); httpCode(err) != 400 {
			t.Errorf("UBLA predefinedAcl upload = %v", err)
		}
	})
}

func sorted(s []string) []string {
	s = slices.Clone(s)
	slices.Sort(s)
	return s
}

func closeWriter(t *testing.T, w *storage.Writer, data string) {
	t.Helper()
	if _, err := w.Write([]byte(data)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestACLWire checks what the provider's ACL resources read: full
// projection on buckets and objects, PUT with predefined ACLs, and the
// JSON shape of access controls.
func TestACLWire(t *testing.T) {
	inst := start(t)
	base := inst.GatewayURL() + "/storage/v1"
	num := project.NumberString(testProject)
	jsonHdr := map[string]string{"Content-Type": "application/json"}
	code, _, body := do(t, "POST", base+"/b?project="+testProject+"&predefinedAcl=publicRead", strings.NewReader(`{"name":"wire-acl"}`), jsonHdr)
	if code != 200 {
		t.Fatalf("create = %d %s", code, body)
	}
	var bkt map[string]any
	_ = json.Unmarshal(body, &bkt)
	if _, ok := bkt["acl"]; ok {
		t.Errorf("noAcl create returned acl: %s", body)
	}
	code, _, body = do(t, "GET", base+"/b/wire-acl?projection=full", nil, nil)
	var full struct {
		Owner struct{ Entity string } `json:"owner"`
		ACL   []struct {
			Kind, ID, Entity, Role, Bucket string
			ProjectTeam                    *struct{ ProjectNumber, Team string } `json:"projectTeam"`
		} `json:"acl"`
		DefaultObjectACL []struct{ Entity, Role string } `json:"defaultObjectAcl"`
	}
	_ = json.Unmarshal(body, &full)
	if code != 200 || full.Owner.Entity != "project-owners-"+num || len(full.ACL) != 2 || len(full.DefaultObjectACL) != 3 {
		t.Fatalf("full bucket = %d %s", code, body)
	}
	if a := full.ACL[0]; a.Kind != "storage#bucketAccessControl" || a.ID != "wire-acl/project-owners-"+num || a.ProjectTeam == nil || a.ProjectTeam.Team != "owners" || a.ProjectTeam.ProjectNumber != num {
		t.Errorf("owners entry = %+v", a)
	}

	// The provider's default object ACL: GET noAcl, then PUT it back with
	// defaultObjectAcl set; the bucket ACL survives the PUT.
	_, _, body = do(t, "GET", base+"/b/wire-acl", nil, nil)
	var put map[string]any
	_ = json.Unmarshal(body, &put)
	put["defaultObjectAcl"] = []map[string]string{{"entity": "user-a@example.com", "role": "READER"}}
	pb, _ := json.Marshal(put)
	code, _, body = do(t, "PUT", base+"/b/wire-acl?ifMetagenerationMatch=1", strings.NewReader(string(pb)), jsonHdr)
	if code != 200 || !strings.Contains(string(body), `"defaultObjectAcl":[{`) || !strings.Contains(string(body), `"entity":"allUsers"`) {
		t.Fatalf("PUT defaultObjectAcl = %d %s", code, body)
	}
	code, _, body = do(t, "POST", base+"/b/wire-acl/acl", strings.NewReader(`{"entity":"bogus","role":"READER"}`), jsonHdr)
	if code != 400 {
		t.Errorf("invalid entity = %d %s", code, body)
	}

	// Object access controls by escaped name, and Objects.Update with
	// predefinedAcl as the provider's object ACL deletion sends it.
	code, _, body = do(t, "POST", inst.GatewayURL()+"/upload/storage/v1/b/wire-acl/o?uploadType=media&name=d/x.txt", strings.NewReader("x"), nil)
	if code != 200 {
		t.Fatalf("upload = %d %s", code, body)
	}
	code, _, body = do(t, "POST", base+"/b/wire-acl/o/d%2Fx.txt/acl", strings.NewReader(`{"entity":"domain-example.com","role":"READER"}`), jsonHdr)
	var oac map[string]any
	_ = json.Unmarshal(body, &oac)
	if code != 200 || oac["kind"] != "storage#objectAccessControl" || oac["domain"] != "example.com" || oac["object"] != "d/x.txt" || oac["generation"] == nil {
		t.Errorf("object ACL insert = %d %s", code, body)
	}
	code, _, body = do(t, "GET", base+"/b/wire-acl/o/d%2Fx.txt/acl/domain-example.com", nil, nil)
	if code != 200 || !strings.Contains(string(body), `"role":"READER"`) {
		t.Errorf("object ACL get = %d %s", code, body)
	}
	_, _, body = do(t, "GET", base+"/b/wire-acl/o/d%2Fx.txt", nil, nil)
	code, _, body = do(t, "PUT", base+"/b/wire-acl/o/d%2Fx.txt?predefinedAcl=private", strings.NewReader(string(body)), jsonHdr)
	var obj struct {
		Owner struct{ Entity string }         `json:"owner"`
		ACL   []struct{ Entity, Role string } `json:"acl"`
	}
	_ = json.Unmarshal(body, &obj)
	if code != 200 || obj.Owner.Entity == "" || len(obj.ACL) != 1 || obj.ACL[0].Entity != obj.Owner.Entity {
		t.Errorf("PUT predefinedAcl=private = %d %s", code, body)
	}
}

// TestManagedFolders covers managed folder CRUD and IAM over the JSON API
// as the provider calls it.
func TestManagedFolders(t *testing.T) {
	inst := start(t)
	base := inst.GatewayURL() + "/storage/v1"
	jsonHdr := map[string]string{"Content-Type": "application/json"}
	mk := func(name, extra string) {
		code, _, b := do(t, "POST", base+"/b?project="+testProject, strings.NewReader(`{"name":"`+name+`"`+extra+`}`), jsonHdr)
		if code != 200 {
			t.Fatalf("create %s = %d %s", name, code, b)
		}
	}
	mk("mf-bucket", `,"iamConfiguration":{"uniformBucketLevelAccess":{"enabled":true}}`)
	mk("mf-fine", "")
	folders := base + "/b/mf-bucket/managedFolders"
	if code, _, b := do(t, "POST", base+"/b/mf-fine/managedFolders", strings.NewReader(`{"name":"a/"}`), jsonHdr); code != 400 {
		t.Errorf("folder without UBLA = %d %s", code, b)
	}
	code, _, b := do(t, "POST", folders, strings.NewReader(`{"name":"dir/sub"}`), jsonHdr)
	var f map[string]any
	_ = json.Unmarshal(b, &f)
	if code != 200 || f["kind"] != "storage#managedFolder" || f["name"] != "dir/sub/" || f["id"] != "mf-bucket/dir/sub/" ||
		f["metageneration"] != "1" || !strings.HasSuffix(f["selfLink"].(string), "/b/mf-bucket/managedFolders/dir%2Fsub%2F") {
		t.Fatalf("insert = %d %s", code, b)
	}
	if code, _, b := do(t, "POST", folders, strings.NewReader(`{"name":"dir/sub/"}`), jsonHdr); code != 409 {
		t.Errorf("duplicate = %d %s", code, b)
	}
	if code, _, b := do(t, "POST", folders, strings.NewReader(`{"name":"dir/"}`), jsonHdr); code != 200 {
		t.Fatalf("insert dir/ = %d %s", code, b)
	}
	for _, p := range []string{"/dir%2Fsub%2F", "/dir/sub/", "/dir/sub"} {
		if code, _, b := do(t, "GET", folders+p, nil, nil); code != 200 || !strings.Contains(string(b), `"name":"dir/sub/"`) {
			t.Errorf("GET %s = %d %s", p, code, b)
		}
	}
	if code, _, _ := do(t, "GET", folders+"/nope%2F", nil, nil); code != 404 {
		t.Errorf("missing folder = %d", code)
	}
	code, _, b = do(t, "GET", folders+"?prefix=dir/s", nil, nil)
	var list struct {
		Kind  string
		Items []struct{ Name string }
	}
	_ = json.Unmarshal(b, &list)
	if code != 200 || list.Kind != "storage#managedFolders" || len(list.Items) != 1 || list.Items[0].Name != "dir/sub/" {
		t.Errorf("list = %d %s", code, b)
	}

	// IAM: a default empty policy, then set with etag checking.
	iamURL := folders + "/dir%2Fsub%2F/iam"
	code, _, b = do(t, "GET", iamURL+"?optionsRequestedPolicyVersion=3", nil, nil)
	if code != 200 || !strings.Contains(string(b), `"resourceId":"projects/_/buckets/mf-bucket/managedFolders/dir/sub/"`) || !strings.Contains(string(b), `"etag":"CAE="`) {
		t.Fatalf("get policy = %d %s", code, b)
	}
	set := `{"etag":"CAE=","bindings":[{"role":"roles/storage.objectViewer","members":["user:r@example.com"]}]}`
	if code, _, b := do(t, "PUT", iamURL, strings.NewReader(set), jsonHdr); code != 200 {
		t.Fatalf("set policy = %d %s", code, b)
	}
	if code, _, b := do(t, "PUT", iamURL, strings.NewReader(set), jsonHdr); code != 412 {
		t.Errorf("stale etag = %d %s", code, b)
	}
	if code, _, b := do(t, "GET", iamURL, nil, nil); code != 200 || !strings.Contains(string(b), "user:r@example.com") {
		t.Errorf("policy after set = %d %s", code, b)
	}
	if code, _, b := do(t, "GET", iamURL+"/testPermissions?permissions=storage.objects.get", nil, nil); code != 200 || !strings.Contains(string(b), "storage.objects.get") {
		t.Errorf("testPermissions = %d %s", code, b)
	}

	// Deleting needs an empty folder unless allowNonEmpty is set, and a
	// bucket with managed folders cannot be deleted.
	if code, _, b := do(t, "POST", inst.GatewayURL()+"/upload/storage/v1/b/mf-bucket/o?uploadType=media&name=dir/sub/x", strings.NewReader("x"), nil); code != 200 {
		t.Fatalf("upload = %d %s", code, b)
	}
	if code, _, _ := do(t, "DELETE", folders+"/dir%2F", nil, nil); code != 409 {
		t.Errorf("delete folder with a nested folder = %d", code)
	}
	if code, _, _ := do(t, "DELETE", folders+"/dir%2Fsub%2F?allowNonEmpty=false", nil, nil); code != 409 {
		t.Errorf("delete non-empty = %d", code)
	}
	if code, _, _ := do(t, "DELETE", folders+"/dir%2Fsub%2F?allowNonEmpty=true", nil, nil); code != 204 {
		t.Errorf("delete allowNonEmpty = %d", code)
	}
	if code, _, _ := do(t, "GET", iamURL, nil, nil); code != 404 {
		t.Errorf("policy of deleted folder = %d", code)
	}
	if code, _, _ := do(t, "DELETE", base+"/b/mf-bucket/o/dir%2Fsub%2Fx", nil, nil); code != 204 {
		t.Errorf("delete object = %d", code)
	}
	if code, _, _ := do(t, "DELETE", base+"/b/mf-bucket", nil, nil); code != 409 {
		t.Errorf("delete bucket with a managed folder = %d", code)
	}
	if code, _, _ := do(t, "DELETE", folders+"/dir%2F", nil, nil); code != 204 {
		t.Errorf("delete empty folder = %d", code)
	}
	if code, _, _ := do(t, "DELETE", base+"/b/mf-bucket", nil, nil); code != 204 {
		t.Errorf("delete bucket = %d", code)
	}
}

// TestManagedFolderIAM: under enforce, a managed folder's policy grants
// access to the objects in it and nothing outside it.
func TestManagedFolderIAM(t *testing.T) {
	inst := start(t, emutest.WithIAMMode(config.IAMEnforce))
	svc, _ := inst.Internal().Env.Lookup("iam")
	keys := svc.(emu.ServiceAccountKeys)
	token := func(p string) string {
		tok, _, err := keys.AccessToken(context.Background(), emu.Principal(p))
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	owner, reader := token(inst.Internal().Config.DefaultPrincipal), token("user:reader@example.com")
	call := func(tok, method, path, body string) int {
		t.Helper()
		req, _ := http.NewRequest(method, inst.GatewayURL()+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, s := range []struct{ method, path, body string }{
		{"POST", "/storage/v1/b?project=" + testProject, `{"name":"mf-iam","iamConfiguration":{"uniformBucketLevelAccess":{"enabled":true}}}`},
		{"POST", "/storage/v1/b/mf-iam/managedFolders", `{"name":"shared/"}`},
		{"POST", "/upload/storage/v1/b/mf-iam/o?uploadType=media&name=shared/deep/a.txt", "a"},
		{"POST", "/upload/storage/v1/b/mf-iam/o?uploadType=media&name=private/b.txt", "b"},
		{"POST", "/upload/storage/v1/b/mf-iam/o?uploadType=media&name=shared", "c"},
	} {
		if c := call(owner, s.method, s.path, s.body); c != 200 {
			t.Fatalf("owner %s %s: %d", s.method, s.path, c)
		}
	}
	get := func(name string) int {
		return call(reader, "GET", "/storage/v1/b/mf-iam/o/"+strings.ReplaceAll(name, "/", "%2F"), "")
	}
	if c := get("shared/deep/a.txt"); c != http.StatusForbidden {
		t.Fatalf("reader before binding: %d", c)
	}
	if c := call(owner, "PUT", "/storage/v1/b/mf-iam/managedFolders/shared%2F/iam",
		`{"bindings":[{"role":"roles/storage.objectViewer","members":["user:reader@example.com"]}]}`); c != 200 {
		t.Fatalf("set folder policy: %d", c)
	}
	if c := get("shared/deep/a.txt"); c != 200 {
		t.Errorf("reader in the folder: %d", c)
	}
	for _, name := range []string{"private/b.txt", "shared"} {
		if c := get(name); c != http.StatusForbidden {
			t.Errorf("reader on %s outside the folder: %d", name, c)
		}
	}
}
