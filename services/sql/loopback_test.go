package sql_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/cloudsqlconn"
	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/loopback"
	"github.com/linuxuser586/gcpemu/internal/vmrelay"
	"github.com/linuxuser586/gcpemu/services/sql"
)

func publicInstance(name string) *sqladmin.DatabaseInstance {
	return &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "POSTGRES_18", RootPassword: "rootpw",
		Settings: &sqladmin.Settings{IpConfiguration: &sqladmin.IpConfiguration{Ipv4Enabled: true,
			AuthorizedNetworks: []*sqladmin.AclEntry{{Value: "127.0.0.1/32"}}}}}
}

// TestLoopbackPerInstance is ADR 0005 on a host with loopback aliases:
// through the VM relay, each instance reports its own 127.0.0.x as its
// PRIMARY address, and the Go connector (the Auth Proxy's dialer) and
// psql reach two instances at the same time there.
func TestLoopbackPerInstance(t *testing.T) {
	emutest.RequireRuntime(t)
	if !loopback.Aliases() {
		t.Skipf("no loopback aliases on this host (%s)", loopback.InstallHint)
	}
	t.Setenv(vmrelay.Env, "1")
	inst := emutest.Start(t, []string{"sql"})
	svc := adminClient(t, inst)
	ctx := context.Background()

	var mu sync.Mutex
	ins := map[string]*sqladmin.DatabaseInstance{}
	t.Run("create", func(t *testing.T) {
		for _, name := range []string{"loop-a", "loop-b"} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				in := createInstance(t, svc, publicInstance(name))
				mu.Lock()
				ins[name] = in
				mu.Unlock()
			})
		}
	})
	if t.Failed() {
		return
	}
	a, b := publicIP(ins["loop-a"]), publicIP(ins["loop-b"])
	for _, ip := range []string{a, b} {
		if p := net.ParseIP(ip); p == nil || !p.IsLoopback() || ip == "127.0.0.1" {
			t.Fatalf("PRIMARY %s, want a 127.0.0.x alias", ip)
		}
	}
	if a == b {
		t.Fatalf("both instances on %s", a)
	}
	cs := must(svc.Connect.Get(testProject, "loop-a").Do())
	if len(cs.IpAddresses) == 0 || cs.IpAddresses[0].IpAddress != a {
		t.Fatalf("connectSettings addresses %+v, want %s", cs.IpAddresses, a)
	}

	admin := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "owner", Expiry: time.Now().Add(time.Hour)})
	d, err := cloudsqlconn.NewDialer(ctx, cloudsqlconn.WithAdminAPIEndpoint(inst.GatewayURL()+"/"), cloudsqlconn.WithTokenSource(admin))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	t.Run("connect", func(t *testing.T) {
		for name, in := range ins {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				queryVia(t, d, in.ConnectionName, "postgres", "rootpw", "postgres")
				// Port 5432 on the same address, as from the host.
				c, err := connect(ctx, publicIP(in), 5432, "postgres", "rootpw", "postgres")
				if err != nil {
					t.Fatalf("%s:5432: %v", publicIP(in), err)
				}
				c.Close(ctx)
			})
		}
	})
}

// TestLoopbackWithoutAliases is ADR 0005 on a host without loopback
// aliases (macOS before `gcpemu loopback install`): one instance at a
// time holds 127.0.0.1, a second is refused with FAILED_PRECONDITION
// naming the fix, and the address passes on once the holder is deleted.
func TestLoopbackWithoutAliases(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Setenv(vmrelay.Env, "1")
	if l, err := net.Listen("tcp", "127.0.0.1:3307"); err != nil {
		t.Skipf("127.0.0.1:3307 is taken: %v", err)
	} else {
		l.Close()
	}
	inst := emutest.Start(t, []string{"sql"})
	svcMod, _ := inst.Env.Lookup("sql")
	sql.SetLoopbackAliases(svcMod, false)
	svc := adminClient(t, inst)

	first := createInstance(t, svc, publicInstance("first"))
	if ip := publicIP(first); ip != "127.0.0.1" {
		t.Fatalf("first instance PRIMARY %s, want 127.0.0.1", ip)
	}
	admin := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "owner", Expiry: time.Now().Add(time.Hour)})
	d, err := cloudsqlconn.NewDialer(context.Background(), cloudsqlconn.WithAdminAPIEndpoint(inst.GatewayURL()+"/"), cloudsqlconn.WithTokenSource(admin))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	queryVia(t, d, first.ConnectionName, "postgres", "rootpw", "postgres")

	_, err = svc.Instances.Insert(testProject, publicInstance("second")).Do()
	var ge *googleapi.Error
	if !errors.As(err, &ge) || ge.Code != 400 || !strings.Contains(ge.Body, `"FAILED_PRECONDITION"`) || !strings.Contains(ge.Message, loopback.InstallHint) {
		t.Fatalf("second public instance: %v, want FAILED_PRECONDITION naming %q", err, loopback.InstallHint)
	}
	// An instance that never runs serves no host clients; starting it
	// is refused like a create.
	stopped := publicInstance("stopped")
	stopped.Settings.ActivationPolicy = "NEVER"
	createInstance(t, svc, stopped)
	_, err = svc.Instances.Patch(testProject, "stopped", &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{ActivationPolicy: "ALWAYS"}}).Do()
	if !errors.As(err, &ge) || !strings.Contains(ge.Body, `"FAILED_PRECONDITION"`) {
		t.Fatalf("starting a second public instance: %v, want FAILED_PRECONDITION", err)
	}

	waitOp(t, svc, must(svc.Instances.Delete(testProject, "first").Do()))
	third := createInstance(t, svc, publicInstance("third"))
	if ip := publicIP(third); ip != "127.0.0.1" {
		t.Fatalf("third instance PRIMARY %s, want 127.0.0.1 after the holder's deletion", ip)
	}
	queryVia(t, d, third.ConnectionName, "postgres", "rootpw", "postgres")
}
