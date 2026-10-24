package sql_test

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/cloudsqlconn"
	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/frontend"
)

// lanAddr is the instance's Postgres address on the --sql-bind address.
func lanAddr(t *testing.T, inst *emutest.Instance, in *sqladmin.DatabaseInstance) (string, int) {
	t.Helper()
	ep := inst.Endpoint("sql-lan:" + in.ConnectionName)
	host, port, err := net.SplitHostPort(strings.Split(ep, ",")[0])
	if err != nil {
		t.Fatalf("sql-lan:%s endpoint %q: %v", in.ConnectionName, ep, err)
	}
	p, _ := strconv.Atoi(port)
	return host, p
}

// TestSQLBind is #620's acceptance on one machine, 127.0.0.1 standing in
// for the LAN address: psql reaches each instance on a port of its own
// there with its user's password and is refused without it; a caller
// coming through the LAN (here --gfe-bind, with a token the Instance
// issued) is told the address, and the Go connector reaches each of two
// instances through its shared port 3307; loopback callers see what they
// always did.
func TestSQLBind(t *testing.T) {
	emutest.RequireRuntime(t)
	inst := emutest.Start(t, []string{"iam", "sql"}, emutest.WithSQLHostClients(), emutest.WithConfig(func(c *config.Config) {
		c.SQLBind = []string{"127.0.0.1"}
		c.GFEBind = []string{"127.0.0.1"}
		c.Ports["gfe-lan"] = 0
	}))
	svc := adminClient(t, inst)
	ctx := context.Background()

	var ins []*sqladmin.DatabaseInstance
	for _, name := range []string{"lan-a", "lan-b"} {
		ins = append(ins, createInstance(t, svc, &sqladmin.DatabaseInstance{
			Name:            name,
			DatabaseVersion: "POSTGRES_17",
			RootPassword:    name + "-pw",
			Settings: &sqladmin.Settings{IpConfiguration: &sqladmin.IpConfiguration{
				Ipv4Enabled:        true,
				AuthorizedNetworks: []*sqladmin.AclEntry{{Value: "0.0.0.0/0"}},
			}},
		}))
	}

	// psql on each instance's own port: its password, and only its own.
	for _, in := range ins {
		host, port := lanAddr(t, inst, in)
		if host != "127.0.0.1" {
			t.Fatalf("%s served on %s, want the --sql-bind address", in.Name, host)
		}
		if _, err := connect(ctx, host, port, "postgres", "wrong", "postgres"); err == nil {
			t.Errorf("%s: wrong password admitted", in.Name)
		}
		if _, err := connect(ctx, host, port, "postgres", "", "postgres"); err == nil {
			t.Errorf("%s: no password admitted", in.Name)
		}
		c, err := connect(ctx, host, port, "postgres", in.Name+"-pw", "postgres")
		if err != nil {
			t.Fatalf("%s on %s:%d: %v", in.Name, host, port, err)
		}
		if _, err := c.Exec(ctx, "CREATE TABLE "+strings.ReplaceAll(in.Name, "-", "_")+"_marker ()"); err != nil {
			t.Fatal(err)
		}
		c.Close(ctx)
	}

	// On loopback the addresses are unchanged, and every instance has a
	// dnsName.
	for _, in := range ins {
		if publicIP(in) == "127.0.0.1" {
			t.Errorf("%s: loopback caller told the --sql-bind address", in.Name)
		}
		if in.DnsName == "" {
			t.Errorf("%s has no dnsName", in.Name)
		}
		cs, err := svc.Connect.Get(testProject, in.Name).Do()
		if err != nil {
			t.Fatal(err)
		}
		if cs.DnsName != in.DnsName || len(cs.IpAddresses) == 0 || cs.IpAddresses[0].IpAddress != publicIP(in) {
			t.Errorf("%s: loopback connectSettings %+v, dnsName %q", in.Name, cs.IpAddresses, cs.DnsName)
		}
	}

	// Through the LAN, with a token the Instance issued.
	tok := accessToken(t, inst, "user:dev@example.com")
	gfe := inst.Endpoint(frontend.LANEndpoint)
	lan := &http.Client{Timeout: 30 * time.Second, Transport: &oauth2.Transport{
		Source: oauth2.StaticTokenSource(tok),
		Base: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, gfe)
			},
			TLSClientConfig: &tls.Config{RootCAs: inst.Internal().Env.CA.Pool()},
		},
	}}
	lsvc, err := sqladmin.NewService(ctx, option.WithHTTPClient(lan), option.WithEndpoint("https://sqladmin.googleapis.com/"))
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range ins {
		got, err := lsvc.Instances.Get(testProject, in.Name).Do()
		if err != nil {
			t.Fatalf("get through the LAN: %v", err)
		}
		if len(got.IpAddresses) != 1 || got.IpAddresses[0].Type != "PRIMARY" || got.IpAddresses[0].IpAddress != "127.0.0.1" {
			t.Errorf("%s through the LAN: ipAddresses %+v, want PRIMARY 127.0.0.1", in.Name, got.IpAddresses)
		}
	}

	// The connector, its admin API calls through the LAN, dials
	// 127.0.0.1:3307 for both instances and reaches each.
	d, err := cloudsqlconn.NewDialer(ctx, cloudsqlconn.WithHTTPClient(lan), cloudsqlconn.WithTokenSource(oauth2.StaticTokenSource(tok)),
		cloudsqlconn.WithAdminAPIEndpoint("https://sqladmin.googleapis.com/"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, in := range ins {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		cfg, _ := pgx.ParseConfig("user=postgres dbname=postgres sslmode=disable")
		cfg.Password = in.Name + "-pw"
		cfg.DialFunc = func(ctx context.Context, _, _ string) (net.Conn, error) { return d.Dial(ctx, in.ConnectionName) }
		c, err := pgx.ConnectConfig(cctx, cfg)
		if err != nil {
			cancel()
			t.Fatalf("connector to %s through the LAN: %v", in.Name, err)
		}
		var mine bool
		err = c.QueryRow(cctx, "SELECT to_regclass($1) IS NOT NULL", strings.ReplaceAll(in.Name, "-", "_")+"_marker").Scan(&mine)
		c.Close(cctx)
		cancel()
		if err != nil || !mine {
			t.Errorf("connector to %s reached another instance (%v)", in.Name, err)
		}
	}
}
