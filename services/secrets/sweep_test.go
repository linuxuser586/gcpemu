package secrets

import (
	"context"
	"io"
	"log/slog"
	"runtime"
	"strings"
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

// fakeSQL is a sql Service whose SetUserPassword, once armed, signals
// entered and blocks until release is closed.
type fakeSQL struct {
	armed   bool
	entered chan struct{}
	release chan struct{}
}

func (f *fakeSQL) Name() string                { return "sql" }
func (f *fakeSQL) Register(emu.Router) error   { return nil }
func (f *fakeSQL) Start(context.Context) error { return nil }
func (f *fakeSQL) Stop(context.Context) error  { return nil }
func (f *fakeSQL) Ready() error                { return nil }

func (f *fakeSQL) InstanceRegion(project, instance string) (string, error) {
	return "us-central1", nil
}

func (f *fakeSQL) SetUserPassword(ctx context.Context, project, instance, user, password string) error {
	if f.armed {
		close(f.entered)
		<-f.release
	}
	return nil
}

// ClockAdvanced waits for a sweep already running, so a rotation that
// sweep claimed is applied by the time it returns.
func TestClockAdvancedWaitsForRunningSweep(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	clk := clock.NewOffset(clock.Real{})
	env := &emu.Env{Config: &config.Config{}, Store: store.NewMemory(), Clock: clk, IDs: emu.NewIDs(true), Log: log,
		Auth: emu.NewPolicyAuthorizer(config.IAMOff, log), Endpoints: emu.NewEndpoints()}
	s := New(env).(*Service)
	sql := &fakeSQL{entered: make(chan struct{}), release: make(chan struct{})}
	env.SetServices(map[string]emu.Service{"secrets": s, "sql": sql})
	ctx := context.Background()

	sec, err := s.api.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: "projects/test-proj/locations/us-central1", SecretId: "db",
		Secret: &secretmanagerpb.Secret{SecretType: secretmanagerpb.Secret_CLOUD_SQL_DB_CREDENTIALS}})
	if err != nil {
		t.Fatal(err)
	}
	creds := &secretmanagerpb.EnableManagedRotationRequest_CloudSqlSingleUserCredentials{CloudSqlSingleUserCredentials: &secretmanagerpb.EnableManagedRotationRequest_CloudSQLSingleUserCredentials{InstanceId: "db", Username: "app"}}
	v1, err := s.api.EnableManagedRotation(ctx, &secretmanagerpb.EnableManagedRotationRequest{Parent: sec.GetName(), Credentials: creds})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := s.parseSecretOnly(sec.GetName())
	if err != nil {
		t.Fatal(err)
	}
	err = env.Store.Update(func(tx store.Tx) error {
		rec, err := getSecret(tx, ref)
		if err != nil {
			return err
		}
		rec.pb.Rotation.NextRotationTime = timestamppb.New(clk.Now().Add(time.Hour))
		rec.pb.Rotation.RotationPeriod = durationpb.New(24 * time.Hour)
		return putSecret(tx, ref, rec, clk.Now())
	})
	if err != nil {
		t.Fatal(err)
	}

	// The background sweep claims the due rotation and blocks setting the
	// Cloud SQL password.
	sql.armed = true
	clk.Advance(2 * time.Hour)
	go s.sweep(ctx)
	<-sql.entered

	// ClockAdvanced must not return while that rotation is unfinished: the
	// fake is released only once ClockAdvanced is seen waiting on the
	// sweep lock, and the rotation must be applied when it returns.
	latest := make(chan string, 1)
	go func() {
		if err := s.ClockAdvanced(ctx); err != nil {
			t.Error(err)
		}
		v, err := s.api.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: sec.GetName() + "/versions/latest"})
		if err != nil {
			t.Error(err)
		}
		latest <- v.GetName()
	}()
	for !waitingOnLock("ClockAdvanced") {
		select {
		case <-latest:
			close(sql.release)
			t.Fatal("ClockAdvanced returned while a running sweep's rotation was unfinished")
		default:
			runtime.Gosched()
		}
	}
	close(sql.release)
	if got := <-latest; got == v1.GetName() {
		t.Fatalf("ClockAdvanced returned before the scheduled rotation: latest is still %s", got)
	}
}

// waitingOnLock reports whether a goroutine running fn is blocked
// acquiring a sync.Mutex.
func waitingOnLock(fn string) bool {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	for _, g := range strings.Split(string(buf), "\n\n") {
		header, _, _ := strings.Cut(g, "\n")
		if strings.Contains(header, "[sync.Mutex.Lock") && strings.Contains(g, "(*Service)."+fn+"(") {
			return true
		}
	}
	return false
}
