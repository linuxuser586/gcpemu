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
	"path"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/linuxuser586/gcpemu/internal/ca"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/frontend"
	"github.com/linuxuser586/gcpemu/internal/hostmode"
	emuruntime "github.com/linuxuser586/gcpemu/internal/runtime"
)

// `gcpemu ca` manages the instance's local root CA (Section 7.4,
// FR-CORE-043): print it, add it to (or remove it from) the OS, container
// and browser trust stores and rotate it.

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
		Short: "Add the CA to the OS, Docker, containerd and browser (NSS) trust stores (asks for confirmation; uses sudo on Linux and macOS)",
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
			afterPlan := printVMTrust(cmd.OutOrStdout(), cfg.Instance, c, true, dryRun)
			nss, note := nssPlan(h, cfg.Instance, c, nssInstall)
			if note != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "note: "+note)
			}
			windowsTrustNote(cmd.ErrOrStderr(), h, true)
			if err := runPlan(cmd, append(plan, nss...), yes, dryRun); err != nil {
				return err
			}
			afterPlan()
			return nil
		},
	}
	install.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation (sudo may still prompt for a password; Windows still shows its own dialog)")
	install.Flags().BoolVar(&dryRun, "dry-run", false, "only print the commands")
	uninstall := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the CA from the OS, Docker, containerd and browser (NSS) trust stores (asks for confirmation; uses sudo on Linux and macOS)",
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
			afterPlan := printVMTrust(cmd.OutOrStdout(), cfg.Instance, c, false, dryRun)
			nss, _ := nssPlan(h, cfg.Instance, c, nssUninstall)
			windowsTrustNote(cmd.ErrOrStderr(), h, false)
			if err := runPlan(cmd, append(plan, nss...), yes, dryRun); err != nil {
				return err
			}
			afterPlan()
			return nil
		},
	}
	uninstall.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation (sudo may still prompt for a password; Windows still shows its own dialog)")
	uninstall.Flags().BoolVar(&dryRun, "dry-run", false, "only print the commands")
	rotate := &cobra.Command{
		Use:   "rotate",
		Short: "Replace the CA with a new one and re-issue managed certificates",
		Long: "Replaces the instance CA. On a running instance the certs service rotates it live and\n" +
			"re-issues every Google-managed certificate; a stopped instance is rotated on disk and\n" +
			"re-issues on next start. Browser (NSS) databases that trust the old CA are updated\n" +
			"in place; re-run `gcpemu ca install` afterwards for the OS and container stores.",
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
					c, err := ca.Load(cfg.InstanceDir(), cfg.Instance)
					if err != nil {
						return err
					}
					return rotateTrust(cmd, cfg.Instance, c)
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
			return rotateTrust(cmd, cfg.Instance, c)
		},
	}
	root.AddCommand(path, print, install, uninstall, rotate)
	return root
}

// rotateTrust replaces the old CA with c in the browser databases that
// trusted it. They belong to the user, so this needs neither sudo nor
// confirmation; the OS and container stores still need `ca install`.
func rotateTrust(cmd *cobra.Command, instance string, c *ca.CA) error {
	if plan, _ := nssPlan(hostTrust(), instance, c, nssReplace); len(plan) > 0 {
		if err := runPlan(cmd, plan, true, false); err != nil {
			return err
		}
	}
	fmt.Fprintln(cmd.OutOrStdout(), "Run `gcpemu ca install` to trust the new CA in the OS and container stores.")
	return nil
}

// windowsTrustNote warns that Windows confirms a change to the current
// user's Root store in a dialog of its own, which the plan waits for.
func windowsTrustNote(out io.Writer, h trustHost, install bool) {
	if h.goos != "windows" {
		return
	}
	verb := "adding the CA to"
	if !install {
		verb = "removing the CA from"
	}
	fmt.Fprintf(out, "note: Windows shows a security dialog to confirm %s your Root certificate store; choose Yes\n", verb)
}

// trustHost describes the host's trust store tooling.
type trustHost struct {
	goos     string
	root     bool
	etc      string // "/etc" unless set (tests)
	lookPath func(string) bool
	isDir    func(string) bool

	// NSS (browser) databases: home holds them, nssUser owns them when
	// gcpemu runs as root under sudo, nssHas reports whether a database
	// holds a nickname.
	home       string
	nssUser    string
	nssHas     func(db, nick string) bool
	fileExists func(string) bool
}

func hostTrust() trustHost {
	h := trustHost{
		goos:       goruntime.GOOS,
		root:       os.Geteuid() == 0,
		lookPath:   func(n string) bool { _, err := exec.LookPath(n); return err == nil },
		isDir:      func(p string) bool { fi, err := os.Stat(p); return err == nil && fi.IsDir() },
		nssHas:     nssHas,
		fileExists: fileExists,
	}
	h.home, h.nssUser = nssUser(h.root)
	return h
}

// trustStep is one command of an install/uninstall plan.
type trustStep struct {
	args []string
	// stdin is fed to the command (used to write the certificate with
	// `sudo tee` so no temp file is needed).
	stdin []byte
	// script is shown below the command when stdin is a shell script.
	script bool
	// soft steps only warn on failure; the rest of the plan still runs.
	soft bool
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
		target := path.Join(dir[0], file)
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
		return []trustStep{{args: sudo("security", "delete-certificate", "-Z", strings.ToUpper(thumbprint(c)), keychain)}}, nil
	case "windows":
		// The current user's Root store needs no administrator rights;
		// Windows asks the user to confirm the change in its own dialog.
		if install {
			return []trustStep{{args: []string{"certutil", "-user", "-addstore", "Root", c.Path()}}}, nil
		}
		return []trustStep{{args: []string{"certutil", "-user", "-delstore", "Root", thumbprint(c)}}}, nil
	}
	return nil, fmt.Errorf("automatic trust store changes are not supported on %s; trust the file from `gcpemu ca path` manually", h.goos)
}

// thumbprint is the CA certificate's SHA-1 hash in hex, which the macOS
// keychain and the Windows certificate stores identify it by.
func thumbprint(c *ca.CA) string {
	sum := sha1.Sum(c.Certificate().Raw)
	return hex.EncodeToString(sum[:])
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
			if s.soft {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s: %v\n", s, err)
				continue
			}
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
// Desktop on macOS uses the system keychain, and a VM runtime's own store
// is left to the command from vmTrustCommand.
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
	var dirs []string
	if docker {
		dirs = append(dirs, etc+"/docker/certs.d")
	}
	script := certsDirScript(etc, instance, c, dirs, containerd, install)
	args := []string{"sh", "-s"}
	if !h.root {
		args = append([]string{"sudo"}, args...)
	}
	return []trustStep{{args: args, stdin: []byte(script), script: true}}
}

// printVMTrust prints what to do so the container trust store inside the
// detected runtime's VM gains (install) or loses the CA, if it keeps one;
// see vmTrustCommand. A restart is printed only by the returned afterPlan,
// once the plan has changed the store the runtime rebuilds from (or, on a
// dry run, been listed for the user to run).
func printVMTrust(out io.Writer, instance string, c *ca.CA, install, dryRun bool) (afterPlan func()) {
	_, det, _ := emuruntime.Locate()
	v := vmTrustCommand(socketTarget(det.Endpoint), instance, c, install)
	if v.store != "" {
		return func() { writeVMTrust(out, v, install, dryRun) }
	}
	writeVMTrust(out, v, install, dryRun)
	return func() {}
}

// socketTarget returns endpoint with a Unix socket's symlinks followed, so
// /var/run/docker.sock, which Docker Desktop on macOS links to its own
// socket by default, names the runtime behind it. Any other endpoint, or
// one that does not resolve, comes back as it is.
func socketTarget(endpoint string) string {
	if strings.Contains(endpoint, "://") && !strings.HasPrefix(endpoint, "unix://") {
		return endpoint
	}
	sock, unix := strings.CutPrefix(endpoint, "unix://")
	if sock == "" {
		return endpoint
	}
	real, err := filepath.EvalSymlinks(sock)
	if err != nil {
		return endpoint
	}
	if unix {
		return "unix://" + real
	}
	return real
}

// writeVMTrust writes the advice for v; nothing when v is empty. On a dry
// run a restart waits for the user to run the listed commands first.
func writeVMTrust(out io.Writer, v vmTrust, install, dryRun bool) {
	verb := "trust the CA for image pulls there"
	if !install {
		verb = "remove the CA from it"
	}
	switch {
	case v.store != "":
		restart := "Restart " + v.what
		if dryRun {
			restart = "After running these commands, restart " + v.what
		}
		fmt.Fprintf(out, "%s rebuilds its container trust store from %s when it starts. %s to %s.\n\n", v.what, v.store, restart, verb)
	case v.command != "":
		run := "run"
		if v.shell != "" {
			run = "run this in " + v.shell + " (PowerShell has no heredocs)"
		}
		fmt.Fprintf(out, "%s keeps its own container trust store inside its VM, which gcpemu does not change. To %s, %s:\n\n%s\n\n", v.what, verb, run, v.command)
	}
}

var (
	colimaSock = regexp.MustCompile(`/\.colima/([^/]+)/docker\.sock$`)
	// Podman machine: $TMPDIR/podman/NAME-api.sock, or (Podman 4)
	// .../podman/machine/PROVIDER/podman.sock, which does not name it.
	podmanSock    = regexp.MustCompile(`/podman/([^/]+)-api\.sock$`)
	podmanOldSock = regexp.MustCompile(`/podman/machine/[^/]+/podman\.sock$`)
)

// vmTrust is how a VM runtime's own container trust store gains or loses
// the CA: by command, run on the host from shell ("" for any POSIX shell),
// or by restarting the runtime, which rebuilds it from the host's store
// (named by store, "" for no restart). The zero value means nothing to do.
type vmTrust struct {
	what, command, shell, store string
}

// vmTrustCommand returns how the trust store inside the VM of the runtime
// at endpoint gains (install) or loses the CA. A VM runtime that keeps its
// own registry trust there (Colima, Podman machine) gets the command, run
// on the host, that installs or removes it; gcpemu never runs anything in
// the VM itself (#175). Docker Desktop builds its bundle from the store
// `ca install` changes (the keychain on macOS, the Windows certificate
// store on Windows) but only when it starts, when on Windows it also
// copies ~/.docker/certs.d over the VM's /etc/docker/certs.d; so it gets a
// restart, not a command (#377, #453). OrbStack also imports the keychain,
// but nothing says it needs a restart for that, so it (and every other
// runtime endpoint) gets the zero vmTrust; printVMTrust follows
// /var/run/docker.sock to the socket behind it first. docker_engine is
// Docker Desktop too: Docker Engine for Windows serves only Windows
// containers, which detection refuses (ADR 0015).
func vmTrustCommand(endpoint, instance string, c *ca.CA, install bool) vmTrust {
	var v vmTrust
	var ssh string
	var dirs []string
	sock := strings.TrimPrefix(endpoint, "unix://")
	if pipe, ok := strings.CutPrefix(endpoint, "npipe://"); ok {
		// The pipe's own name, from //./pipe/NAME or \\.\pipe\NAME.
		switch pipe[strings.LastIndexAny(pipe, `/\`)+1:] {
		case "dockerDesktopLinuxEngine", "docker_engine":
			return vmTrust{what: "Docker Desktop", store: "the Windows certificate store"}
		case "podman-machine-default":
			// The heredoc needs Git Bash: PowerShell has none, and piping
			// a here-string to a native command sends CRLFs, which break sh.
			v.what, v.shell, ssh = "Podman machine podman-machine-default", "Git Bash", "podman machine ssh podman-machine-default"
			dirs = []string{"/etc/containers/certs.d"}
		default:
			return vmTrust{}
		}
	} else if strings.HasSuffix(sock, "/.docker/run/docker.sock") {
		return vmTrust{what: "Docker Desktop", store: "the keychain"}
	} else if m := colimaSock.FindStringSubmatch(sock); m != nil {
		v.what, ssh = "Colima (profile "+m[1]+")", "colima ssh -p "+m[1]+" --"
		dirs = []string{"/etc/docker/certs.d"}
	} else if m := podmanSock.FindStringSubmatch(sock); m != nil {
		v.what, ssh = "Podman machine "+m[1], "podman machine ssh "+m[1]
		dirs = []string{"/etc/containers/certs.d"}
	} else if podmanOldSock.MatchString(sock) {
		v.what, ssh = "Podman machine", "podman machine ssh"
		dirs = []string{"/etc/containers/certs.d"}
	} else {
		return vmTrust{}
	}
	script := certsDirScript("/etc", instance, c, dirs, false, install)
	v.command = ssh + " sudo sh -s <<'GCPEMU_VM'\n" + script + "GCPEMU_VM"
	return v
}

// certsDirScript is the shell script that adds (install) or removes the CA
// for every registry host in each certs.d directory of dirs (Docker's and
// Podman's read every *.crt in DIR/HOST) and, with containerd, in
// containerd's hosts.toml files. The CA itself goes in etc/gcpemu.
func certsDirScript(etc, instance string, c *ca.CA, dirs []string, containerd, install bool) string {
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
		for _, d := range dirs {
			b.WriteString("  mkdir -p " + d + "/$h && cp " + shared + " " + d + "/$h/" + file + "\n")
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
		for _, d := range dirs {
			b.WriteString("  rm -f " + d + "/$h/" + file + "\n  rmdir " + d + "/$h 2>/dev/null || true\n")
		}
		if containerd {
			b.WriteString("  f=" + etc + "/containerd/certs.d/$h/hosts.toml\n")
			b.WriteString("  if [ -e $f ] && grep -qF '" + marker + "' $f; then rm -f $f; rmdir " + etc + "/containerd/certs.d/$h 2>/dev/null || true; fi\n")
		}
		b.WriteString("done\nrm -f " + shared + "\n")
	}
	return b.String()
}
