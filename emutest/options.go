package emutest

import (
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/config"
)

// Option customises an Instance before it starts.
type Option struct {
	apply func(*config.Config)
}

// WithIAMMode sets the IAM enforcement level: "off", "audit" (the
// default) or "enforce".
func WithIAMMode(mode string) Option { return WithConfig(func(c *config.Config) { c.IAMMode = mode }) }

// WithDeterministic enables deterministic IDs and the fake clock.
func WithDeterministic() Option {
	return WithConfig(func(c *config.Config) { c.Deterministic = true })
}

// WithConsole serves the Web console under /console, as an Instance does
// by default.
func WithConsole() Option {
	return WithConfig(func(c *config.Config) { on := true; c.Console = &on })
}

// WithSQLHostClients has Cloud SQL instances with a public IP serve host
// clients on a loopback address on a VM runtime (ADR 0005), as gcpemu
// does, for tests that dial the address an instance reports (the Cloud
// SQL connector, pgx or psql to its PRIMARY IP). Without it they serve
// none, and the host reaches each instance only on its published port
// (GCPEMU_SQL_<INSTANCE>). On a host without loopback aliases every
// instance would serve on 127.0.0.1, so the Instance then holds a lock
// shared by the user's test binaries for as long as it runs, and their
// instances must serve one at a time.
func WithSQLHostClients() Option {
	return WithConfig(func(c *config.Config) { c.NoSQLHostClients = false })
}

// WithSeed applies the Seed file at path before the Instance reports
// ready, as `gcpemu start --seed` does.
func WithSeed(path string) Option { return WithConfig(func(c *config.Config) { c.Seed = path }) }

// WithDataDir runs a Persistent Instance in dir instead of an Ephemeral
// one: its state is kept in dir, and an Instance started later in the same
// dir finds it there. The Instance never removes dir, and its networks and
// volumes outlive the test until `gcpemu prune` removes them. For a dir
// the test owns (a Test data dir), use WithTestDataDir.
func WithDataDir(dir string) Option {
	return WithConfig(func(c *config.Config) { c.Ephemeral, c.DataDir = false, dir })
}

// WithTestDataDir runs a Persistent Instance in dir, as WithDataDir does,
// for a Test data dir that t owns, such as t.TempDir(). When t ends, after
// its Instances have stopped, the containers, networks and volumes of the
// Instance in dir are removed, as an Ephemeral Instance's are when it
// stops; Instances started again in dir during t still find them. A
// failure to remove them fails t.
func WithTestDataDir(t testing.TB, dir string) Option {
	persistent := WithDataDir(dir)
	return WithConfig(func(c *config.Config) {
		persistent.apply(c)
		removeAtEnd(t, dir)
	})
}

// WithOperationLatency makes the Operations of the Service serviceID
// ("compute", "gke", "sql", …, or "*" for every Service) complete after d
// instead of at once.
func WithOperationLatency(serviceID string, d time.Duration) Option {
	return WithConfig(func(c *config.Config) { c.LROLatency[serviceID] = d.String() })
}

// WithStrictProjects refuses requests for a project that was not declared,
// in a Seed file or by creating it, instead of creating it on first use.
func WithStrictProjects() Option {
	return WithConfig(func(c *config.Config) { c.StrictProjects = true })
}

// WithOffline never reaches the internet: images are not pulled and
// nothing is read through from Google.
func WithOffline() Option { return WithConfig(func(c *config.Config) { c.Offline = true }) }

// WithExternal delegates the API named api ("firestore", "spanner", …),
// which no Service serves, to the External emulator at addr (host:port),
// as `gcpemu start --external api=addr` does. An addr of "managed" has the
// Instance run Google's emulator for firestore, datastore, spanner or
// bigtable itself, which needs a container runtime (see RequireRuntime).
func WithExternal(api, addr string) Option {
	return WithConfig(func(c *config.Config) {
		if c.External == nil {
			c.External = map[string]string{}
		}
		c.External[api] = addr
	})
}

// WithConfig edits the Instance's configuration directly. It is for
// gcpemu's own tests only: config.Config is internal to gcpemu, and the
// knobs it reaches carry no compatibility promise.
func WithConfig(f func(*config.Config)) Option { return Option{apply: f} }
