// Package cli implements the gcpemu command line (FR-CORE-001..007).
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/instance"
)

// flagFields maps flag names to Config field names for precedence handling.
var flagFields = map[string]string{
	"instance":                      "Instance",
	"data-dir":                      "DataDir",
	"ephemeral":                     "Ephemeral",
	"services":                      "Services",
	"bind":                          "Bind",
	"i-understand-this-is-insecure": "Insecure",
	"port":                          "Ports",
	"port-range":                    "PortRange",
	"iam-mode":                      "IAMMode",
	"default-principal":             "DefaultPrincipal",
	"log-format":                    "LogFormat",
	"log-level":                     "LogLevel",
	"lro-latency":                   "LROLatency",
	"deterministic":                 "Deterministic",
	"strict-projects":               "StrictProjects",
	"seed":                          "Seed",
	"wait-timeout":                  "WaitTimeout",
	"dns-no-forward":                "DNSNoForward",
	"offline":                       "Offline",
	"host-mode":                     "HostMode",
	"console":                       "Console",
}

type rootOpts struct {
	flags      config.Config
	console    bool
	ports      []string
	lro        map[string]string
	configFile string
	factories  map[string]instance.Factory
}

// New returns the root command. factories lists the services compiled in.
func New(factories map[string]instance.Factory) *cobra.Command {
	o := &rootOpts{factories: factories}
	root := &cobra.Command{
		Use:           "gcpemu",
		Short:         "Local emulator for Google Cloud services",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	pf := root.PersistentFlags()
	pf.StringVar(&o.flags.Instance, "instance", "default", "instance name")
	pf.StringVar(&o.flags.DataDir, "data-dir", "", "instance data directory (default $XDG_STATE_HOME/gcpemu/<instance>)")
	pf.StringVar(&o.configFile, "config", "gcpemu.yaml", "config file")

	root.AddCommand(o.startCmd(), o.stopCmd(), o.statusCmd(), o.resetCmd(), o.logsCmd(),
		o.envCmd(), o.versionCmd(), o.timeCmd(), o.faultCmd(), o.tofuProviderCmd(), o.doctorCmd(),
		o.cdnCmd(), o.consoleCmd())
	root.AddCommand(o.hostsCmd())
	root.AddCommand(o.caCmd())
	root.AddCommand(o.adminCmd())
	return root
}

// load builds the effective config: defaults < file < env < flags.
func (o *rootOpts) load(fs *pflag.FlagSet) (*config.Config, error) {
	c := config.Defaults()
	if err := c.LoadFile(o.configFile); err != nil {
		return nil, err
	}
	if err := c.LoadEnv(os.Getenv); err != nil {
		return nil, err
	}
	changed := map[string]bool{}
	fs.Visit(func(f *pflag.Flag) {
		if field, ok := flagFields[f.Name]; ok {
			changed[field] = true
		}
	})
	if len(o.ports) > 0 {
		ports, err := config.ParsePorts(strings.Join(o.ports, ","))
		if err != nil {
			return nil, fmt.Errorf("--port: %w", err)
		}
		o.flags.Ports = ports
	}
	o.flags.LROLatency = o.lro
	if fs.Changed("console") {
		o.flags.Console = &o.console
	}
	c.ApplyFlags(&o.flags, changed)
	return &c, nil
}

func (o *rootOpts) startCmd() *cobra.Command {
	var detach bool
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the emulator",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := o.load(cmd.Flags())
			if err != nil {
				return err
			}
			if detach {
				return startDetached(cmd, cfg)
			}
			return o.runForeground(cfg)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&detach, "detach", false, "run in the background and return once ready")
	f.BoolVar(&o.flags.Ephemeral, "ephemeral", false, "keep state in memory and data in a temp dir removed on exit")
	f.StringSliceVar(&o.flags.Services, "services", nil, "services to run (default all): "+strings.Join(config.AllServices, ","))
	f.StringVar(&o.flags.Bind, "bind", "127.0.0.1", "address to bind listeners to")
	f.BoolVar(&o.flags.Insecure, "i-understand-this-is-insecure", false, "allow non-loopback bind with IAM off")
	f.StringSliceVar(&o.ports, "port", nil, "port overrides, e.g. gateway=0,gcs=4443; a bare 0 makes every listener pick a free port")
	f.StringVar(&o.flags.PortRange, "port-range", "", "LO-HI range that free ports are picked from, e.g. 20000-20999")
	f.StringVar(&o.flags.IAMMode, "iam-mode", config.IAMAudit, "IAM enforcement: off, audit, enforce")
	f.StringVar(&o.flags.DefaultPrincipal, "default-principal", "user:dev@example.com", "principal for unauthenticated requests")
	f.StringVar(&o.flags.LogFormat, "log-format", "text", "log format: text or json")
	f.StringVar(&o.flags.LogLevel, "log-level", "info", "log level")
	f.StringToStringVar(&o.lro, "lro-latency", nil, "LRO latency per service, e.g. container=20s (default instant)")
	f.BoolVar(&o.flags.Deterministic, "deterministic", false, "deterministic IDs and fake clock")
	f.BoolVar(&o.flags.StrictProjects, "strict-projects", false, "require projects to be declared")
	f.StringVar(&o.flags.Seed, "seed", "", "seed file to apply before reporting ready")
	f.DurationVar(&o.flags.WaitTimeout, "wait-timeout", 120*time.Second, "readiness timeout for --detach and CI")
	f.BoolVar(&o.flags.DNSNoForward, "dns-no-forward", false, "do not forward unknown DNS names to the host resolver")
	f.BoolVar(&o.flags.Offline, "offline", false, "never reach the internet")
	f.BoolVar(&o.console, "console", true, "serve the Web console under /console on the gateway (default false when CI=true)")
	f.BoolVar(&o.flags.HostMode, "host-mode", false, "serve real Google hostnames to host processes via a frontend container and its DNS (see `gcpemu env`, `gcpemu hosts`)")
	return cmd
}

func (o *rootOpts) runForeground(cfg *config.Config) error {
	logOut := io.Writer(os.Stderr)
	if os.Getenv("GCPEMU_DETACHED") == "1" {
		logOut = os.Stdout // redirected to the log file by the parent
	}
	in, err := instance.New(cfg, o.factories, logOut)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Not ready until seeded: `start --detach` and CI poll /_emu/v1/ready.
	seeded := func() {}
	if cfg.Seed != "" {
		seeded = in.HoldReady("seed", "applying "+cfg.Seed)
	}
	if err := in.Start(ctx); err != nil {
		_ = in.Shutdown(context.Background())
		return err
	}
	if cfg.Seed != "" {
		if err := in.ApplySeed(ctx, cfg.Seed); err != nil {
			_ = in.Shutdown(context.Background())
			return fmt.Errorf("seed: %w", err)
		}
	}
	seeded()
	wctx, cancel := context.WithTimeout(ctx, cfg.WaitTimeout)
	err = in.WaitReady(wctx)
	cancel()
	if err != nil {
		in.Env.Log.Error("not ready", "err", err)
		if os.Getenv("CI") == "true" { // FR-CI-003: fail fast
			_ = in.Shutdown(context.Background())
			return err
		}
	} else {
		in.Env.Log.Info("ready")
	}
	select {
	case <-ctx.Done():
	case <-in.Done():
	}
	sctx, scancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer scancel()
	return in.Shutdown(sctx)
}

func startDetached(cmd *cobra.Command, cfg *config.Config) error {
	dir := cfg.InstanceDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if c, err := clientFor(cfg); err == nil {
		if _, err := c.get("/_emu/v1/health"); err == nil {
			return fmt.Errorf("instance %q is already running (%s)", cfg.Instance, dir)
		}
	}
	_ = os.Remove(filepath.Join(dir, instance.EndpointsFile))
	logf, err := os.OpenFile(filepath.Join(dir, instance.LogFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	var logStart int64
	if fi, err := logf.Stat(); err == nil {
		logStart = fi.Size()
	}
	exe, err := detachExe()
	if err != nil {
		return err
	}
	var args []string
	for _, a := range os.Args[1:] {
		if a != "--detach" && a != "--detach=true" {
			args = append(args, a)
		}
	}
	child := exec.Command(exe, args...)
	child.Stdout, child.Stderr = logf, logf
	child.Env = append(os.Environ(), "GCPEMU_DETACHED=1")
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := child.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()

	logPath := filepath.Join(dir, instance.LogFile)
	// The child gives up after the same wait timeout (and exits at once in
	// CI, FR-CI-003); the grace lets its own reason reach the user.
	deadline := time.Now().Add(cfg.WaitTimeout + 5*time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			return fmt.Errorf("emulator exited during startup (%v):\n%s\nfull log: %s", err, logTail(logPath, logStart, 20), logPath)
		case <-time.After(100 * time.Millisecond):
		}
		c, err := clientFor(cfg)
		if err != nil {
			continue
		}
		if code, _, err := c.do(http.MethodGet, "/_emu/v1/ready"); err == nil && code == http.StatusOK {
			fmt.Fprintf(cmd.OutOrStdout(), "gcpemu instance %q ready (pid %d, gateway %s)\n", cfg.Instance, child.Process.Pid, c.base)
			_ = child.Process.Release()
			return nil
		}
	}
	reasons := notReady(cfg)
	_ = child.Process.Signal(syscall.SIGTERM)
	if reasons == "" {
		reasons = logTail(logPath, logStart, 20)
	}
	return fmt.Errorf("emulator not ready after %s:\n%s\nfull log: %s", cfg.WaitTimeout, reasons, logPath)
}

// detachExe is the binary `start --detach` runs (a variable for tests).
var detachExe = os.Executable

// notReady returns the not-ready services and their reasons from a
// starting instance's readiness endpoint, one per line, or "".
func notReady(cfg *config.Config) string {
	c, err := clientFor(cfg)
	if err != nil {
		return ""
	}
	_, b, err := c.do(http.MethodGet, "/_emu/v1/ready")
	if err != nil {
		return ""
	}
	var r struct {
		Services map[string]struct {
			Ready  bool   `json:"ready"`
			Reason string `json:"reason"`
		} `json:"services"`
	}
	if json.Unmarshal(b, &r) != nil {
		return ""
	}
	var lines []string
	for name, st := range r.Services {
		if !st.Ready {
			lines = append(lines, "  "+name+": "+st.Reason)
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// logTail returns the last n lines written to the log at path after
// offset start.
func logTail(path string, start int64, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return ""
	}
	b, _ := io.ReadAll(io.LimitReader(f, 1<<20))
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i := range lines {
		lines[i] = "  " + lines[i]
	}
	return strings.Join(lines, "\n")
}

func (o *rootOpts) stopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop a running instance",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := o.load(cmd.Flags())
			if err != nil {
				return err
			}
			c, err := clientFor(cfg)
			if err != nil {
				return err
			}
			pidFile := filepath.Join(cfg.InstanceDir(), instance.PIDFile)
			pid := 0
			if b, err := os.ReadFile(pidFile); err == nil {
				pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
			}
			if _, _, err := c.do(http.MethodPost, "/_emu/v1/shutdown"); err != nil {
				return err
			}
			// Done once the PID file is gone and the process has exited, so
			// that scripts can start a new instance right away.
			for i := 0; i < 600; i++ {
				_, err := os.Stat(pidFile)
				if os.IsNotExist(err) && (pid <= 0 || !processAlive(pid)) {
					fmt.Fprintf(cmd.OutOrStdout(), "gcpemu instance %q stopped\n", cfg.Instance)
					return nil
				}
				time.Sleep(100 * time.Millisecond)
			}
			return errors.New("timed out waiting for shutdown")
		},
	}
}

func (o *rootOpts) statusCmd() *cobra.Command {
	var asJSON, needReady bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show instance status",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := o.load(cmd.Flags())
			if err != nil {
				return err
			}
			c, err := clientFor(cfg)
			if err != nil {
				return fmt.Errorf("instance %q is not running", cfg.Instance)
			}
			info, err := c.get("/_emu/v1/info")
			if err != nil {
				return fmt.Errorf("instance %q is not running: %w", cfg.Instance, err)
			}
			code, ready, _ := c.do(http.MethodGet, "/_emu/v1/ready")
			// --ready makes status a readiness probe (NFR-PORT-004: the OCI
			// image's HEALTHCHECK, as distroless has no curl).
			notReady := func() error {
				if needReady && code != http.StatusOK {
					return fmt.Errorf("instance %q is not ready", cfg.Instance)
				}
				return nil
			}
			cts, err := c.get("/_emu/v1/containers")
			if err != nil {
				cts = []byte(`{"containers":[]}`)
			}
			if asJSON {
				fmt.Fprintf(cmd.OutOrStdout(), "{\"info\":%s,\"ready\":%s,\"containers\":%s}\n", info, ready, cts)
				return notReady()
			}
			var i struct {
				Instance, ID, Version, Dir, IamMode string
				Pid                                 int
				Endpoints                           map[string]string
			}
			var r struct {
				Services map[string]struct {
					Ready  bool
					Reason string
				}
			}
			_ = json.Unmarshal(info, &i)
			_ = json.Unmarshal(ready, &r)
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "instance %s (id %s, pid %d, version %s)\ndir      %s\niam      %s\n", i.Instance, i.ID, i.Pid, i.Version, i.Dir, i.IamMode)
			fmt.Fprintln(out, "services:")
			for _, n := range sortedKeys(r.Services) {
				s := r.Services[n]
				state := "ready"
				if !s.Ready {
					state = "not ready: " + s.Reason
				}
				fmt.Fprintf(out, "  %-8s %s\n", n, state)
			}
			fmt.Fprintln(out, "endpoints:")
			for _, n := range sortedKeys(i.Endpoints) {
				fmt.Fprintf(out, "  %-8s %s\n", n, i.Endpoints[n])
			}
			var ct struct{ Containers []instance.ContainerInfo }
			_ = json.Unmarshal(cts, &ct)
			fmt.Fprintf(out, "containers: %d\n", len(ct.Containers))
			for _, x := range ct.Containers {
				fmt.Fprintf(out, "  %-8s %-24s %-8s %s (%s)\n", x.Service, x.Resource, x.Role, x.Name, x.State)
			}
			return notReady()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.Flags().BoolVar(&needReady, "ready", false, "exit non-zero unless every service is ready")
	return cmd
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (o *rootOpts) resetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reset",
		Short: "Wipe all state of a running instance, keeping configuration",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := o.load(cmd.Flags())
			if err != nil {
				return err
			}
			c, err := clientFor(cfg)
			if err != nil {
				return err
			}
			code, body, err := c.do(http.MethodPost, "/_emu/v1/reset")
			if err != nil {
				return err
			}
			if code != http.StatusOK {
				return fmt.Errorf("reset failed: %s", body)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "state reset")
			return nil
		},
	}
}

func (o *rootOpts) logsCmd() *cobra.Command {
	var requests bool
	cmd := &cobra.Command{
		Use:   "logs [service]",
		Short: "Print the instance log, or the request log with --requests",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := o.load(cmd.Flags())
			if err != nil {
				return err
			}
			svc := ""
			if len(args) == 1 {
				svc = args[0]
			}
			if requests {
				c, err := clientFor(cfg)
				if err != nil {
					return err
				}
				b, err := c.get("/_emu/v1/requests?service=" + svc)
				if err != nil {
					return err
				}
				_, err = cmd.OutOrStdout().Write(b)
				return err
			}
			b, err := os.ReadFile(filepath.Join(cfg.InstanceDir(), instance.LogFile))
			if err != nil {
				return err
			}
			for _, line := range strings.SplitAfter(string(b), "\n") {
				if svc == "" || strings.Contains(line, "service="+svc) || strings.Contains(line, `"service":"`+svc+`"`) {
					fmt.Fprint(cmd.OutOrStdout(), line)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&requests, "requests", false, "print the structured request log from the running instance")
	return cmd
}

func (o *rootOpts) versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "gcpemu %s\n", instance.Version)
		},
	}
}

func (o *rootOpts) timeCmd() *cobra.Command {
	t := &cobra.Command{Use: "time", Short: "Control the emulator clock"}
	t.AddCommand(&cobra.Command{
		Use:   "advance DURATION",
		Short: "Advance the emulator clock (lifecycle rules, TTLs)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := o.load(cmd.Flags())
			if err != nil {
				return err
			}
			c, err := clientFor(cfg)
			if err != nil {
				return err
			}
			code, body, err := c.do(http.MethodPost, "/_emu/v1/time/advance?by="+args[0])
			if err != nil {
				return err
			}
			if code != http.StatusOK {
				return fmt.Errorf("%s", body)
			}
			_, err = cmd.OutOrStdout().Write(body)
			return err
		},
	})
	return t
}

// processAlive reports whether a process with pid exists.
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
