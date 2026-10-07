package sql_test

import (
	"context"
	"net"
	"testing"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

const testProject = "sql-test-project"

// adminClient returns a sqladmin v1beta4 client pointed at the gateway.
func adminClient(t testing.TB, inst *emutest.Instance) *sqladmin.Service {
	t.Helper()
	svc, err := sqladmin.NewService(context.Background(),
		option.WithEndpoint(inst.GatewayURL()+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// waitOp polls an operation until DONE and fails the test on an error.
func waitOp(t testing.TB, svc *sqladmin.Service, op *sqladmin.Operation) *sqladmin.Operation {
	t.Helper()
	op, err := waitOpErr(svc, op)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

type opFailed struct{ op *sqladmin.Operation }

func (e *opFailed) Error() string {
	if e.op.Error != nil && len(e.op.Error.Errors) > 0 {
		return e.op.OperationType + " failed: " + e.op.Error.Errors[0].Code + ": " + e.op.Error.Errors[0].Message
	}
	return e.op.OperationType + " failed"
}

func waitOpErr(svc *sqladmin.Service, op *sqladmin.Operation) (*sqladmin.Operation, error) {
	deadline := time.Now().Add(3 * time.Minute)
	for op.Status != "DONE" {
		if time.Now().After(deadline) {
			return op, context.DeadlineExceeded
		}
		time.Sleep(100 * time.Millisecond)
		var err error
		if op, err = svc.Operations.Get(op.TargetProject, op.Name).Do(); err != nil {
			return op, err
		}
	}
	if op.Error != nil {
		return op, &opFailed{op}
	}
	return op, nil
}

// createInstance inserts an instance and waits for RUNNABLE.
func createInstance(t testing.TB, svc *sqladmin.Service, in *sqladmin.DatabaseInstance) *sqladmin.DatabaseInstance {
	t.Helper()
	op, err := svc.Instances.Insert(testProject, in).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitOp(t, svc, op)
	got, err := svc.Instances.Get(testProject, in.Name).Do()
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "RUNNABLE" {
		t.Fatalf("state = %s, want RUNNABLE", got.State)
	}
	return got
}

// assertNoRuntimeObjects fails if containers or volumes of resource remain.
func assertNoRuntimeObjects(t testing.TB, inst *emutest.Instance, resource string) {
	t.Helper()
	ctx := context.Background()
	rt, err := inst.Env.Containers.Runtime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sel := map[string]string{runtime.LabelInstance: rt.InstanceID, runtime.LabelResource: resource}
	cs, err := rt.ListContainers(ctx, sel)
	if err != nil {
		t.Fatal(err)
	}
	vols, err := rt.ListVolumes(ctx, sel)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) > 0 || len(vols) > 0 {
		t.Fatalf("leftover runtime objects for %s: %d containers, volumes %v", resource, len(cs), vols)
	}
}

func cidrHas(cidr, ip string) bool {
	_, n, err := net.ParseCIDR(cidr)
	return err == nil && n.Contains(net.ParseIP(ip))
}

// staticTS is a token source for a fixed access token.
func staticTS(tok string) oauth2.TokenSource {
	return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: tok, TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)})
}
