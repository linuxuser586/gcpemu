package sql_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"strings"
	"testing"

	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	sqlv1 "google.golang.org/api/sqladmin/v1"
	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/emutest"
)

func httpCode(err error) (int, string) {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		reason := ""
		if len(ge.Errors) > 0 {
			reason = ge.Errors[0].Reason
		}
		return ge.Code, reason
	}
	return 0, ""
}

// TestAdminAPI exercises the control plane without a data plane (stopped
// instances need no container): both API versions, validation and error
// shapes, pagination, operations, flags and tiers, connectSettings,
// ephemeral and client certificates, deletion protection.
func TestAdminAPI(t *testing.T) {
	inst := emutest.Start(t, []string{"sql"})
	ctx := context.Background()
	svc := adminClient(t, inst)
	v1, err := sqlv1.NewService(ctx, option.WithEndpoint(inst.GatewayURL()+"/sqladmin/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	stopped := func(name string) *sqladmin.DatabaseInstance {
		return &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "POSTGRES_15", Region: "europe-west1",
			Settings: &sqladmin.Settings{Tier: "db-f1-micro", ActivationPolicy: "NEVER"}}
	}
	waitOp(t, svc, must(svc.Instances.Insert(testProject, stopped("a")).Do()))
	waitOp(t, svc, must(svc.Instances.Insert(testProject, stopped("b")).Do()))

	// Validation errors.
	for _, tc := range []struct {
		in     *sqladmin.DatabaseInstance
		code   int
		reason string
	}{
		{stopped("a"), http.StatusConflict, "instanceAlreadyExists"},
		{stopped("Bad_Name"), http.StatusBadRequest, "invalid"},
		{&sqladmin.DatabaseInstance{Name: "m", DatabaseVersion: "MYSQL_8_0"}, http.StatusNotImplemented, "notImplemented"},
		{&sqladmin.DatabaseInstance{Name: "r", Region: "mars-north1"}, http.StatusBadRequest, "invalid"},
		{&sqladmin.DatabaseInstance{Name: "f", Settings: &sqladmin.Settings{DatabaseFlags: []*sqladmin.DatabaseFlags{{Name: "max_connections", Value: "5"}}}}, http.StatusBadRequest, "invalidFlagValue"},
		{&sqladmin.DatabaseInstance{Name: "n", Settings: &sqladmin.Settings{IpConfiguration: &sqladmin.IpConfiguration{Ipv4Enabled: false, ForceSendFields: []string{"Ipv4Enabled"}}}}, http.StatusBadRequest, "invalid"},
	} {
		_, err := svc.Instances.Insert(testProject, tc.in).Do()
		if code, reason := httpCode(err); code != tc.code || reason != tc.reason {
			t.Errorf("insert %s: got %d %q (%v), want %d %q", tc.in.Name, code, reason, err, tc.code, tc.reason)
		}
	}
	if _, err := svc.Instances.Get(testProject, "nope").Do(); func() bool { c, r := httpCode(err); return c != 404 || r != "instanceDoesNotExist" }() {
		t.Errorf("get missing: %v", err)
	}

	// v1 client: get and paginated list.
	a, err := v1.Instances.Get(testProject, "a").Do()
	if err != nil {
		t.Fatal(err)
	}
	if a.State != "RUNNABLE" || a.Settings.ActivationPolicy != "NEVER" || a.ConnectionName != testProject+":europe-west1:a" ||
		a.DatabaseInstalledVersion != "POSTGRES_15_18" || !strings.HasPrefix(a.SelfLink, "https://sqladmin.googleapis.com/v1/") ||
		a.Settings.SettingsVersion != 1 || a.Settings.Edition != "ENTERPRISE" || a.Settings.AvailabilityType != "ZONAL" ||
		!strings.HasSuffix(a.ServiceAccountEmailAddress, "@gcp-sa-cloud-sql.iam.gserviceaccount.com") {
		t.Fatalf("v1 get = %+v", a)
	}
	l1, err := v1.Instances.List(testProject).MaxResults(1).Do()
	if err != nil || len(l1.Items) != 1 || l1.NextPageToken == "" {
		t.Fatalf("list page 1: %+v %v", l1, err)
	}
	l2, err := v1.Instances.List(testProject).MaxResults(1).PageToken(l1.NextPageToken).Do()
	if err != nil || len(l2.Items) != 1 || l2.Items[0].Name != "b" || l2.NextPageToken != "" {
		t.Fatalf("list page 2: %+v %v", l2, err)
	}
	if l, _ := svc.Instances.List(testProject).Filter("name:b").Do(); len(l.Items) != 1 {
		t.Errorf("filter name:b = %d items", len(l.Items))
	}

	// Patch a stopped instance: recorded settings and flags, no container.
	op := waitOp(t, svc, must(svc.Instances.Patch(testProject, "a", &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{
		Tier: "db-custom-2-7680", DataDiskSizeGb: 50, AvailabilityType: "REGIONAL",
		DatabaseFlags: []*sqladmin.DatabaseFlags{{Name: "log_statement", Value: "ddl"}},
		UserLabels:    map[string]string{"env": "test"},
	}}).Do()))
	if op.OperationType != "UPDATE" {
		t.Errorf("patch operation type = %s", op.OperationType)
	}
	got := must(svc.Instances.Get(testProject, "a").Do())
	if got.Settings.Tier != "db-custom-2-7680" || got.Settings.DataDiskSizeGb != 50 || got.Settings.AvailabilityType != "REGIONAL" ||
		got.Settings.SettingsVersion != 2 || len(got.Settings.DatabaseFlags) != 1 || got.Settings.UserLabels["env"] != "test" || got.Region != "europe-west1" {
		t.Fatalf("after patch = %+v", got.Settings)
	}
	// Update (PUT) replaces the flag list.
	got.Settings.DatabaseFlags = nil
	waitOp(t, svc, must(svc.Instances.Update(testProject, "a", got).Do()))
	if g := must(svc.Instances.Get(testProject, "a").Do()); len(g.Settings.DatabaseFlags) != 0 || g.Settings.Tier != "db-custom-2-7680" {
		t.Fatalf("after update flags = %+v tier = %s", g.Settings.DatabaseFlags, g.Settings.Tier)
	}
	if _, err := svc.Instances.Patch(testProject, "a", &sqladmin.DatabaseInstance{Region: "us-east1"}).Do(); err == nil {
		t.Error("region change accepted")
	}

	// Child resources need a running instance.
	if _, err := svc.Databases.Insert(testProject, "a", &sqladmin.Database{Name: "x"}).Do(); func() bool { c, _ := httpCode(err); return c != 400 }() {
		t.Errorf("database on stopped instance: %v", err)
	}
	dbs := must(svc.Databases.List(testProject, "a").Do())
	users := must(svc.Users.List(testProject, "a").Do())
	if len(dbs.Items) != 1 || dbs.Items[0].Name != "postgres" || dbs.Items[0].Charset != "UTF8" || len(users.Items) != 1 || users.Items[0].Name != "postgres" {
		t.Fatalf("default databases %+v users %+v", dbs.Items, users.Items)
	}

	// Operations: newest first, v1beta4 links.
	ops := must(svc.Operations.List(testProject).Instance("a").Do())
	if len(ops.Items) != 3 || ops.Items[0].OperationType != "UPDATE" || ops.Items[2].OperationType != "CREATE" ||
		!strings.HasPrefix(ops.Items[0].SelfLink, "https://sqladmin.googleapis.com/sql/v1beta4/projects/") || ops.Items[0].Status != "DONE" {
		t.Fatalf("operations = %+v", ops.Items)
	}

	// Flags and tiers.
	fl := must(svc.Flags.List().DatabaseVersion("POSTGRES_16").Do())
	var maxConn *sqladmin.Flag
	for _, f := range fl.Items {
		if f.Name == "max_connections" {
			maxConn = f
		}
	}
	if maxConn == nil || !maxConn.RequiresRestart || maxConn.Type != "INTEGER" || maxConn.MinValue != 14 {
		t.Fatalf("max_connections flag = %+v", maxConn)
	}
	if tl := must(svc.Tiers.List(testProject).Do()); len(tl.Items) == 0 || tl.Items[0].Kind != "sql#tier" {
		t.Fatalf("tiers = %+v", tl)
	}

	// connectSettings and ephemeral certificates (FR-SQL-005).
	cs := must(svc.Connect.Get(testProject, "a").Do())
	if cs.BackendType != "SECOND_GEN" || cs.Region != "europe-west1" || cs.ServerCaCert == nil || cs.DatabaseVersion != "POSTGRES_15" {
		t.Fatalf("connectSettings = %+v", cs)
	}
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	eph := must(svc.Connect.GenerateEphemeralCert(testProject, "a", &sqladmin.GenerateEphemeralCertRequest{
		PublicKey: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: der})),
	}).Do())
	b, _ := pem.Decode([]byte(eph.EphemeralCert.Cert))
	cert, err := x509.ParseCertificate(b.Bytes)
	if err != nil || cert.Issuer.CommonName != "Google Cloud SQL Client CA" || !cert.PublicKey.(*rsa.PublicKey).Equal(&key.PublicKey) {
		t.Fatalf("ephemeral cert: %v %+v", err, cert)
	}
	if _, err := svc.Connect.GenerateEphemeralCert(testProject, "a", &sqladmin.GenerateEphemeralCertRequest{
		PublicKey: eph.EphemeralCert.Cert, AccessToken: "bogus",
	}).Do(); err == nil {
		t.Error("ephemeral cert with an invalid access token accepted")
	}
	cas := must(svc.Instances.ListServerCas(testProject, "a").Do())
	if len(cas.Certs) != 1 || !strings.Contains(cas.Certs[0].CommonName, "Google Cloud SQL Server CA") || cas.ActiveVersion != cas.Certs[0].Sha1Fingerprint {
		t.Fatalf("server CAs = %+v", cas)
	}

	// sslCerts.
	ins := must(svc.SslCerts.Insert(testProject, "a", &sqladmin.SslCertsInsertRequest{CommonName: "laptop"}).Do())
	if ins.ClientCert == nil || !strings.Contains(ins.ClientCert.CertPrivateKey, "PRIVATE KEY") || ins.Operation.Status != "DONE" {
		t.Fatalf("sslCerts.insert = %+v", ins)
	}
	if l := must(svc.SslCerts.List(testProject, "a").Do()); len(l.Items) != 1 || l.Items[0].CommonName != "laptop" {
		t.Fatalf("sslCerts.list = %+v", l.Items)
	}
	if _, err := svc.SslCerts.Insert(testProject, "a", &sqladmin.SslCertsInsertRequest{CommonName: "laptop"}).Do(); err == nil {
		t.Error("duplicate common name accepted")
	}
	waitOp(t, svc, must(svc.SslCerts.Delete(testProject, "a", ins.ClientCert.CertInfo.Sha1Fingerprint).Do()))
	if _, err := svc.SslCerts.Get(testProject, "a", ins.ClientCert.CertInfo.Sha1Fingerprint).Do(); err == nil {
		t.Error("deleted cert still returned")
	}

	// Deletion protection.
	waitOp(t, svc, must(svc.Instances.Patch(testProject, "b", &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{DeletionProtectionEnabled: true}}).Do()))
	if _, err := svc.Instances.Delete(testProject, "b").Do(); err == nil {
		t.Error("protected instance deleted")
	}
	waitOp(t, svc, must(svc.Instances.Patch(testProject, "b", &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{
		DeletionProtectionEnabled: false, ForceSendFields: []string{"DeletionProtectionEnabled"},
	}}).Do()))
	waitOp(t, svc, must(svc.Instances.Delete(testProject, "b").Do()))
	if _, err := svc.Instances.Get(testProject, "b").Do(); err == nil {
		t.Error("deleted instance still returned")
	}
	if _, err := svc.Operations.Get(testProject, "no-such-op").Do(); func() bool { c, _ := httpCode(err); return c != 404 }() {
		t.Errorf("missing operation: %v", err)
	}
}

// TestEnforce checks that admin calls require cloudsql.* permissions in
// enforce mode.
func TestEnforce(t *testing.T) {
	inst := emutest.Start(t, []string{"sql"}, emutest.WithIAMMode("enforce"))
	tok := accessToken(t, inst, "user:mallory@example.com")
	svc, err := sqladmin.NewService(context.Background(), option.WithEndpoint(inst.GatewayURL()+"/"), option.WithTokenSource(staticTS(tok.AccessToken)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Instances.List(testProject).Do()
	if c, _ := httpCode(err); c != http.StatusForbidden || !strings.Contains(err.Error(), "cloudsql.instances.list") {
		t.Fatalf("list without permission: %v", err)
	}
	grantProjectRole(t, inst, "roles/cloudsql.viewer", "user:mallory@example.com")
	if _, err := svc.Instances.List(testProject).Do(); err != nil {
		t.Fatalf("list as viewer: %v", err)
	}
	if _, err := svc.Instances.Insert(testProject, &sqladmin.DatabaseInstance{Name: "x"}).Do(); func() bool { c, _ := httpCode(err); return c != 403 }() {
		t.Fatalf("insert as viewer: %v", err)
	}
}
