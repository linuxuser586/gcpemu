package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/linuxuser586/gcpemu/internal/config"
)

// ExitError makes main exit with Code and print nothing: `gcpemu run`'s
// command has already said why it failed.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// runCmd is `gcpemu run` (FR-CI-008): it hosts an Instance, as foreground
// `start` does, runs one command against it and tears it down.
func (o *rootOpts) runCmd() *cobra.Command {
	var trustCA, noUniverse bool
	cmd := &cobra.Command{
		Use:   "run [flags] -- COMMAND [ARGS...]",
		Short: "Start an Instance, run one command against it, then tear the Instance down",
		Long: `Start an Instance in this process, run COMMAND once it is ready with the
variables that gcpemu env prints added to its environment, then tear the
Instance down and exit with COMMAND's exit code. It takes every gcpemu start
flag, but the Instance is Ephemeral and logs at warn by default.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if cmd.ArgsLenAtDash() != 0 || len(args) == 0 {
				return errors.New("run takes its command after --: gcpemu run [flags] -- COMMAND [ARGS...]")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			base := config.Defaults()
			base.Ephemeral = true
			base.LogLevel = "warn"
			cfg, err := o.loadOver(cmd.Flags(), base)
			if err != nil {
				return err
			}
			// LookPath checks a path too, which exec.Command leaves to Start
			// on Unix.
			if _, err := exec.LookPath(args[0]); err != nil {
				return err
			}
			child := exec.Command(args[0], args[1:]...)
			if err := refuseRunning(cfg); err != nil {
				return err
			}
			return o.runCommand(cmd, cfg, child, trustCA, noUniverse)
		},
	}
	f := cmd.Flags()
	o.instanceFlags(f)
	f.Lookup("ephemeral").DefValue = "true"
	f.Lookup("log-level").DefValue = "warn"
	envFlags(f, &trustCA, &noUniverse)
	return cmd
}

// runCommand hosts cfg's Instance, runs child against it once it is ready
// and tears the Instance down after child exits. Ctrl+C and SIGTERM abort
// the startup, or once child runs, go to child, unless the terminal has
// already sent child the Ctrl+C.
func (o *rootOpts) runCommand(cmd *cobra.Command, cfg *config.Config, child *exec.Cmd, trustCA, noUniverse bool) error {
	stderr := cmd.ErrOrStderr()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var running *os.Process
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go func() {
		for sig := range sigs {
			mu.Lock()
			if running != nil {
				if sig != os.Interrupt || !groupGetsCtrlC() { // not twice
					_ = running.Signal(sig)
				}
			} else {
				cancel()
			}
			mu.Unlock()
		}
	}()

	in, err := o.host(ctx, cfg, stderr, true, true)
	if err != nil {
		return err
	}
	teardown := func() {
		sctx, scancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer scancel()
		if err := in.Shutdown(sctx); err != nil {
			fmt.Fprintf(stderr, "gcpemu: warning: tearing down instance %q: %v\n", cfg.Instance, err)
		}
	}
	vars, err := clientEnv(newAdminClient(in.Env.Endpoints.All(), in.AdminToken()), trustCA, noUniverse, stderr)
	if err != nil {
		teardown()
		return err
	}
	child.Env = os.Environ()
	for _, k := range sortedKeys(vars) {
		child.Env = append(child.Env, k+"="+vars[k]) // the last of a name wins
	}
	child.Stdin, child.Stdout, child.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), stderr

	mu.Lock()
	if err = ctx.Err(); err == nil {
		if err = child.Start(); err == nil {
			running = child.Process
		}
	}
	mu.Unlock()
	if err != nil {
		teardown()
		return err
	}
	err = child.Wait()
	teardown()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return &ExitError{Code: exitStatus(ee.ProcessState)}
	}
	return err
}
