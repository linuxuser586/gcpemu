package sql

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
	"github.com/linuxuser586/gcpemu/services/sql/proxy"
)

func TestValidateFlags(t *testing.T) {
	cases := []struct {
		name, value string
		ok          bool
	}{
		{"max_connections", "100", true},
		{"max_connections", "13", false},
		{"max_connections", "abc", false},
		{"log_connections", "on", true},
		{"log_connections", "true", false},
		{"random_page_cost", "1.1", true},
		{"random_page_cost", "-1", false},
		{"log_statement", "ddl", true},
		{"log_statement", "everything", false},
		{"pgaudit.log", "read,write", true},
		{"pgaudit.log", "read,bogus", false},
		{"timezone", "Europe/Paris", true},
		{"cloudsql.iam_authentication", "on", true},
		{"no_such_flag", "1", false},
	}
	for _, c := range cases {
		_, err := validateFlags([]*sqladmin.DatabaseFlags{{Name: c.name, Value: c.value}}, "POSTGRES_16")
		if (err == nil) != c.ok {
			t.Errorf("%s=%s: err=%v, want ok=%v", c.name, c.value, err, c.ok)
		}
	}
	if _, err := validateFlags([]*sqladmin.DatabaseFlags{{Name: "work_mem", Value: "64"}, {Name: "work_mem", Value: "128"}}, "POSTGRES_16"); err == nil {
		t.Error("duplicate flag accepted")
	}
}

func TestFlagApplication(t *testing.T) {
	got := pgSettings(map[string]string{"work_mem": "4096", "cloudsql.iam_authentication": "on", "cloudsql.logical_decoding": "on", "pg_stat_statements.track": "all"})
	want := map[string]string{"work_mem": "4096", "wal_level": "logical"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pgSettings = %v, want %v", got, want)
	}
	if !needsRestart(map[string]string{}, map[string]string{"max_connections": "50"}) {
		t.Error("max_connections must require a restart")
	}
	if needsRestart(map[string]string{"work_mem": "1"}, map[string]string{"work_mem": "2"}) {
		t.Error("work_mem must not require a restart")
	}
	q := alterSystemSQL(map[string]string{"work_mem": "1", "lock_timeout": "5"}, map[string]string{"work_mem": "2", "log_statement": "it's"})
	for _, want := range []string{`ALTER SYSTEM RESET "lock_timeout";`, `ALTER SYSTEM SET "work_mem" = '2';`, `ALTER SYSTEM SET "log_statement" = 'it''s';`} {
		if !strings.Contains(q, want) {
			t.Errorf("alterSystemSQL missing %q in:\n%s", want, q)
		}
	}
}

func TestMergeAndDefaults(t *testing.T) {
	dst := map[string]any{"settings": map[string]any{"tier": "a", "userLabels": map[string]any{"x": "1"}, "databaseFlags": []any{1}}}
	merge(dst, map[string]any{"settings": map[string]any{"tier": "b", "databaseFlags": []any{2, 3}, "userLabels": nil}})
	st := dst["settings"].(map[string]any)
	if st["tier"] != "b" || len(st["databaseFlags"].([]any)) != 2 || st["userLabels"] != nil {
		t.Fatalf("merge = %v", dst)
	}

	s := &sqladmin.Settings{IpConfiguration: &sqladmin.IpConfiguration{RequireSsl: true}}
	applySettingsDefaults(s, map[string]any{"ipConfiguration": map[string]any{"requireSsl": true}}, "us-central1-b", true)
	if s.IpConfiguration.Ipv4Enabled != true || s.IpConfiguration.SslMode != "TRUSTED_CLIENT_CERTIFICATE_REQUIRED" ||
		s.ActivationPolicy != "ALWAYS" || s.Tier != defaultTier || *s.StorageAutoResize != true || s.LocationPreference.Zone != "us-central1-b" {
		t.Fatalf("defaults = %+v %+v", s, s.IpConfiguration)
	}
	s2 := &sqladmin.Settings{IpConfiguration: &sqladmin.IpConfiguration{}}
	applySettingsDefaults(s2, map[string]any{"ipConfiguration": map[string]any{"ipv4Enabled": false}}, "z", true)
	if s2.IpConfiguration.Ipv4Enabled {
		t.Error("explicit ipv4Enabled=false overridden")
	}
	s3 := &sqladmin.Settings{Tier: "db-perf-optimized-N-2", IpConfiguration: &sqladmin.IpConfiguration{SslMode: "ENCRYPTED_ONLY", RequireSsl: true}}
	applySettingsDefaults(s3, nil, "z", false)
	if s3.Edition != "ENTERPRISE_PLUS" || s3.IpConfiguration.RequireSsl {
		t.Errorf("edition=%s requireSsl=%v", s3.Edition, s3.IpConfiguration.RequireSsl)
	}
}

func TestNetworkHelpers(t *testing.T) {
	for _, c := range []struct {
		value, ip string
		ok        bool
	}{
		{"0.0.0.0/0", "203.0.113.9", true},
		{"10.0.0.0/8", "10.1.2.3", true},
		{"10.0.0.0/8", "11.1.2.3", false},
		{"192.0.2.1", "192.0.2.1", true},
		{"192.0.2.1", "192.0.2.2", false},
	} {
		if got := aclMatches(c.value, c.ip); got != c.ok {
			t.Errorf("aclMatches(%s, %s) = %v", c.value, c.ip, got)
		}
	}
	for _, c := range []struct{ in, want string }{
		{"default", "projects/p/global/networks/default"},
		{"https://www.googleapis.com/compute/v1/projects/q/global/networks/vpc", "projects/q/global/networks/vpc"},
		{"projects/q/global/networks/vpc", "projects/q/global/networks/vpc"},
	} {
		if got := normalizeNetwork("p", c.in); got != c.want {
			t.Errorf("normalizeNetwork(%s) = %s", c.in, got)
		}
	}
	if !validCIDR("1.2.3.0/24") || validCIDR("1.2.3.0/33") || validCIDR("1.2.3/24") || !validIPv4("8.8.8.8") || validIPv4("256.1.1.1") {
		t.Error("CIDR/IP validation")
	}
	if b, o, err := parseGCSURI("gs://bkt/dir/file.sql.gz"); err != nil || b != "bkt" || o != "dir/file.sql.gz" {
		t.Errorf("parseGCSURI = %s %s %v", b, o, err)
	}
	if _, _, err := parseGCSURI("s3://x/y"); err == nil {
		t.Error("non-gs URI accepted")
	}
}

func TestIAMUserMatching(t *testing.T) {
	sa := &sqladmin.User{Name: "app@p.iam", Type: "CLOUD_IAM_SERVICE_ACCOUNT"}
	user := &sqladmin.User{Name: "Alice@Example.com", Type: "CLOUD_IAM_USER"}
	for _, c := range []struct {
		u  *sqladmin.User
		p  string
		ok bool
	}{
		{sa, "serviceAccount:app@p.iam.gserviceaccount.com", true},
		{sa, "serviceAccount:other@p.iam.gserviceaccount.com", false},
		{sa, "user:app@p.iam", false},
		{user, "user:alice@example.com", true},
		{user, "serviceAccount:alice@example.com", false},
	} {
		if got := iamUserMatches(c.u, emu.Principal(c.p)); got != c.ok {
			t.Errorf("iamUserMatches(%s, %s) = %v", c.u.Name, c.p, got)
		}
	}
	for _, c := range []struct {
		u  sqladmin.User
		ok bool
	}{
		{sqladmin.User{Name: "app"}, true},
		{sqladmin.User{Name: "cloudsqladmin"}, false},
		{sqladmin.User{Name: "a@p.iam", Type: "CLOUD_IAM_SERVICE_ACCOUNT"}, true},
		{sqladmin.User{Name: "a@p.iam.gserviceaccount.com", Type: "CLOUD_IAM_SERVICE_ACCOUNT"}, false},
		{sqladmin.User{Name: "bob", Type: "CLOUD_IAM_USER"}, false},
		{sqladmin.User{Name: "g@x.com", Type: "CLOUD_IAM_GROUP"}, false},
	} {
		if err := validateUser(&c.u); (err == nil) != c.ok {
			t.Errorf("validateUser(%+v) = %v", c.u, err)
		}
	}
	if q := createRoleSQL(&sqladmin.User{Name: `we"ird`}, "p'w"); !strings.Contains(q, `"we""ird"`) || !strings.Contains(q, `'p''w'`) || !strings.Contains(q, "cloudsqlsuperuser") {
		t.Errorf("createRoleSQL = %s", q)
	}
}

// testService builds a service on a memory store without a runtime.
func testService(t *testing.T, mode string) *Service {
	t.Helper()
	cfg := config.Defaults()
	cfg.IAMMode = mode
	env := &emu.Env{
		Config: &cfg, Store: store.NewMemory(), Clock: clock.Real{}, IDs: emu.NewIDs(false),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: emu.NewPolicyAuthorizer(mode, slog.New(slog.NewTextHandler(io.Discard, nil))),
		Endpoints: emu.NewEndpoints(),
	}
	return New(env).(*Service)
}

func TestDecideLogin(t *testing.T) {
	s := testService(t, config.IAMOff)
	rec := &instanceRecord{PublicIP: "172.30.0.5", PrivateIP: "10.9.0.3", Instance: &sqladmin.DatabaseInstance{
		Project: "p", Name: "i", ConnectionName: "p:r:i",
		Settings: &sqladmin.Settings{IpConfiguration: &sqladmin.IpConfiguration{Ipv4Enabled: true, SslMode: "ALLOW_UNENCRYPTED_AND_ENCRYPTED",
			AuthorizedNetworks: []*sqladmin.AclEntry{{Value: "198.51.100.0/24"}}}},
	}}
	if err := initPKI(rec, "p", "i"); err != nil {
		t.Fatal(err)
	}
	_ = s.env.Store.Update(func(tx store.Tx) error {
		_ = store.PutJSON(tx, nsInstances, "p/i", rec)
		return store.PutJSON(tx, nsUsers, "p/i/sa@p.iam", &userRecord{User: newUser("p", "i", "sa@p.iam", "CLOUD_IAM_SERVICE_ACCOUNT")})
	})
	ctx := context.Background()
	login := func(req proxy.LoginRequest) string {
		r := s.decideLogin(ctx, rec, &req)
		return r.Action
	}
	direct := proxy.LoginRequest{Listener: proxy.ListenerDirect, Local: "172.30.0.5", User: "postgres", Database: "postgres"}

	req := direct
	req.Remote = "203.0.113.1"
	if a := login(req); a != proxy.ActionDeny {
		t.Errorf("unauthorized network: %s", a)
	}
	req.Remote = "198.51.100.7"
	if a := login(req); a != proxy.ActionPassthrough {
		t.Errorf("authorized network: %s", a)
	}
	req = direct
	req.Local, req.Remote = "10.9.0.3", "10.9.0.77" // private IP: exempt
	if a := login(req); a != proxy.ActionPassthrough {
		t.Errorf("private IP: %s", a)
	}

	rec.Instance.Settings.IpConfiguration.SslMode = "ENCRYPTED_ONLY"
	req = direct
	req.Remote = "198.51.100.7"
	if a := login(req); a != proxy.ActionDeny {
		t.Errorf("plaintext with ENCRYPTED_ONLY: %s", a)
	}
	req.TLS = true
	if a := login(req); a != proxy.ActionPassthrough {
		t.Errorf("TLS with ENCRYPTED_ONLY: %s", a)
	}

	// IAM user: needs the flag, then asks for the token.
	req.User = "sa@p.iam"
	if r := s.decideLogin(ctx, rec, &req); r.Action != proxy.ActionDeny || !strings.Contains(r.Message, "cloudsql.iam_authentication") {
		t.Errorf("IAM without flag: %+v", r)
	}
	rec.Instance.Settings.DatabaseFlags = []*sqladmin.DatabaseFlags{{Name: "cloudsql.iam_authentication", Value: "on"}}
	if a := login(req); a != proxy.ActionPassword {
		t.Errorf("IAM first call: %s", a)
	}
	bad := "garbage"
	req.Password = &bad
	if a := login(req); a != proxy.ActionDeny {
		t.Errorf("IAM bad token: %s", a)
	}

	// Connector with an IAM ephemeral certificate: trusted without a password.
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	certPEM, err := newEphemeralCert(rec, &key.PublicKey, "serviceAccount:sa@p.iam.gserviceaccount.com", 0)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := pem.Decode([]byte(certPEM))
	conn := proxy.LoginRequest{Listener: proxy.ListenerConnector, Local: "172.30.0.5", Remote: "203.0.113.1", TLS: true, ClientCert: b.Bytes, User: "sa@p.iam"}
	if a := login(conn); a != proxy.ActionTrust {
		t.Errorf("connector IAM: %s", a)
	}
	conn.User = "postgres"
	if a := login(conn); a != proxy.ActionPassthrough {
		t.Errorf("connector built-in: %s", a)
	}
	// A certificate from another CA is refused.
	other := &instanceRecord{}
	_ = initPKI(other, "p", "j")
	foreign, _ := newEphemeralCert(other, &key.PublicKey, "", 0)
	fb, _ := pem.Decode([]byte(foreign))
	conn.ClientCert = fb.Bytes
	if a := login(conn); a != proxy.ActionDeny {
		t.Errorf("foreign certificate: %s", a)
	}
	// Client certificates must still exist (sslCerts) unless ephemeral.
	cp, _, _ := newClientCert(rec, "laptop")
	cb, _ := pem.Decode([]byte(cp))
	conn.ClientCert = cb.Bytes
	if a := login(conn); a != proxy.ActionDeny {
		t.Errorf("unregistered client certificate: %s", a)
	}
}

func TestPKI(t *testing.T) {
	rec := &instanceRecord{}
	if err := initPKI(rec, "proj", "inst"); err != nil {
		t.Fatal(err)
	}
	srv, _ := parseCert(rec.ServerCertPEM)
	ca, _ := parseCert(rec.ServerCAPEM)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	if _, err := srv.Verify(x509.VerifyOptions{Roots: pool}); err != nil || srv.Subject.CommonName != "proj:inst" {
		t.Fatalf("server cert: %v CN=%s", err, srv.Subject.CommonName)
	}
	sc := sslCertOf(rec.ServerCAPEM, "proj", "inst")
	if sc.CommonName != `C=US,O=Google\, Inc,CN=Google Cloud SQL Server CA` || len(sc.Sha1Fingerprint) != 40 {
		t.Errorf("server CA sslCert = %+v", sc)
	}
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	pub, err := parsePublicKey(string(pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: der})))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := newEphemeralCert(rec, pub, "user:a@b.c", 0)
	b, _ := pem.Decode([]byte(p))
	info, err := verifyClientCert(rec, b.Bytes)
	if err != nil || !info.Ephemeral || info.Principal != "user:a@b.c" {
		t.Fatalf("verifyClientCert = %+v %v", info, err)
	}
}

func TestMatchesFilter(t *testing.T) {
	in := &sqladmin.DatabaseInstance{Name: "a", State: "RUNNABLE", Region: "us-east1", Settings: &sqladmin.Settings{UserLabels: map[string]string{"env": "dev"}}}
	for f, want := range map[string]bool{
		"":                                     true,
		"name:a":                               true,
		"name:b":                               false,
		"state:RUNNABLE AND region:us-east1":   true,
		"settings.userLabels.env:dev":          true,
		`settings.userLabels.env="prod"`:       false,
		"databaseVersion=POSTGRES_17 region:x": false,
	} {
		if got := matchesFilter(in, f); got != want {
			t.Errorf("filter %q = %v", f, got)
		}
	}
}

func TestBootstrapSQL(t *testing.T) {
	rec := &instanceRecord{RootPassword: "it's", Instance: &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{
		DatabaseFlags: []*sqladmin.DatabaseFlags{{Name: "work_mem", Value: "8192"}},
	}}}
	q := bootstrapSQL(rec)
	for _, want := range []string{"CREATE ROLE cloudsqlsuperuser", "PASSWORD 'it''s'", "GRANT cloudsqlsuperuser TO postgres", `ALTER SYSTEM SET "work_mem" = '8192'`} {
		if !strings.Contains(q, want) {
			t.Errorf("bootstrap SQL lacks %q", want)
		}
	}
	if strings.Contains(q, "SUPERUSER") {
		t.Error("postgres must not be a superuser")
	}
}
