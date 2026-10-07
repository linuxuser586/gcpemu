package cli

import (
	"bufio"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/linuxuser586/gcpemu/internal/ca"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/frontend"
	"github.com/linuxuser586/gcpemu/internal/hostmode"
)

// `gcpemu ca` manages the instance's local root CA (Section 7.4,
// FR-CORE-043): print it, add it to (or remove it from) the OS trust store
// and rotate it.

func (o *rootOpts) caCmd() *cobra.Command {
	root := &cobra.Command{Use: "ca", Short: "Manage the instance's local certificate authority"}
	loadCA := func(cmd *cobra.Command) (*config.Config, *ca.CA, error) {
		cfg, err := o.load(cmd.Flags())
		if err != nil {
			return nil, nil, err
		}
		c, err := ca.Load(cfg.InstanceDir(), cfg.Instance)
		return cfg, c, err
	}
	path := &cobra.Command{
		Use:   "path",
		Short: "Print the path of the CA certificate (ca.pem)",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, c, err := loadCA(cmd)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), c.Path())
			return nil
		},
	}
	print := &cobra.Command{
		Use:   "print",
		Short: "Print the CA certificate (PEM)",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, c, err := loadCA(cmd)
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(c.PEM())
			return err
		},
	}
	var yes, dryRun bool
	install := &cobra.Command{
		Use:   "install",
		Short: "Add the CA to the OS, Docker and containerd trust stores (asks for confirmation; uses sudo)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, c, err := loadCA(cmd)
			if err != nil {
				return err
			}
			h := hostTrust()
			plan, err := trustPlan(h, cfg.Instance, c, true)
			if err != nil {
				return err
			}
			plan = append(plan, containerTrustPlan(h, cfg.Instance, c, true)...)
			return runPlan(cmd, plan, yes, dryRun)
		},
	}
	install.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation (sudo may still prompt for a password)")
	install.Flags().BoolVar(&dryRun, "dry-run", false, "only print the commands")
	uninstall := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the CA from the OS, Docker and containerd trust stores (asks for confirmation; uses sudo)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, c, err := loadCA(cmd)
			if err != nil {
				return err
			}
			h := hostTrust()
			plan, err := trustPlan(h, cfg.Instance, c, false)
			if err != nil {
				return err
			}
			plan = append(plan, containerTrustPlan(h, cfg.Instance, c, false)...)
			return runPlan(cmd, plan, yes, dryRun)
		},
	}
	uninstall.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation (sudo may still prompt for a password)")
	uninstall.Flags().BoolVar(&dryRun, "dry-run", false, "only print the commands")
	rotate := &cobra.Command{
		Use:   "rotate",
		Short: "Replace the CA with a new one and re-issue managed certificates",
		Long: "Replaces the instance CA. On a running instance the certs service rotates it live and\n" +
			"re-issues every Google-managed certificate; a stopped instance is rotated on disk and\n" +
			"re-issues on next start. Re-run `gcpemu ca install` afterwards.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := o.load(cmd.Flags())
			if err != nil {
				return err
			}
			if cl, err := clientFor(cfg); err == nil {
				code, b, err := cl.do(http.MethodPost, "/_emu/v1/ca/rotate")
				switch {
				case err == nil && code == http.StatusOK:
					fmt.Fprintf(cmd.OutOrStdout(), "Rotated the CA of running instance %q; managed certificates were re-issued.\n", cfg.Instance)
					fmt.Fprintln(cmd.OutOrStdout(), "Run `gcpemu ca install` to trust the new CA.")
					return nil
				case err == nil && code == http.StatusNotFound:
					return fmt.Errorf("instance %q is running without the certs service; stop it (`gcpemu stop`) and rotate again", cfg.Instance)
				case err == nil:
					return fmt.Errorf("rotate: %d %s", code, strings.TrimSpace(string(b)))
				}
				// Stale endpoints file: the instance is not reachable.
			}
			c, err := ca.Load(cfg.InstanceDir(), cfg.Instance)
			if err != nil {
				return err
			}
			if err := c.Rotate(cfg.Instance); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Rotated the CA in %s; managed certificates are re-issued on next start.\n", c.Path())
			fmt.Fprintln(cmd.OutOrStdout(), "Run `gcpemu ca install` to trust the new CA.")
			return nil
		},
	}
	root.AddCommand(path, print, install, uninstall, rotate)
	return root
}

// trustHost describes the host's trust store tooling.
type trustHost struct {
	goos     string
	root     bool
	etc      string // "/etc" unless set (tests)
	lookPath func(string) bool
	isDir    func(string) bool
}

func hostTrust() trustHost {
	return trustHost{
		goos:     goruntime.GOOS,
		root:     os.Geteuid() == 0,
		lookPath: func(n string) bool { _, err := exec.LookPath(n); return err == nil },
		isDir:    func(p string) bool { fi, err := os.Stat(p); return err == nil && fi.IsDir() },
	}
}

// trustStep is one command of an install/uninstall plan.
type trustStep struct {
	args []string
	// stdin is fed to the command (used to write the certificate with
	// `sudo tee` so no temp file is needed).
	stdin []byte
	// script is shown below the command when stdin is a shell script.
	script bool
}

func (s trustStep) String() string { return strings.Join(s.args, " ") }

// trustPlan returns the commands that add (install) or remove the CA.
func trustPlan(h trustHost, instance string, c *ca.CA, install bool) ([]trustStep, error) {
	sudo := func(args ...string) []string {
		if h.root {
			return args
		}
		return append([]string{"sudo"}, args...)
	}
	file := "gcpemu-" + instance + ".crt"
	switch h.goos {
	case "linux":
		var dir, update []string
		switch {
		case h.isDir("/usr/local/share/ca-certificates") && h.lookPath("update-ca-certificates"):
			dir = []string{"/usr/local/share/ca-certificates"}
			update = []string{"update-ca-certificates"}
			if !install {
				update = append(update, "--fresh")
			}
		case h.isDir("/etc/pki/ca-trust/source/anchors") && h.lookPath("update-ca-trust"):
			dir = []string{"/etc/pki/ca-trust/source/anchors"}
			update = []string{"update-ca-trust", "extract"}
		default:
			return nil, errors.New("no supported trust store found (need update-ca-certificates or update-ca-trust); trust the file from `gcpemu ca path` manually")
		}
		target := filepath.Join(dir[0], file)
		if install {
			return []trustStep{
				{args: sudo("tee", target), stdin: c.PEM()},
				{args: sudo(update...)},
			}, nil
		}
		return []trustStep{{args: sudo("rm", "-f", target)}, {args: sudo(update...)}}, nil
	case "darwin":
		const keychain = "/Library/Keychains/System.keychain"
		if install {
			return []trustStep{{args: sudo("security", "add-trusted-cert", "-d", "-r", "trustRoot", "-k", keychain, c.Path())}}, nil
		}
		sum := sha1.Sum(c.Certificate().Raw)
		return []trustStep{{args: sudo("security", "delete-certificate", "-Z", strings.ToUpper(hex.EncodeToString(sum[:])), keychain)}}, nil
	}
	return nil, fmt.Errorf("automatic trust store changes are not supported on %s; trust the file from `gcpemu ca path` manually", h.goos)
}

// runPlan prints the plan, asks for confirmation unless yes, and runs it
// attached to the terminal so sudo can prompt for a password.
func runPlan(cmd *cobra.Command, plan []trustStep, yes, dryRun bool) error {
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "The following commands will be run:")
	for _, s := range plan {
		fmt.Fprintf(out, "  %s\n", s)
		if s.script {
			for _, l := range strings.Split(strings.TrimRight(string(s.stdin), "\n"), "\n") {
				fmt.Fprintf(out, "      | %s\n", l)
			}
		}
	}
	if dryRun {
		return nil
	}
	if !yes {
		if fi, err := os.Stdin.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
			return errors.New("not a terminal: re-run with --yes to confirm")
		}
		fmt.Fprint(out, "Proceed? [y/N] ")
		line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			return errors.New("aborted")
		}
	}
	for _, s := range plan {
		c := exec.Command(s.args[0], s.args[1:]...)
		c.Stdout, c.Stderr = io.Discard, cmd.ErrOrStderr()
		if s.stdin != nil {
			c.Stdin = strings.NewReader(string(s.stdin))
		} else {
			c.Stdin, c.Stdout = os.Stdin, out
		}
		if err := c.Run(); err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
	}
	fmt.Fprintln(out, "Done.")
	return nil
}

// containerTrustPlan returns the step that makes Docker and containerd
// trust the CA for the registry hosts the emulator serves over TLS
// (LOCATION-docker.pkg.dev in host mode; Section 7.4). They keep their
// own per-registry trust, read on every pull, so no daemon restart is
// needed. Docker reads every *.crt in certs.d/HOST, so the CA goes in as
// gcpemu-INSTANCE.crt next to the user's files; containerd's hosts.toml
// is written only where none exists or gcpemu wrote it (config_path must
// point at /etc/containerd/certs.d, as on GKE and k3s). Linux only: Docker
// Desktop on macOS uses the system keychain.
func containerTrustPlan(h trustHost, instance string, c *ca.CA, install bool) []trustStep {
	if h.goos != "linux" {
		return nil
	}
	etc := h.etc
	if etc == "" {
		etc = "/etc"
	}
	docker, containerd := h.isDir(etc+"/docker"), h.isDir(etc+"/containerd")
	if !docker && !containerd {
		return nil
	}
	var hosts []string
	for _, n := range hostmode.HostsBlockNames(nil) {
		if frontend.IsRegistryHost(n) {
			hosts = append(hosts, n)
		}
	}
	file := "gcpemu-" + instance + ".crt"
	shared := etc + "/gcpemu/" + file
	marker := "# written by gcpemu (" + instance + ")"
	var b strings.Builder
	b.WriteString("set -e\nhosts='" + strings.Join(hosts, " ") + "'\n")
	if install {
		b.WriteString("mkdir -p " + etc + "/gcpemu\ncat > " + shared + " <<'GCPEMU_CA'\n" + strings.TrimSpace(string(c.PEM())) + "\nGCPEMU_CA\n")
		b.WriteString("for h in $hosts; do\n")
		if docker {
			b.WriteString("  mkdir -p " + etc + "/docker/certs.d/$h && cp " + shared + " " + etc + "/docker/certs.d/$h/" + file + "\n")
		}
		if containerd {
			b.WriteString("  f=" + etc + "/containerd/certs.d/$h/hosts.toml\n")
			b.WriteString("  if [ ! -e $f ] || grep -q '^# written by gcpemu' $f; then\n")
			b.WriteString("    mkdir -p " + etc + "/containerd/certs.d/$h\n")
			b.WriteString("    printf '%s\\nserver = \"https://%s\"\\n\\n[host.\"https://%s\"]\\n  ca = \"%s\"\\n' '" + marker + "' $h $h " + shared + " > $f\n")
			b.WriteString("  fi\n")
		}
		b.WriteString("done\n")
	} else {
		b.WriteString("for h in $hosts; do\n")
		if docker {
			b.WriteString("  rm -f " + etc + "/docker/certs.d/$h/" + file + "\n  rmdir " + etc + "/docker/certs.d/$h 2>/dev/null || true\n")
		}
		if containerd {
			b.WriteString("  f=" + etc + "/containerd/certs.d/$h/hosts.toml\n")
			b.WriteString("  if [ -e $f ] && grep -qF '" + marker + "' $f; then rm -f $f; rmdir " + etc + "/containerd/certs.d/$h 2>/dev/null || true; fi\n")
		}
		b.WriteString("done\nrm -f " + shared + "\n")
	}
	args := []string{"sh", "-s"}
	if !h.root {
		args = append([]string{"sudo"}, args...)
	}
	return []trustStep{{args: args, stdin: []byte(b.String()), script: true}}
}
