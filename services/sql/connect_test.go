package sql_test

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/cloudsqlconn"
	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"
	iamv1 "google.golang.org/api/iam/v1"
	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// accessToken mints an emulator access token for principal.
func accessToken(t testing.TB, inst *emutest.Instance, principal string) *oauth2.Token {
	t.Helper()
	iam, _ := inst.Env.Lookup("iam")
	keys, ok := iam.(emu.ServiceAccountKeys)
	if !ok {
		t.Fatal("iam service does not provide tokens")
	}
	tok, exp, err := keys.AccessToken(context.Background(), emu.Principal(principal))
	if err != nil {
		t.Fatal(err)
	}
	return &oauth2.Token{AccessToken: tok, TokenType: "Bearer", Expiry: time.Now().Add(time.Duration(exp) * time.Second)}
}

// grantProjectRole adds a project-level IAM binding.
func grantProjectRole(t testing.TB, inst *emutest.Instance, role, member string) {
	t.Helper()
	iam, _ := inst.Env.Lookup("iam")
	ps := iam.(emu.IAMPolicyStore)
	pol := fmt.Sprintf(`{"bindings":[{"role":%q,"members":[%q]}]}`, role, member)
	if _, err := ps.SetPolicyJSON(context.Background(), "//cloudresourcemanager.googleapis.com/projects/"+testProject, []byte(pol)); err != nil {
		t.Fatal(err)
	}
}

// TestConnectivityAndIAM covers FR-SQL-004 (authorized networks on the
// public IP, deny then allow), FR-SQL-005 (cloudsqlconn through port 3307
// with a BUILT_IN user and with automatic IAM authentication) and
// FR-SQL-006 (IAM database users with an access token as password,
// checked for cloudsql.instances.login in enforce mode).
func TestConnectivityAndIAM(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	inst := emutest.Start(t, []string{"sql"}, emutest.WithIAMMode("enforce"))
	svc := adminClient(t, inst)
	ctx := context.Background()

	in := createInstance(t, svc, &sqladmin.DatabaseInstance{
		Name:            "conn",
		DatabaseVersion: "POSTGRES_17",
		RootPassword:    "rootpw",
		Settings: &sqladmin.Settings{
			IpConfiguration: &sqladmin.IpConfiguration{Ipv4Enabled: true},
			DatabaseFlags:   []*sqladmin.DatabaseFlags{{Name: "cloudsql.iam_authentication", Value: "on"}},
		},
	})
	ip := publicIP(in)

	// No authorized networks: the public IP refuses the host.
	_, err := connect(ctx, ip, 5432, "postgres", "rootpw", "postgres")
	if err == nil || !strings.Contains(err.Error(), "not authorized") {
		t.Fatalf("connect without authorized networks: %v", err)
	}
	// The published host port is subject to the same rule.
	hostAddr := inst.EnvVars()["GCPEMU_SQL_CONN"]
	if hostAddr == "" {
		t.Fatalf("GCPEMU_SQL_CONN missing from %v", inst.EnvVars())
	}
	hh, hp, _ := net.SplitHostPort(hostAddr)
	var hostPort int
	fmt.Sscan(hp, &hostPort)
	if _, err := connect(ctx, hh, hostPort, "postgres", "rootpw", "postgres"); err == nil {
		t.Fatal("host port admitted without authorized networks")
	}
	waitOp(t, svc, must(svc.Instances.Patch(testProject, "conn", &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{
		IpConfiguration: &sqladmin.IpConfiguration{Ipv4Enabled: true, AuthorizedNetworks: []*sqladmin.AclEntry{{Value: "127.0.0.1/32"}}},
	}}).Do()))
	for _, addr := range [][2]any{{ip, 5432}, {hh, hostPort}} {
		c, err := connect(ctx, addr[0].(string), addr[1].(int), "postgres", "rootpw", "postgres")
		if err != nil {
			t.Fatalf("connect %v after authorizing the host: %v", addr, err)
		}
		c.Close(ctx)
	}
	// Connections are in the request log (FR-CORE-061).
	var admitted, refused bool
	for _, e := range inst.Env.RequestLog.Entries("sql") {
		if e.Protocol != "postgres" || e.Resource != "projects/"+testProject+"/instances/conn" || e.Principal != "postgres" {
			continue
		}
		admitted = admitted || (e.Status == 0 && e.Code == "OK")
		refused = refused || (e.Status == 1 && e.Code == "28000")
	}
	if !admitted || !refused {
		t.Errorf("sql request log: %+v", inst.Env.RequestLog.Entries("sql"))
	}

	// IAM database users.
	const sa = "app-sa@" + testProject + ".iam.gserviceaccount.com"
	saUser := strings.TrimSuffix(sa, ".gserviceaccount.com")
	if _, err := svc.Users.Insert(testProject, "conn", &sqladmin.User{Name: sa, Type: "CLOUD_IAM_SERVICE_ACCOUNT"}).Do(); err == nil {
		t.Fatal("service account user with .gserviceaccount.com accepted")
	}
	waitOp(t, svc, must(svc.Users.Insert(testProject, "conn", &sqladmin.User{Name: saUser, Type: "CLOUD_IAM_SERVICE_ACCOUNT"}).Do()))
	waitOp(t, svc, must(svc.Users.Insert(testProject, "conn", &sqladmin.User{Name: "alice@example.com", Type: "CLOUD_IAM_USER"}).Do()))
	iamSvc, err := iamv1.NewService(ctx, option.WithEndpoint(inst.GatewayURL()+"/iam/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := iamSvc.Projects.ServiceAccounts.Create("projects/"+testProject, &iamv1.CreateServiceAccountRequest{AccountId: "app-sa"}).Do(); err != nil {
		t.Fatal(err)
	}
	saTok := accessToken(t, inst, "serviceAccount:"+sa)

	if _, err := connect(ctx, ip, 5432, saUser, saTok.AccessToken, "postgres"); err == nil || !strings.Contains(err.Error(), "cloudsql.instances.login") {
		t.Fatalf("IAM login without roles/cloudsql.instanceUser: %v", err)
	}
	grantProjectRole(t, inst, "roles/cloudsql.instanceUser", "serviceAccount:"+sa)
	c, err := connect(ctx, ip, 5432, saUser, saTok.AccessToken, "postgres")
	if err != nil {
		t.Fatalf("IAM login: %v", err)
	}
	var who string
	_ = c.QueryRow(ctx, "SELECT current_user").Scan(&who)
	c.Close(ctx)
	if who != saUser {
		t.Fatalf("current_user = %q, want %q", who, saUser)
	}
	if _, err := connect(ctx, ip, 5432, "alice@example.com", saTok.AccessToken, "postgres"); err == nil {
		t.Fatal("token of another principal accepted")
	}
	if _, err := connect(ctx, ip, 5432, saUser, "not-a-token", "postgres"); err == nil {
		t.Fatal("invalid token accepted")
	}

	// The Go connector (port 3307), BUILT_IN user. "owner" is the
	// emulator's default-principal token, used for the admin API.
	admin := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "owner", Expiry: time.Now().Add(time.Hour)})
	d, err := cloudsqlconn.NewDialer(ctx, cloudsqlconn.WithAdminAPIEndpoint(inst.GatewayURL()+"/"), cloudsqlconn.WithTokenSource(admin))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	queryVia(t, d, in.ConnectionName, "postgres", "rootpw", "postgres")

	// Automatic IAM authentication: the token travels in the ephemeral
	// certificate and no password is sent.
	di, err := cloudsqlconn.NewDialer(ctx, cloudsqlconn.WithAdminAPIEndpoint(inst.GatewayURL()+"/"),
		cloudsqlconn.WithIAMAuthN(), cloudsqlconn.WithIAMAuthNTokenSources(admin, oauth2.StaticTokenSource(saTok)))
	if err != nil {
		t.Fatal(err)
	}
	defer di.Close()
	if got := queryVia(t, di, in.ConnectionName, saUser, "", "postgres"); got != saUser {
		t.Fatalf("connector IAM current_user = %q", got)
	}
}

// queryVia connects through a cloudsqlconn dialer and returns current_user.
func queryVia(t testing.TB, d *cloudsqlconn.Dialer, icn, user, password, db string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := pgx.ParseConfig(fmt.Sprintf("user=%s dbname=%s sslmode=disable", user, db))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Password = password
	cfg.DialFunc = func(ctx context.Context, _, _ string) (net.Conn, error) { return d.Dial(ctx, icn) }
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connector dial as %s: %v", user, err)
	}
	defer conn.Close(ctx)
	var who string
	if err := conn.QueryRow(ctx, "SELECT current_user").Scan(&who); err != nil {
		t.Fatal(err)
	}
	return who
}
