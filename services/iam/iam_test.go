package iam_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/api/googleapi"
	iamv1 "google.golang.org/api/iam/v1"
	"google.golang.org/api/option"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/instance"
	"github.com/linuxuser586/gcpemu/services/iam"
)

const testProject = "test-project"

// testInstance mirrors emutest.Instance but boots only the iam factory so
// these tests do not depend on other service packages building.
type testInstance struct {
	*instance.Instance
}

func (i *testInstance) Endpoint(name string) string { return i.Env.Endpoints.Get(name) }
func (i *testInstance) GatewayURL() string          { return "http://" + i.Endpoint("gateway") }

func enforce(c *config.Config) { c.IAMMode = config.IAMEnforce }

// persistent uses dir as a --data-dir (non-ephemeral) instance directory.
func persistent(dir string) func(*config.Config) {
	return func(c *config.Config) { c.Ephemeral, c.DataDir = false, dir }
}

func startIAM(t *testing.T, opts ...func(*config.Config)) *testInstance {
	t.Helper()
	cfg := config.Defaults()
	cfg.Ephemeral = true
	cfg.DataDir = t.TempDir()
	cfg.Services = []string{"iam"}
	cfg.LogLevel = "warn"
	for k := range cfg.Ports {
		cfg.Ports[k] = 0
	}
	for _, o := range opts {
		o(&cfg)
	}
	var logOut io.Writer = io.Discard
	if os.Getenv("GCPEMU_TEST_LOG") == "1" {
		logOut = os.Stderr
		cfg.LogLevel = "debug"
	}
	in, err := instance.New(&cfg, map[string]instance.Factory{"iam": iam.New}, logOut)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := in.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = in.Shutdown(context.Background()) })
	if err := in.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	return &testInstance{in}
}

func (i *testInstance) stop(t *testing.T) {
	t.Helper()
	if err := i.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func iamClient(t *testing.T, inst *testInstance, opts ...option.ClientOption) *iamv1.Service {
	t.Helper()
	if len(opts) == 0 {
		opts = []option.ClientOption{option.WithoutAuthentication()}
	}
	opts = append(opts, option.WithEndpoint(inst.GatewayURL()+"/iam/"))
	c, err := iamv1.NewService(context.Background(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func httpCode(err error) int {
	if e, ok := err.(*googleapi.Error); ok {
		return e.Code
	}
	return 0
}

func createSA(t *testing.T, c *iamv1.Service, id string) *iamv1.ServiceAccount {
	t.Helper()
	sa, err := c.Projects.ServiceAccounts.Create("projects/"+testProject, &iamv1.CreateServiceAccountRequest{
		AccountId:      id,
		ServiceAccount: &iamv1.ServiceAccount{DisplayName: "SA " + id},
	}).Do()
	if err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
	return sa
}

func TestServiceAccountLifecycle(t *testing.T) {
	inst := startIAM(t)
	c := iamClient(t, inst)
	sa := createSA(t, c, "app-runner")
	if sa.Email != "app-runner@test-project.iam.gserviceaccount.com" || sa.ProjectId != testProject || len(sa.UniqueId) != 21 {
		t.Fatalf("unexpected account %+v", sa)
	}
	if _, err := c.Projects.ServiceAccounts.Create("projects/"+testProject, &iamv1.CreateServiceAccountRequest{AccountId: "app-runner"}).Do(); httpCode(err) != 409 {
		t.Fatalf("duplicate create: want 409, got %v", err)
	}
	if _, err := c.Projects.ServiceAccounts.Create("projects/"+testProject, &iamv1.CreateServiceAccountRequest{AccountId: "Bad"}).Do(); httpCode(err) != 400 {
		t.Fatalf("bad id: want 400, got %v", err)
	}
	name := "projects/-/serviceAccounts/" + sa.Email
	got, err := c.Projects.ServiceAccounts.Get(name).Do()
	if err != nil || got.UniqueId != sa.UniqueId {
		t.Fatalf("get: %v %+v", err, got)
	}
	if _, err := c.Projects.ServiceAccounts.Get("projects/-/serviceAccounts/" + sa.UniqueId).Do(); err != nil {
		t.Fatalf("get by unique id: %v", err)
	}
	patched, err := c.Projects.ServiceAccounts.Patch(name, &iamv1.PatchServiceAccountRequest{
		ServiceAccount: &iamv1.ServiceAccount{DisplayName: "Renamed", Description: "d"},
		UpdateMask:     "displayName",
	}).Do()
	if err != nil || patched.DisplayName != "Renamed" || patched.Description != "" {
		t.Fatalf("patch: %v %+v", err, patched)
	}
	createSA(t, c, "second-sa")
	list, err := c.Projects.ServiceAccounts.List("projects/" + testProject).PageSize(1).Do()
	if err != nil || len(list.Accounts) != 1 || list.NextPageToken == "" {
		t.Fatalf("list page 1: %v %+v", err, list)
	}
	list2, err := c.Projects.ServiceAccounts.List("projects/" + testProject).PageToken(list.NextPageToken).Do()
	if err != nil || len(list2.Accounts) != 1 || list2.NextPageToken != "" {
		t.Fatalf("list page 2: %v %+v", err, list2)
	}
	if _, err := c.Projects.ServiceAccounts.Disable(name, &iamv1.DisableServiceAccountRequest{}).Do(); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Projects.ServiceAccounts.Get(name).Do(); !got.Disabled {
		t.Fatal("not disabled")
	}
	if _, err := c.Projects.ServiceAccounts.Enable(name, &iamv1.EnableServiceAccountRequest{}).Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Projects.ServiceAccounts.Delete(name).Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Projects.ServiceAccounts.Get(name).Do(); httpCode(err) != 404 {
		t.Fatalf("get deleted: want 404, got %v", err)
	}
	res, err := c.Projects.ServiceAccounts.Undelete("projects/-/serviceAccounts/"+sa.UniqueId, &iamv1.UndeleteServiceAccountRequest{}).Do()
	if err != nil || res.RestoredAccount.Email != sa.Email {
		t.Fatalf("undelete: %v %+v", err, res)
	}
	if _, err := c.Projects.ServiceAccounts.Get(name).Do(); err != nil {
		t.Fatalf("get undeleted: %v", err)
	}
}

// TestPersistence checks accounts, keys and the OIDC signing key survive a
// restart with --data-dir, and the signing key is stored 0600.
func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	inst := startIAM(t, persistent(dir))
	sa := createSA(t, iamClient(t, inst), "persisted-sa")
	kid1 := fetchJSON[struct {
		Keys []map[string]string `json:"keys"`
	}](t, inst.GatewayURL()+"/oauth2/v3/certs").Keys[0]["kid"]
	inst.stop(t)

	keyPath := filepath.Join(dir, "data", "iam", "signing-key.pem")
	st, err := os.Stat(keyPath)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("signing key %s: %v %v", keyPath, err, st)
	}

	inst = startIAM(t, persistent(dir))
	if _, err := iamClient(t, inst).Projects.ServiceAccounts.Get("projects/-/serviceAccounts/" + sa.Email).Do(); err != nil {
		t.Fatalf("account after restart: %v", err)
	}
	kid2 := fetchJSON[struct {
		Keys []map[string]string `json:"keys"`
	}](t, inst.GatewayURL()+"/oauth2/v3/certs").Keys[0]["kid"]
	if kid1 != kid2 {
		t.Fatalf("signing key changed across restart: %s != %s", kid1, kid2)
	}
}
