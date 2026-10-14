package cli

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/linuxuser586/gcpemu/internal/ca"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/loopback"
	"github.com/linuxuser586/gcpemu/internal/netplane"
	emuruntime "github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/internal/vmrelay"
)

// doctorCmd implements FR-CORE-007: check the host and print fixes.
func (o *rootOpts) doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check container runtime, ports, CA trust, disk, arch and credentials",
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

			hasRuntime := false
			if checkPlatform(out, report, runtime.GOOS, runtime.GOARCH) {
				cl, det, rtErr := emuruntime.Locate()
				if rtErr != nil {
					fix := "install Docker Engine >= 24, Docker Desktop, OrbStack, Colima or Podman >= 4.9 (needed only for GKE, Cloud SQL and Cloud NAT)"
					if det.Source != "" {
						fix = "start that runtime, or choose a running one (unset DOCKER_HOST or DOCKER_CONTEXT, or `docker context use`)"
					}
					report(false, "container runtime: "+rtErr.Error(), fix)
				} else {
					report(true, fmt.Sprintf("container runtime %s (chosen by %s)", det.Endpoint, det.Source), "")
				}
				if others := slices.DeleteFunc(slices.Clone(det.Found), func(s string) bool { return "unix://"+s == det.Endpoint }); len(others) > 0 {
					fmt.Fprintf(out, "       also found: %s\n", strings.Join(others, ", "))
				}
				hasRuntime = rtErr == nil
				if hasRuntime {
					checkVMRelay(cmd.Context(), out, cl)
				}
			}

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

			checkLBBind(out, report, cfg, running == nil, hasRuntime)
			if runtime.GOOS == "darwin" {
				checkLoopback(out, loopback.Aliases())
			}

			dir := cfg.InstanceDir()
			if err := os.MkdirAll(dir, 0o700); err != nil {
				report(false, "data dir "+dir+" writable", err.Error())
			} else {
				if free, err := freeGiB(dir); err == nil {
					report(free >= 2, fmt.Sprintf("disk: %d GiB free in %s", free, dir), "free at least 2 GiB (container images and Postgres data)")
				}
			}

			caPath := filepath.Join(dir, ca.CertFile)
			if pemBytes, err := os.ReadFile(caPath); err == nil {
				fmt.Fprintf(out, "[ok  ] emulator CA present at %s\n", caPath)
				checkCATrust(out, cfg.Instance, pemBytes)
			} else {
				fmt.Fprintln(out, "[--  ] emulator CA not created yet (created on first start)")
			}

			checkForeignCreds(out, report)

			if problems > 0 {
				return fmt.Errorf("%d problem(s) found", problems)
			}
			fmt.Fprintln(out, "all checks passed")
			return nil
		},
	}
}

// checkPlatform reports goos/goarch and what it supports, and whether an
// Instance there can use a container runtime (emuruntime.UnusableOn), so
// the runtime is worth checking.
func checkPlatform(out io.Writer, report func(bool, string, string), goos, goarch string) bool {
	platform := fmt.Sprintf("platform %s/%s", goos, goarch)
	report(goos == "linux" || goos == "darwin" || goos == "windows", platform, "run on Linux, macOS or Windows")
	if goos == "darwin" {
		fmt.Fprintln(out, "       note: GKE is Linux-only in v1.0 (NFR-PORT-003)")
	}
	if err := emuruntime.UnusableOn(goos); err != nil {
		fmt.Fprintf(out, "[--  ] Cloud SQL, GKE, Cloud NAT and host mode: %v (ADR 0008); every other Service runs here\n", err)
		return false
	}
	return true
}

// checkVMRelay prints whether an Instance started now uses the VM relay
// (ADR 0005), and why. Its services network does not exist before it
// starts, so the runtime's default bridge network stands in for it.
func checkVMRelay(ctx context.Context, out io.Writer, cl *emuruntime.Client) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	gw := ""
	for _, name := range []string{"bridge", "podman"} {
		if n, err := cl.InspectNetwork(ctx, name); err == nil && n.Gateway != "" {
			gw = n.Gateway
			break
		}
	}
	if gw == "" && cl.Self() == "" && os.Getenv(vmrelay.Env) == "" {
		fmt.Fprintln(out, "[--  ] VM relay: undetermined (no default bridge network to probe)")
		return
	}
	fmt.Fprintf(out, "[ok  ] VM relay: %s\n", vmrelay.Decide(cl.Self() != "", gw))
}

// checkLoopback reports whether the host has the 127.0.0.x loopback
// aliases (ADR 0005). Missing ones are a note, not a failure: one Cloud SQL
// instance and the forwarding rules still work on 127.0.0.1.
func checkLoopback(out io.Writer, aliases bool) {
	if aliases {
		fmt.Fprintln(out, "[ok  ] loopback aliases 127.0.0.2-127.0.0.254 present")
		return
	}
	fmt.Fprintf(out, "[--  ] loopback aliases 127.0.0.2-127.0.0.254 missing: `%s` lets more than one Cloud SQL instance run and gives each load balancer forwarding rule its own address\n", loopback.InstallHint)
}

// checkDNSUDP checks the UDP side of the DNS port. An mDNS daemon (avahi or
// browsers on most Linux desktops, mDNSResponder on macOS) holds UDP
// 0.0.0.0:5353; the emulator and its services-network forwarder share the
// port on a specific address, so that clash is a note, not a failure, unless
// the bind is the wildcard.
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
		fmt.Fprintf(out, "[ok  ] port %s/udp (dns) shared with another socket on the wildcard address, likely mDNS (avahi-daemon, mDNSResponder or a browser); DNS answers still reach the emulator\n", addr)
		return
	}
	report(false, fmt.Sprintf("port %s/udp (dns) free", addr),
		"stop whatever holds it, bind a specific address (--bind 127.0.0.1) or use --port dns=5354")
}

// checkCATrust reports where the CA is trusted: the OS store, then each
// browser (NSS) database on Linux, which browsers read instead of the OS
// store. Missing trust is a note, not a failure, since only HTTPS to
// load balancers and host mode need it.
func checkCATrust(out io.Writer, instance string, pemBytes []byte) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		fmt.Fprintln(out, "[--  ] emulator CA unreadable; skipping trust checks")
		return
	}
	trusted := func(ok bool, where string) {
		if ok {
			fmt.Fprintf(out, "[ok  ] emulator CA trusted by %s\n", where)
		} else {
			fmt.Fprintf(out, "[--  ] emulator CA not trusted by %s (run `gcpemu ca install`)\n", where)
		}
	}
	trusted(osTrustsCA(block.Bytes), "the OS trust store")
	if runtime.GOOS != "linux" {
		return
	}
	home, _ := nssUser(os.Geteuid() == 0)
	var dbs []nssDB
	for _, d := range nssDBs(home) {
		if fileExists(filepath.Join(d.dir, "cert9.db")) {
			dbs = append(dbs, d)
		}
	}
	if len(dbs) == 0 {
		return
	}
	if !certutilPresent() {
		fmt.Fprintf(out, "[--  ] cannot check browser (NSS) trust: certutil not found (install %s, then run `gcpemu ca install`)\n", nssProvider)
		return
	}
	for _, d := range dbs {
		trusted(nssHas(d.dir, nssNickname(instance)), d.what+" ("+d.dir+")")
	}
}

// systemBundles are the CA bundles update-ca-certificates and
// update-ca-trust generate. They are read directly, not through Go's
// system pool, so SSL_CERT_FILE (`gcpemu env --trust`) cannot pass for
// OS trust.
var systemBundles = []string{
	"/etc/ssl/certs/ca-certificates.crt",
	"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
	"/etc/pki/tls/certs/ca-bundle.crt",
}

// osTrustsCA reports whether the OS trust store holds the CA (DER).
func osTrustsCA(der []byte) bool {
	if runtime.GOOS != "linux" {
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return false
		}
		_, err = cert.Verify(x509.VerifyOptions{KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
		return err == nil
	}
	for _, f := range systemBundles {
		rest, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for {
			var b *pem.Block
			if b, rest = pem.Decode(rest); b == nil {
				break
			}
			if bytes.Equal(b.Bytes, der) {
				return true
			}
		}
	}
	return false
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

// checkForeignCreds reports Foreign credentials that ADC would pick up
// (#247): a GOOGLE_APPLICATION_CREDENTIALS file from outside the Invalid
// universe, or gcloud's application-default credentials, which `gcpemu
// env` overrides only while GOOGLE_APPLICATION_CREDENTIALS is unset.
func checkForeignCreds(out io.Writer, report func(bool, string, string)) {
	if p := os.Getenv(adcEnv); p != "" {
		why, err := foreignCredsFile(p)
		switch {
		case err != nil:
			report(false, adcEnv+": "+err.Error(), "point it at a readable key file, unset it, or run `eval \"$(gcpemu env)\"` again")
		case why != "":
			report(false, adcEnv+": "+why, "unset it, or use a key created against gcpemu; its token requests go to real Google")
		default:
			report(true, adcEnv+" is an emulator key", "")
		}
		return
	}
	if wk := wellKnownADC(); wk != "" {
		fmt.Fprintf(out, "[--  ] gcloud's application-default credentials at %s go to real Google; `eval \"$(gcpemu env)\"` points ADC at an emulator key instead\n", wk)
	}
}
