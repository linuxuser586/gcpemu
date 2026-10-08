package secrets_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/jackc/pgx/v5"
	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1beta4"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/linuxuser586/gcpemu/emutest"
)

// sqlOp waits for a Cloud SQL Operation.
func sqlOp(t *testing.T, svc *sqladmin.Service, op *sqladmin.Operation, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(3 * time.Minute); op.Status != "DONE"; {
		if time.Now().After(deadline) {
			t.Fatal("operation timed out")
		}
		time.Sleep(100 * time.Millisecond)
		if op, err = svc.Operations.Get(op.TargetProject, op.Name).Do(); err != nil {
			t.Fatal(err)
		}
	}
	if op.Error != nil {
		t.Fatalf("%s failed: %+v", op.OperationType, op.Error.Errors[0])
	}
}

// login reports whether user/password can log in to the instance.
func login(ctx context.Context, ip, user, password string) error {
	cfg, err := pgx.ParseConfig(fmt.Sprintf("host=%s port=5432 user=%s dbname=postgres sslmode=prefer connect_timeout=5", ip, user))
	if err != nil {
		return err
	}
	cfg.Password = password
	c, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return err
	}
	return c.Close(ctx)
}

// Managed rotation sets each new password on the Cloud SQL user and adds
// it as the secret's newest version, on demand and on schedule.
func TestManagedRotation(t *testing.T) {
	emutest.RequireRuntime(t)
	e := start(t, []string{"sql"})
	sql, err := sqladmin.NewService(e.ctx, option.WithEndpoint(e.inst.GatewayURL()+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	op, err := sql.Instances.Insert(testProject, &sqladmin.DatabaseInstance{Name: "db", Region: "us-central1", RootPassword: "rootpw",
		Settings: &sqladmin.Settings{IpConfiguration: &sqladmin.IpConfiguration{Ipv4Enabled: true, AuthorizedNetworks: []*sqladmin.AclEntry{{Value: "0.0.0.0/0"}}}}}).Do()
	sqlOp(t, sql, op, err)
	op, err = sql.Users.Insert(testProject, "db", &sqladmin.User{Name: "app", Password: "initial"}).Do()
	sqlOp(t, sql, op, err)
	in, err := sql.Instances.Get(testProject, "db").Do()
	if err != nil {
		t.Fatal(err)
	}
	ip := in.IpAddresses[0].IpAddress

	loc := parent + "/locations/us-central1"
	_, err = e.sm.CreateSecret(e.ctx, &secretmanagerpb.CreateSecretRequest{Parent: parent, SecretId: "g", Secret: &secretmanagerpb.Secret{Replication: automatic, SecretType: secretmanagerpb.Secret_CLOUD_SQL_DB_CREDENTIALS}})
	wantCode(t, "global typed secret", err, codes.InvalidArgument)
	other := must(e.sm.CreateSecret(e.ctx, &secretmanagerpb.CreateSecretRequest{Parent: parent + "/locations/us-east1", SecretId: "far", Secret: &secretmanagerpb.Secret{SecretType: secretmanagerpb.Secret_CLOUD_SQL_DB_CREDENTIALS}}))
	creds := &secretmanagerpb.EnableManagedRotationRequest_CloudSqlSingleUserCredentials{CloudSqlSingleUserCredentials: &secretmanagerpb.EnableManagedRotationRequest_CloudSQLSingleUserCredentials{InstanceId: "db", Username: "app", Password: "first-pw"}}
	_, err = e.sm.EnableManagedRotation(e.ctx, &secretmanagerpb.EnableManagedRotationRequest{Parent: other.GetName(), Credentials: creds})
	wantCode(t, "other region", err, codes.InvalidArgument)

	sec := must(e.sm.CreateSecret(e.ctx, &secretmanagerpb.CreateSecretRequest{Parent: loc, SecretId: "app-db", Secret: &secretmanagerpb.Secret{SecretType: secretmanagerpb.Secret_CLOUD_SQL_DB_CREDENTIALS}}))
	_, err = e.sm.RotateSecret(e.ctx, &secretmanagerpb.RotateSecretRequest{Parent: sec.GetName()})
	wantCode(t, "rotate before enable", err, codes.FailedPrecondition)
	_, err = e.sm.AddSecretVersion(e.ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: sec.GetName(), Payload: &secretmanagerpb.SecretPayload{Data: []byte("x")}})
	wantCode(t, "manual version", err, codes.FailedPrecondition)

	v1 := must(e.sm.EnableManagedRotation(e.ctx, &secretmanagerpb.EnableManagedRotationRequest{Parent: sec.GetName(), Credentials: creds}))
	if err := login(e.ctx, ip, "app", "first-pw"); err != nil {
		t.Fatalf("login with the given password: %v", err)
	}
	_, err = e.sm.EnableManagedRotation(e.ctx, &secretmanagerpb.EnableManagedRotationRequest{Parent: sec.GetName(), Credentials: creds})
	wantCode(t, "enable twice", err, codes.FailedPrecondition)

	v2 := must(e.sm.RotateSecret(e.ctx, &secretmanagerpb.RotateSecretRequest{Parent: sec.GetName()}))
	pw := string(must(e.sm.AccessSecretVersion(e.ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: v2.GetName()})).GetPayload().GetData())
	if v2.GetName() == v1.GetName() || pw == "first-pw" || len(pw) < 16 {
		t.Fatalf("rotated %s %q", v2.GetName(), pw)
	}
	if err := login(e.ctx, ip, "app", pw); err != nil {
		t.Fatalf("login with the rotated password: %v", err)
	}
	if login(e.ctx, ip, "app", "first-pw") == nil {
		t.Fatal("the old password still works")
	}
	got := must(e.sm.GetSecret(e.ctx, &secretmanagerpb.GetSecretRequest{Name: sec.GetName()}))
	if got.GetRotation().GetManagedRotationStatus().GetState() != secretmanagerpb.Rotation_ManagedRotationStatus_ACTIVE {
		t.Fatalf("status %v", got.GetRotation())
	}

	// A schedule rotates when it falls due on the Emulator clock.
	now := e.inst.Env.Clock.Now()
	must(e.sm.UpdateSecret(e.ctx, &secretmanagerpb.UpdateSecretRequest{
		Secret:     &secretmanagerpb.Secret{Name: sec.GetName(), Rotation: &secretmanagerpb.Rotation{NextRotationTime: timestamppb.New(now.Add(time.Hour)), RotationPeriod: durationpb.New(30 * 24 * time.Hour)}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"rotation"}},
	}))
	e.advance(2 * time.Hour)
	latest := must(e.sm.AccessSecretVersion(e.ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: sec.GetName() + "/versions/latest"}))
	if latest.GetName() == v2.GetName() {
		t.Fatal("no scheduled rotation")
	}
	if err := login(e.ctx, ip, "app", string(latest.GetPayload().GetData())); err != nil {
		t.Fatalf("login after the scheduled rotation: %v", err)
	}
}
