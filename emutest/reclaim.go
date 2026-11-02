package emutest

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/instance"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// logFunc reports a sweep's failures, as testing.TB's Logf does.
type logFunc func(format string, args ...any)

// sweeper reclaims, once, the Instances of test processes that died.
type sweeper struct {
	once   sync.Once
	detect func() (*runtime.Client, error)
	// stderr gets each failure too, since a test's log shows only when it
	// fails or runs with -v (#914).
	stderr io.Writer
}

// processSweep runs before the first Instance this process starts.
var processSweep = &sweeper{detect: runtime.Detect, stderr: os.Stderr}

// run sweeps the first time it is called, if there is a runtime, logging
// failures with logf and writing them to stderr.
func (s *sweeper) run(logf logFunc) {
	s.once.Do(func() {
		cl, err := s.detect()
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		sweep(ctx, cl, func(format string, args ...any) {
			logf(format, args...)
			if s.stderr != nil {
				fmt.Fprintf(s.stderr, format+"\n", args...)
			}
		})
	})
}

// sweep reclaims the Instances of tests that have ended: those whose Test
// data dir holds their ID and has an owner lock file it can lock, and the
// Abandoned Instance IDs whose data dirs were all in the OS temp dir. It
// logs each failure with logf.
func sweep(ctx context.Context, cl *runtime.Client, logf logFunc) {
	tagged, err := instance.FindTagged(ctx, cl)
	if err != nil {
		logf("emutest: reclaiming the Instances of ended tests: %v", err)
		return
	}
	for _, a := range tagged {
		for _, dir := range a.Dirs {
			if _, err := instance.ReclaimEnded(ctx, cl, a, dir); err != nil {
				logf("emutest: reclaiming Instance %s of an ended test: %v", a.ID, err)
			}
		}
	}
	abandoned, err := instance.FindAbandoned(ctx, cl, config.StateHome())
	if err != nil {
		logf("emutest: reclaiming the Instances of ended tests: %v", err)
		return
	}
	for _, a := range abandoned {
		// Ended Test data dirs were reclaimed above, or still fail to be.
		if a.Unverified || a.EndedTest() || !allInTempDir(a.Dirs) {
			continue
		}
		if err := a.Prune(ctx, cl); err != nil {
			logf("emutest: reclaiming Abandoned Instance %s: %v", a.ID, err)
		}
	}
}

// allInTempDir reports whether dirs, which may no longer exist, are all
// inside the OS temp dir, symlinks resolved (on macOS /var is
// /private/var).
func allInTempDir(dirs []string) bool {
	tmp := resolveExisting(os.TempDir())
	for _, d := range dirs {
		rel, err := filepath.Rel(tmp, resolveExisting(d))
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return false
		}
	}
	return len(dirs) > 0
}

// resolveExisting returns the absolute path p with the symlinks of its
// longest existing ancestor resolved.
func resolveExisting(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	rest := ""
	for q := p; ; q = filepath.Dir(q) {
		if r, err := filepath.EvalSymlinks(q); err == nil {
			return filepath.Join(r, rest)
		}
		if filepath.Dir(q) == q {
			return p
		}
		rest = filepath.Join(filepath.Base(q), rest)
	}
}
