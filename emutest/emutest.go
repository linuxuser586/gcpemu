// Package emutest starts an in-process gcpemu instance for Go tests
// (FR-CI-009). Every port is allocated dynamically and state is ephemeral.
package emutest

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/instance"
	"github.com/linuxuser586/gcpemu/services"
)

// Instance is a running test emulator.
type Instance struct {
	*instance.Instance
}

// Option customises the configuration before start.
type Option func(*config.Config)

// WithIAMMode sets the IAM enforcement mode.
func WithIAMMode(mode string) Option { return func(c *config.Config) { c.IAMMode = mode } }

// WithDeterministic enables deterministic IDs and the fake clock.
func WithDeterministic() Option { return func(c *config.Config) { c.Deterministic = true } }

// Start launches an instance running services (plus dependencies) and stops
// it when the test ends. Set GCPEMU_TEST_LOG=1 to see emulator logs.
func Start(t testing.TB, svcs []string, opts ...Option) *Instance {
	t.Helper()
	cfg := config.Defaults()
	cfg.Ephemeral = true
	cfg.DataDir = t.TempDir()
	cfg.Services = svcs
	cfg.LogLevel = "warn"
	cfg.Ports[config.AllPorts] = 0
	for _, o := range opts {
		o(&cfg)
	}
	var logOut io.Writer = io.Discard
	if os.Getenv("GCPEMU_TEST_LOG") == "1" {
		logOut = os.Stderr
		cfg.LogLevel = "debug"
	}
	in, err := instance.New(&cfg, services.Factories(), logOut)
	if err != nil {
		t.Fatalf("emutest: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := in.Start(ctx); err != nil {
		t.Fatalf("emutest: start: %v", err)
	}
	t.Cleanup(func() {
		sctx, scancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer scancel()
		_ = in.Shutdown(sctx)
	})
	if err := in.WaitReady(ctx); err != nil {
		t.Fatalf("emutest: %v", err)
	}
	return &Instance{in}
}

// Endpoint returns the address (host:port) of a named listener, e.g.
// "gateway", "gcs", "pubsub".
func (i *Instance) Endpoint(name string) string { return i.Env.Endpoints.Get(name) }

// GatewayURL returns "http://<gateway>".
func (i *Instance) GatewayURL() string { return "http://" + i.Endpoint("gateway") }

// Setenv sets every client environment variable the instance advertises
// for the duration of the test.
func (i *Instance) Setenv(t testing.TB) {
	for k, v := range i.EnvVars() {
		t.Setenv(k, v)
	}
}
