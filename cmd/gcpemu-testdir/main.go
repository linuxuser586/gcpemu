// Command gcpemu-testdir lets a test harness with no OS file lock of its
// own, such as the console's Playwright e2e, keep its Instances in Test
// data dirs (ADR 0076, #1035). It is a development tool, never released.
//
//	gcpemu-testdir hold <dir>
//
// takes the owner lock of the Test data dir dir, creating dir if need be,
// prints the line "held" once it has it, and keeps it until its stdin
// closes. The harness gives it a pipe for stdin, which closes when the
// harness ends however it ends, so the lock is held for exactly as long
// as the harness lives. It ignores SIGINT and SIGTERM, which reach it
// with the rest of its process group while the harness still stops its
// Instance.
//
//	gcpemu-testdir reclaim <root>
//
// is a run-start reclaim (ADR 0057): it reclaims every Test data dir
// whose test has ended, those directly under root among them, with their
// Instances' containers, networks and volumes, for up to reclaimTimeout;
// a later reclaim finishes what it did not. While the container runtime
// does not answer, it keeps the dirs under root that no object records,
// and fails.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/linuxuser586/gcpemu/internal/instance"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// heldLine is the line hold prints once it holds the lock.
const heldLine = "held"

// errUsage is a command line gcpemu-testdir does not take.
var errUsage = errors.New("usage: gcpemu-testdir hold <dir> | reclaim <root>")

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "gcpemu-testdir:", err)
		os.Exit(1)
	}
	// The tests run main from TestMain, which must not go on to the tests.
	os.Exit(0)
}

// run runs the command line args, hold reading stdin and writing stdout.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) != 2 {
		return errUsage
	}
	switch args[0] {
	case "hold":
		return hold(args[1], stdin, stdout)
	case "reclaim":
		return reclaim(args[1])
	}
	return errUsage
}

// hold takes dir's owner lock, writes heldLine to stdout, and holds the
// lock until stdin closes.
func hold(dir string, stdin io.Reader, stdout io.Writer) error {
	signal.Ignore(os.Interrupt, syscall.SIGTERM)
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	release, err := instance.HoldOwnerLock(dir)
	if err != nil {
		return err
	}
	defer release()
	if _, err := fmt.Fprintln(stdout, heldLine); err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, stdin)
	return nil
}

// reclaimTimeout bounds a reclaim, which may wait for each ended dir's
// Instance process to stop, so that it fits in the time Playwright gives
// its webServer to start.
const reclaimTimeout = 45 * time.Second

// reclaim reclaims the Test data dirs whose test has ended: every one an
// object on the runtime records, and every one directly under root.
func reclaim(root string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), reclaimTimeout)
	defer cancel()
	cl, err := runtime.DetectContext(ctx)
	if err != nil {
		cl, err = nil, fmt.Errorf("no container runtime, so the dirs under %s that no object records are kept: %w", root, err)
	}
	return errors.Join(err, instance.ReclaimEndedUnder(ctx, root, []*runtime.Client{cl}))
}
