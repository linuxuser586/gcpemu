package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/netplane"
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
				var names []string
				for n := range config.DefaultPorts {
					names = append(names, n)
				}
				for n := range cfg.Ports {
					if _, dflt := config.DefaultPorts[n]; !dflt && n != config.AllPorts {
						names = append(names, n)
					}
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
					if n == "dns" {
						checkDNSUDP(out, report, cfg.Bind, strconv.Itoa(p))
					}
				}
			}

			checkLBBind(out, report, cfg, running == nil, rt != "")

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

// checkDNSUDP checks the UDP side of the DNS port. An mDNS daemon (avahi,
// browsers) holds UDP 0.0.0.0:5353 on most Linux desktops; the emulator and
// its services-network forwarder share the port on a specific address, so
// that clash is a note, not a failure, unless the bind is the wildcard.
func checkDNSUDP(out io.Writer, report func(bool, string, string), host, port string) {
	addr := net.JoinHostPort(host, port)
	pc, err := net.ListenPacket("udp", addr)
	if err == nil {
		pc.Close()
		report(true, fmt.Sprintf("port %s/udp (dns) free", addr), "")
		return
	}
	pc, err = netplane.ListenUDP(context.Background(), host, port)
	if err == nil {
		pc.Close()
		fmt.Fprintf(out, "[ok  ] port %s/udp (dns) shared with another socket on the wildcard address, likely mDNS (avahi-daemon or a browser); DNS answers still reach the emulator\n", addr)
		return
	}
	report(false, fmt.Sprintf("port %s/udp (dns) free", addr),
		"stop whatever holds it, bind a specific address (--bind 127.0.0.1) or use --port dns=5354")
}

// checkLBBind reports the --lb-bind addresses: those the running instance
// serves forwarding rules on, otherwise whether the configured ones can be
// bound (an address of this host; privileged ports need the lb-lan relay).
func checkLBBind(out io.Writer, report func(bool, string, string), cfg *config.Config, running, hasRuntime bool) {
	if running {
		c, err := clientFor(cfg)
		if err != nil {
			return
		}
		b, err := c.get("/_emu/v1/info")
		if err != nil {
			return
		}
		var i struct{ Endpoints map[string]string }
		_ = json.Unmarshal(b, &i)
		for _, n := range sortedKeys(i.Endpoints) {
			if name, ok := strings.CutPrefix(n, "lb-lan:"); ok {
				fmt.Fprintf(out, "[ok  ] forwarding rule %s exposed beyond loopback on %s\n", name, i.Endpoints[n])
			}
		}
		return
	}
	if len(cfg.LBBind) == 0 {
		return
	}
	local := map[string]bool{}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				local[ipn.IP.String()] = true
			}
		}
	}
	for _, a := range cfg.LBBind {
		ip := net.ParseIP(a)
		report(ip != nil && local[ip.String()], fmt.Sprintf("--lb-bind %s is an address of this host", a),
			"pick one from `ip -br addr` (or `ifconfig`)")
	}
	if b, err := os.ReadFile("/proc/sys/net/ipv4/ip_unprivileged_port_start"); err == nil {
		start, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		if start > 80 {
			report(hasRuntime, fmt.Sprintf("--lb-bind ports below %d relayed by the lb-lan container", start),
				"install a container runtime, or let the emulator bind them: sudo sysctl net.ipv4.ip_unprivileged_port_start=80")
		}
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
