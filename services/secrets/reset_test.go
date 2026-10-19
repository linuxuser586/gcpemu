package secrets

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

const resetParent = "projects/test-proj/locations/us-central1"

// newResetService returns a started service whose Cloud SQL is sql.
func newResetService(t *testing.T, sql *fakeSQL) (*Service, *emu.Env, *clock.Offset) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	clk := clock.NewOffset(clock.Real{})
	env := &emu.Env{Config: &config.Config{}, Store: store.NewMemory(), Clock: clk, IDs: emu.NewIDs(true), Log: log,
		Auth: emu.NewPolicyAuthorizer(config.IAMOff, log), Endpoints: emu.NewEndpoints()}
	s := New(env).(*Service)
	env.SetServices(map[string]emu.Service{"secrets": s, "sql": sql})
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	return s, env, clk
}

// A sweep in flight across a Reset writes nothing afterwards: a managed
// rotation blocked setting the Cloud SQL password before the Reset adds
// no version to a secret of the same name created after it (#307).
func TestResetWaitsForSweep(t *testing.T) {
	sql := &fakeSQL{entered: make(chan struct{}), release: make(chan struct{})}
	s, env, clk := newResetService(t, sql)
	ctx := context.Background()

	sec, err := s.api.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: resetParent, SecretId: "db",
		Secret: &secretmanagerpb.Secret{SecretType: secretmanagerpb.Secret_CLOUD_SQL_DB_CREDENTIALS}})
	if err != nil {
		t.Fatal(err)
	}
	creds := &secretmanagerpb.EnableManagedRotationRequest_CloudSqlSingleUserCredentials{CloudSqlSingleUserCredentials: &secretmanagerpb.EnableManagedRotationRequest_CloudSQLSingleUserCredentials{InstanceId: "db", Username: "app"}}
	if _, err := s.api.EnableManagedRotation(ctx, &secretmanagerpb.EnableManagedRotationRequest{Parent: sec.GetName(), Credentials: creds}); err != nil {
		t.Fatal(err)
	}
	ref, err := s.parseSecretOnly(sec.GetName())
	if err != nil {
		t.Fatal(err)
	}
	// The rotation falls due; the background sweep claims it and blocks
	// in Cloud SQL.
	sql.armed = true
	err = env.Store.Update(func(tx store.Tx) error {
		rec, err := getSecret(tx, ref)
		if err != nil {
			return err
		}
		rec.pb.Rotation.NextRotationTime = timestamppb.New(clk.Now().Add(-time.Minute))
		rec.pb.Rotation.RotationPeriod = durationpb.New(24 * time.Hour)
		return putSecret(tx, ref, rec, clk.Now())
	})
	if err != nil {
		t.Fatal(err)
	}
	s.kick()
	<-sql.entered

	// An Instance Reset, then the same secret created again.
	recreated := make(chan error, 1)
	go func() {
		if err := s.Reset(ctx); err != nil {
			recreated <- err
			return
		}
		_ = env.Store.Reset()
		s.ResetDone()
		_, err := s.api.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: resetParent, SecretId: "db",
			Secret: &secretmanagerpb.Secret{}})
		recreated <- err
	}()
	select {
	case err := <-recreated:
		close(sql.release)
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(200 * time.Millisecond):
		close(sql.release) // Reset is waiting for the sweep
		if err := <-recreated; err != nil {
			t.Fatal(err)
		}
	}
	HoldSweep(s)() // the old sweep is over

	vs, err := s.api.ListSecretVersions(ctx, &secretmanagerpb.ListSecretVersionsRequest{Parent: sec.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if len(vs.GetVersions()) != 0 {
		t.Fatalf("the secret created after Reset has versions %v from a rotation started before it", vs.GetVersions())
	}
}

// After a Reset the sweep runs again: a secret created afterwards expires
// on time (#307).
func TestSweepRunsAfterReset(t *testing.T) {
	s, _, clk := newResetService(t, &fakeSQL{})
	ctx := context.Background()
	if err := s.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	s.ResetDone()
	sec, err := s.api.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: resetParent, SecretId: "short",
		Secret: &secretmanagerpb.Secret{Expiration: &secretmanagerpb.Secret_ExpireTime{ExpireTime: timestamppb.New(clk.Now().Add(time.Second))}}})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := s.api.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: sec.GetName()}); err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the sweep did not delete an expired secret created after Reset")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
