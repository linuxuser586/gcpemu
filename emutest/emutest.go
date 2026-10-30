// Package emutest starts an in-process gcpemu instance for Go tests
// (FR-CI-009). Every port is allocated dynamically and state is ephemeral
// unless WithDataDir says otherwise.
//
// Start runs an Instance for one test; New and Close run one for a whole
// package, from TestMain. An Instance hands out client options for any
// Google client library (GRPCOptions, HTTPOptions) and ready-made Cloud
// Storage and Pub/Sub clients (Storage, PubSub).
package emutest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/instance"
	"github.com/linuxuser586/gcpemu/services"
)

// shutdownInstance and removeInstanceResources are
// (*instance.Instance).Shutdown and removeResources, replaced by tests.
var (
	shutdownInstance        = func(ctx context.Context, in *instance.Instance) error { return in.Shutdown(ctx) }
	removeInstanceResources = removeResources
)

// Instance is a running test emulator.
type Instance struct {
	in        *instance.Instance
	ownedDir  string // the temp data dir Close removes, "" with WithDataDir
	dropOwner func() // drops ownedDir's owner lock, nil when none is held
	release   func() // drops the host loopback lock, nil when none is held
	closeOnce sync.Once
	closeErr  error
	removeErr error // removing ownedDir, part of closeErr
}

// Start launches an Instance running svcs (plus their dependencies) and
// stops it when the test ends. Set GCPEMU_TEST_LOG=1 to see emulator logs.
func Start(t testing.TB, svcs []string, opts ...Option) *Instance {
	t.Helper()
	i, err := start(context.Background(), 60*time.Second, svcs, opts, testDeadline(t), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sctx, scancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer scancel()
		// Shutdown errors are ignored, but a data dir left behind is
		// reported, as t.TempDir does.
		_ = i.Close(sctx)
		if i.removeErr != nil {
			t.Errorf("emutest: %v", i.removeErr)
		}
	})
	return i
}

// New launches an Instance running svcs (plus their dependencies) that
// lives until Close, for sharing one Instance across a package's tests
// from TestMain. ctx bounds the start only. Unless WithDataDir is given,
// the Instance keeps its files in a temporary directory that Close
// removes. Set GCPEMU_TEST_LOG=1 to see emulator logs.
func New(ctx context.Context, svcs []string, opts ...Option) (*Instance, error) {
	deadline, _ := ctx.Deadline()
	return start(ctx, 0, svcs, opts, deadline, func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	})
}

// start launches an Instance running svcs with opts within timeout (zero:
// only ctx bounds it), waiting for the host loopback lock, when it needs
// one, until lockDeadline (zero: forever). The first Instance of the
// process first reclaims those of tests that have ended (ADR 0047),
// outside timeout, logging failures with logf.
func start(ctx context.Context, timeout time.Duration, svcs []string, opts []Option, lockDeadline time.Time, logf logFunc) (*Instance, error) {
	cfg := testConfig(svcs, opts)
	processSweep.run(logf)
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var release func()
	if !cfg.NoSQLHostClients {
		var err error
		if release, err = holdHostLoopback(lockDeadline); err != nil {
			return nil, fmt.Errorf("emutest: %w", err)
		}
	}
	i, err := launch(ctx, &cfg, release)
	if err != nil {
		return nil, fmt.Errorf("emutest: %w", err)
	}
	return i, nil
}

// testConfig is the configuration of a test Instance running svcs with
// opts applied.
func testConfig(svcs []string, opts []Option) config.Config {
	cfg := config.Defaults()
	cfg.Ephemeral = true
	cfg.Services = svcs
	cfg.LogLevel = "warn"
	cfg.Ports[config.AllPorts] = 0
	cfg.NoSQLHostClients = true
	cfg.NoRegistryRelay = true
	for _, o := range opts {
		if o.apply != nil {
			o.apply(&cfg)
		}
	}
	return cfg
}

// launch starts an Instance from cfg and waits until it is ready; release
// is called once it stops. With no data dir in cfg, it runs in a temporary
// one it removes then.
func launch(ctx context.Context, cfg *config.Config, release func()) (_ *Instance, err error) {
	// Not the named result: a failed launch returns nil, but must still
	// clean up the Instance it built (#863).
	i := &Instance{release: release}
	defer func() {
		if err != nil {
			i.cleanup()
		}
	}()
	if cfg.DataDir == "" {
		if cfg.DataDir, err = os.MkdirTemp("", "gcpemu-test-"); err != nil {
			return nil, err
		}
		i.ownedDir = cfg.DataDir
		if i.dropOwner, err = holdOwnerLock(cfg.DataDir); err != nil {
			return nil, err
		}
	}
	var logOut io.Writer = io.Discard
	if os.Getenv("GCPEMU_TEST_LOG") == "1" {
		logOut = os.Stderr
		cfg.LogLevel = "debug"
	}
	in, err := instance.New(cfg, services.Factories(), logOut)
	if err != nil {
		return nil, err
	}
	stop := func() {
		sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = i.shutdown(sctx, in)
	}
	// Not ready until seeded, as with `gcpemu start --seed`.
	seeded := func() {}
	if cfg.Seed != "" {
		seeded = in.HoldReady("seed", "applying "+cfg.Seed)
	}
	if err := in.Start(ctx); err != nil {
		stop()
		return nil, fmt.Errorf("start: %w", err)
	}
	if cfg.Seed != "" {
		if err := in.ApplySeed(ctx, cfg.Seed); err != nil {
			stop()
			return nil, fmt.Errorf("seed: %w", err)
		}
	}
	seeded()
	if err := in.WaitReady(ctx); err != nil {
		stop()
		return nil, err
	}
	i.in = in
	return i, nil
}

// Close stops the Instance and removes its temporary data dir, unless its
// containers could not be removed; the next test process reclaims them
// then. Calling it again returns the first call's error.
func (i *Instance) Close(ctx context.Context) error {
	i.closeOnce.Do(func() {
		err := i.shutdown(ctx, i.in)
		i.removeErr = i.cleanup()
		i.closeErr = errors.Join(err, i.removeErr)
	})
	return i.closeErr
}

// shutdown stops in, i's emulator. If that fails and the containers,
// networks or volumes it leaves cannot be removed, i gives up its Test
// data dir, so cleanup leaves it on disk: the dir still holds the
// Instance ID, and the next sweep removes what is left (ADR 0047). Close
// and a failed launch both stop this way.
func (i *Instance) shutdown(ctx context.Context, in *instance.Instance) error {
	err := shutdownInstance(ctx, in)
	if err != nil && i.ownedDir != "" && removeInstanceResources(i.ownedDir) != nil {
		i.ownedDir = ""
	}
	return err
}

// cleanup drops the host loopback lock and the owner lock, then removes
// the Test data dir, if i still owns one: shutdown gives up the dir
// of an Instance whose containers, networks or volumes remain, and with
// WithDataDir i owns none.
func (i *Instance) cleanup() error {
	if i.release != nil {
		i.release()
		i.release = nil
	}
	if i.dropOwner != nil {
		i.dropOwner()
		i.dropOwner = nil
	}
	if i.ownedDir == "" {
		return nil
	}
	return os.RemoveAll(i.ownedDir)
}

// Internal returns the running emulator. It is for gcpemu's own tests
// only: instance.Instance is internal to gcpemu and carries no
// compatibility promise.
func (i *Instance) Internal() *instance.Instance { return i.in }

// Endpoint returns the address (host:port) of a named listener, e.g.
// "gateway", "gcs", "pubsub".
func (i *Instance) Endpoint(name string) string { return i.in.Env.Endpoints.Get(name) }

// GatewayURL returns "http://<gateway>".
func (i *Instance) GatewayURL() string { return "http://" + i.Endpoint("gateway") }

// EnvVars returns every client environment variable the Instance
// advertises (`gcpemu env`), such as STORAGE_EMULATOR_HOST and
// PUBSUB_EMULATOR_HOST.
func (i *Instance) EnvVars() map[string]string { return i.in.EnvVars() }

// Setenv sets every client environment variable the Instance advertises
// for the duration of the test.
func (i *Instance) Setenv(t testing.TB) {
	for k, v := range i.EnvVars() {
		t.Setenv(k, v)
	}
}
