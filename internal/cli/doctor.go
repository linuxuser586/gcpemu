package cli

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"syscall"

	"github.com/spf13/cobra"
)

// doctorCmd implements FR-CORE-007: check the host and print fixes.
func (o *rootOpts) doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check container runtime, ports, CA trust, disk and arch",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := o.load(cmd.Flags())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			problems := 0
			report := func(ok bool, what, fix string) {
				mark := "ok  "
				if !ok {
					mark = "FAIL"
					problems++
				}
				fmt.Fprintf(out, "[%s] %s\n", mark, what)
				if !ok && fix != "" {
					fmt.Fprintf(out, "       fix: %s\n", fix)
				}
			}

			report(runtime.GOOS == "linux" || runtime.GOOS == "darwin",
				fmt.Sprintf("platform %s/%s", runtime.GOOS, runtime.GOARCH),
				"run on Linux or macOS (Windows via WSL2)")
			if runtime.GOOS == "darwin" {
				fmt.Fprintln(out, "       note: GKE is Linux-only in v1.0 (NFR-PORT-003)")
			}

			rt := detectRuntime()
			report(rt != "", "container runtime "+orNone(rt),
				"install Docker Engine >= 24, Podman >= 4.9 or containerd >= 1.7 (needed only for GKE and Cloud SQL)")

			_, running := readEndpoints(cfg)
			if running == nil {
				fmt.Fprintf(out, "[ok  ] instance %q is running; skipping port checks\n", cfg.Instance)
			} else {
				names := make([]string, 0, len(cfg.Ports))
				for n := range cfg.Ports {
					names = append(names, n)
				}
				sort.Strings(names)
				for _, n := range names {
					p := cfg.Port(n)
					if p == 0 {
						continue
					}
					addr := net.JoinHostPort(cfg.Bind, strconv.Itoa(p))
					l, err := net.Listen("tcp", addr)
					if err == nil {
						l.Close()
					}
					report(err == nil, fmt.Sprintf("port %s (%s) free", addr, n),
						fmt.Sprintf("stop whatever holds it or use --port %s=0", n))
				}
			}

			dir := cfg.InstanceDir()
			if err := os.MkdirAll(dir, 0o700); err != nil {
				report(false, "data dir "+dir+" writable", err.Error())
			} else {
				var st syscall.Statfs_t
				if err := syscall.Statfs(dir, &st); err == nil {
					free := st.Bavail * uint64(st.Bsize) >> 30
					report(free >= 2, fmt.Sprintf("disk: %d GiB free in %s", free, dir), "free at least 2 GiB (container images and Postgres data)")
				}
			}

			ca := filepath.Join(dir, "ca.pem")
			if _, err := os.Stat(ca); err == nil {
				fmt.Fprintf(out, "[ok  ] emulator CA present at %s (trust it with `gcpemu ca install`)\n", ca)
			} else {
				fmt.Fprintln(out, "[--  ] emulator CA not created yet (created on first start)")
			}

			if problems > 0 {
				return fmt.Errorf("%d problem(s) found", problems)
			}
			fmt.Fprintln(out, "all checks passed")
			return nil
		},
	}
}

func orNone(s string) string {
	if s == "" {
		return "(none found)"
	}
	return s
}

// detectRuntime finds a usable container runtime CLI or socket.
func detectRuntime() string {
	for _, sock := range []string{os.Getenv("DOCKER_HOST"), "/var/run/docker.sock", "/run/podman/podman.sock", "/run/containerd/containerd.sock"} {
		if sock == "" {
			continue
		}
		if fi, err := os.Stat(trimUnix(sock)); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return sock
		}
	}
	for _, bin := range []string{"docker", "podman", "nerdctl"} {
		if p, err := exec.LookPath(bin); err == nil {
			return p
		}
	}
	return ""
}

func trimUnix(s string) string {
	if len(s) > 7 && s[:7] == "unix://" {
		return s[7:]
	}
	return s
}
