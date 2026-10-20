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

// Instance is a running test emulator.
type Instance struct {
	in        *instance.Instance
	ownedDir  string // the temp data dir Close removes, "" with WithDataDir
	release   func() // drops the host loopback lock, nil when none is held
	closeOnce sync.Once
	closeErr  error
}

// Start launches an Instance running svcs (plus their dependencies) and
// stops it when the test ends. Set GCPEMU_TEST_LOG=1 to see emulator logs.
func Start(t testing.TB, svcs []string, opts ...Option) *Instance {
	t.Helper()
	cfg := testConfig(svcs, opts)
	var release func()
	if !cfg.NoSQLHostClients {
		var err error
		if release, err = holdHostLoopback(testDeadline(t)); err != nil {
			t.Fatalf("emutest: %v", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	i, err := launch(ctx, &cfg, release)
	if err != nil {
		t.Fatalf("emutest: %v", err)
	}
	t.Cleanup(func() {
		sctx, scancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer scancel()
		_ = i.Close(sctx)
	})
	return i
}

// New launches an Instance running svcs (plus their dependencies) that
// lives until Close, for sharing one Instance across a package's tests
// from TestMain. ctx bounds the start only. Unless WithDataDir is given,
// the Instance keeps its files in a temporary directory that Close
// removes. Set GCPEMU_TEST_LOG=1 to see emulator logs.
func New(ctx context.Context, svcs []string, opts ...Option) (*Instance, error) {
	cfg := testConfig(svcs, opts)
	var release func()
	if !cfg.NoSQLHostClients {
		deadline, _ := ctx.Deadline()
		var err error
		if release, err = holdHostLoopback(deadline); err != nil {
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
func launch(ctx context.Context, cfg *config.Config, release func()) (i *Instance, err error) {
	i = &Instance{release: release}
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
		_ = in.Shutdown(sctx)
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

// Close stops the Instance and removes its temporary data dir. Calling it
// again returns the first call's error.
func (i *Instance) Close(ctx context.Context) error {
	i.closeOnce.Do(func() {
		err := i.in.Shutdown(ctx)
		i.closeErr = errors.Join(err, i.cleanup())
	})
	return i.closeErr
}

// cleanup drops the host loopback lock and removes the temporary data dir.
func (i *Instance) cleanup() error {
	if i.release != nil {
		i.release()
		i.release = nil
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
