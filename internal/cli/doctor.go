package cli

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
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
	"github.com/linuxuser586/gcpemu/internal/frontend"
	"github.com/linuxuser586/gcpemu/internal/instance"
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

			checkPlatform(report, runtime.GOOS, runtime.GOARCH)
			inContainer := emuruntime.InContainer()
			cl, det, rtErr := emuruntime.Locate()
			checkRuntimeDetection(out, report, det, rtErr)
			hasRuntime := rtErr == nil
			// hostRuntime: a runtime whose host network is this host's,
			// for a relay container there.
			hostRuntime := false
			if hasRuntime {
				vm, known := checkVMRelay(cmd.Context(), out, cl)
				hostRuntime = known && !vm && cl.Self() == ""
				info, infoErr := cl.Info(cmd.Context())
				if cl.Self() != "" && infoErr != nil {
					fmt.Fprintf(out, "[--  ] VM runtime: undetermined (cannot read the runtime's info: %v)\n", infoErr)
				} else if cl.Self() != "" {
					checkVMRuntime(cmd.Context(), out, cl, cfg, info)
				}
				if infoErr == nil {
					checkRuntimeArch(out, runtime.GOOS, machineArch(), info.Arch)
				}
				found, err := findAbandoned(cmd.Context(), cl)
				checkAbandoned(out, found, err)
			}

			_, running := readEndpoints(cfg)
			if running == nil {
				fmt.Fprintf(out, "[ok  ] instance %q is running; skipping port checks\n", cfg.Instance)
			} else {
				for _, n := range portNames(cfg) {
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

			checkLANBind(out, report, cfg, running == nil, hasRuntime, hostRuntime, inContainer)
			if running == nil {
				checkUnpublished(out, cfg)
			}
			checkHostOnly(out, cfg, runtime.GOOS, inContainer, running == nil)

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
				checkCATrust(out, cfg.Instance, caPath, pemBytes, inContainer)
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

// portNames lists the configured ports' names: the default ones and any
// other --port, sorted.
func portNames(cfg *config.Config) []string {
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
	return names
}

// checkHostOnly runs the checks of goos's host: the loopback aliases on
// macOS and the excluded port ranges on Windows. In a Containerized
// Instance (inContainer) the host's OS is unknown and out of reach, so it
// notes what to check on each host instead.
func checkHostOnly(out io.Writer, cfg *config.Config, goos string, inContainer, running bool) {
	switch {
	case inContainer:
		fmt.Fprintf(out, "[--  ] host checks not run in a Containerized Instance; on the host: on a macOS host, `%s` with the native binary adds the loopback aliases; "+
			"on a Windows host, check the published ports and --port-range against `netsh int ipv4 show excludedportrange protocol=tcp`\n", loopback.InstallHint)
	case goos == "darwin":
		checkLoopback(out, loopback.Aliases())
	case goos == "windows":
		checkExcludedPorts(out, cfg, running)
	}
}

// checkExcludedPorts warns, on Windows, about ports in one of its
// excluded port ranges, which no address binds there (ADR 0015): the
// running Instance's forwarding rules' ports, which its --lb-bind
// addresses serve too, or else the configured ports.
func checkExcludedPorts(out io.Writer, cfg *config.Config, running bool) {
	ports := map[string]int{}
	if running {
		c, err := clientFor(cfg)
		if err != nil {
			return
		}
		b, err := c.get("/_emu/v1/lb/listeners")
		if err != nil {
			return
		}
		ports = forwardingRulePorts(b)
	} else {
		for _, n := range portNames(cfg) {
			if p := cfg.Port(n); p != 0 {
				ports["port "+n] = p
			}
		}
	}
	if len(ports) == 0 {
		return
	}
	tcp, err := loopback.ExcludedRanges("tcp")
	if err != nil {
		fmt.Fprintf(out, "[--  ] excluded port ranges: cannot read them: %v\n", err)
		return
	}
	reportExcludedPorts(out, "tcp", tcp, ports)
	if p := cfg.Port("dns"); !running && p != 0 {
		if udp, err := loopback.ExcludedRanges("udp"); err == nil {
			reportExcludedPorts(out, "udp", udp, map[string]int{"port dns": p})
		}
	}
}

// forwardingRulePorts returns the port of each forwarding rule in an lb
// listeners report, served or unavailable, by "forwarding rule <path>".
func forwardingRulePorts(body []byte) map[string]int {
	type rule struct {
		ForwardingRule string `json:"forwardingRule"`
		Port           int    `json:"port"`
	}
	var v struct{ Listeners, Unavailable []rule }
	ports := map[string]int{}
	if json.Unmarshal(body, &v) != nil {
		return ports
	}
	for _, r := range append(v.Listeners, v.Unavailable...) {
		ports["forwarding rule "+r.ForwardingRule] = r.Port
	}
	return ports
}

// reportExcludedPorts warns about each of ports (by what uses it) that one
// of ranges, network's excluded port ranges, holds.
func reportExcludedPorts(out io.Writer, network string, ranges []loopback.PortRange, ports map[string]int) {
	warned := false
	for _, what := range sortedKeys(ports) {
		r, ok := loopback.ExcludedPort(ranges, ports[what])
		if !ok {
			continue
		}
		fmt.Fprintf(out, "[WARN] %s (port %d) is in Windows' excluded port range %s, so nothing can listen on it\n", what, ports[what], r)
		warned = true
	}
	if warned {
		fmt.Fprintf(out, "       fix: use a port outside the ranges `netsh int ipv4 show excludedportrange protocol=%s` lists; Hyper-V, WSL and others reserve them\n", network)
	}
}

// checkPlatform reports goos/goarch and whether it is supported.
func checkPlatform(report func(bool, string, string), goos, goarch string) {
	platform := fmt.Sprintf("platform %s/%s", goos, goarch)
	report(goos == "linux" || goos == "darwin" || goos == "windows", platform, "run on Linux, macOS or Windows")
}

// checkVMRelay prints whether an Instance started now uses the VM relay
// (ADR 0005), and why. Its services network does not exist before it
// starts, so the runtime's default bridge network stands in for it. It
// reports whether the runtime is a VM runtime, if that was determined.
func checkVMRelay(ctx context.Context, out io.Writer, cl *emuruntime.Client) (vm, known bool) {
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
		return false, false
	}
	d := vmrelay.Decide(cl.Self() != "", gw)
	fmt.Fprintf(out, "[ok  ] VM relay: %s\n", d)
	return d.VM, true
}

// checkVMRuntime prints, for a Containerized Instance, whether it treats
// its runtime as a VM runtime and serves GKE LoadBalancer Services on its
// published ports (ADR 0012), and flags clusters created otherwise.
func checkVMRuntime(ctx context.Context, out io.Writer, cl *emuruntime.Client, cfg *config.Config, info emuruntime.Info) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// `start` may have had the settings as flags, which doctor lacks and
	// which win over env and file there.
	setting, portRange, aliasSetting := cfg.VMRuntime, cfg.PortRange, cfg.LoopbackAliases
	self, err := cl.InspectContainer(ctx, cl.Self())
	if err == nil {
		if v := startFlag(self.Cmd, "vm-runtime"); v != "" {
			setting = v
		}
		if v := startFlag(self.Cmd, "port-range"); v != "" {
			portRange = v
		}
		if v := startFlag(self.Cmd, "loopback-aliases"); v != "" {
			aliasSetting = v
		}
	}
	d := emuruntime.DecideVM(setting, true, info)
	aliases := emuruntime.DecideLoopbackAliases(aliasSetting, info)
	published, perr := cl.SelfPorts(ctx)
	if err = errors.Join(err, perr); err != nil {
		fmt.Fprintf(out, "[--  ] VM runtime: %s; cannot read this container's published ports: %v\n", d, err)
	} else {
		reportVMRuntime(out, d, aliases, portRange, published)
	}
	dir, err := filepath.Abs(cfg.InstanceDir())
	if err != nil {
		return
	}
	servers, err := cl.ListContainers(ctx, map[string]string{
		emuruntime.LabelService: "gke", emuruntime.LabelRole: "server", emuruntime.LabelDir: dir,
	})
	if err != nil {
		fmt.Fprintf(out, "[--  ] GKE clusters: cannot list them: %v\n", err)
		return
	}
	reportServiceLB(out, d.On, servers)
}

// checkUnpublished lists the forwarding rules of the running Instance that
// have no published port (ADR 0012), which only a Containerized Instance
// has, and the GCPEMU_LB_* variables withheld as ambiguous (ADR 0013); an
// Instance without the lb service reports neither. It also lists
// what keeps host clients from Kubernetes Services (ADR 0014), which an
// Instance without the gke service does not report.
func checkUnpublished(out io.Writer, cfg *config.Config) {
	c, err := clientFor(cfg)
	if err != nil {
		return
	}
	if b, err := c.get("/_emu/v1/lb/listeners"); err == nil {
		reportUnpublished(out, b)
	}
	if b, err := c.get("/_emu/v1/host/services"); err == nil {
		reportHostServices(out, b)
	}
	if b, err := c.get("/_emu/v1/sql/hosts"); err == nil {
		reportSQLHosts(out, b)
	}
}

// reportSQLHosts prints each public Cloud SQL instance's host address, and
// flags the instances that have none and a runtime that refused a
// loopback alias (ADR 0005, ADR 0012).
func reportSQLHosts(out io.Writer, body []byte) {
	var v struct {
		Instances []struct {
			Instance string `json:"instance"`
			Address  string `json:"address"`
			Missing  string `json:"missing"`
		} `json:"instances"`
		AliasesRefused bool `json:"aliasesRefused"`
	}
	if json.Unmarshal(body, &v) != nil {
		return
	}
	if v.AliasesRefused {
		fmt.Fprintln(out, "[WARN] the runtime refused a loopback alias, so Cloud SQL instances use only 127.0.0.1 and one at a time serves host clients")
		fmt.Fprintf(out, "       fix: only a macOS host needs loopback aliases added (`%s`, from the native binary), as Linux and Windows hosts already have them;\n", loopback.InstallHint)
		fmt.Fprintf(out, "       set %s=true unless the runtime is Docker Desktop, and restart the emulator\n", emuruntime.LoopbackAliasesEnv)
	}
	for _, in := range v.Instances {
		if in.Address == "" {
			fmt.Fprintf(out, "[WARN] Cloud SQL instance %s has no host address, so host clients cannot reach it: %s\n", in.Instance, in.Missing)
			continue
		}
		fmt.Fprintf(out, "[ok  ] Cloud SQL instance %s: host clients reach it on %s\n", in.Instance, in.Address)
	}
}

// reportUnpublished prints each forwarding rule of an lb listeners report
// that has no published port, with what to publish, and each GCPEMU_LB_*
// variable withheld because more than one rule would print it (ADR 0013),
// with the qualified variables to use instead.
func reportUnpublished(out io.Writer, body []byte) {
	var v struct {
		Listeners []struct {
			ForwardingRule string `json:"forwardingRule"`
			Port           int    `json:"port"`
			Unpublished    string `json:"unpublished"`
		} `json:"listeners"`
		Withheld []struct {
			Variable        string   `json:"variable"`
			ForwardingRules []string `json:"forwardingRules"`
			Qualified       []string `json:"qualified"`
		} `json:"withheld"`
	}
	if json.Unmarshal(body, &v) != nil {
		return
	}
	for _, l := range v.Listeners {
		if l.Unpublished != "" {
			fmt.Fprintf(out, "[WARN] forwarding rule %s (port %d) has no published port, so the host cannot reach it\n", l.ForwardingRule, l.Port)
			fmt.Fprintf(out, "       fix: %s\n", l.Unpublished)
		}
	}
	for _, w := range v.Withheld {
		fmt.Fprintf(out, "[WARN] `gcpemu env` does not print %s: forwarding rules %s share it\n", w.Variable, joinList(w.ForwardingRules, "and"))
		fix := "use " + joinList(w.Qualified, "or")
		if slices.Contains(w.Qualified, w.Variable) {
			// Their qualified variables flatten alike.
			fix = "rename one of the forwarding rules"
		}
		fmt.Fprintf(out, "       fix: %s\n", fix)
	}
}

// joinList joins items as "a, b and c", with conj for "and".
func joinList(items []string, conj string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " " + conj + " " + items[len(items)-1]
}

// startFlag returns the value of --name in a container's command, as
// "--name value" or "--name=value", or "".
func startFlag(cmd []string, name string) string {
	for i, a := range cmd {
		if v, ok := strings.CutPrefix(a, "--"+name+"="); ok {
			return v
		}
		if a == "--"+name && i+1 < len(cmd) {
			return cmd[i+1]
		}
	}
	return ""
}

// reportVMRuntime prints the VM-runtime decision d and, on a VM runtime,
// the loopback-alias decision for Cloud SQL and how many ports of
// portRange the container publishes, warning when none.
func reportVMRuntime(out io.Writer, d emuruntime.VMDecision, aliases emuruntime.AliasDecision, portRange string, published map[int]string) {
	fmt.Fprintf(out, "[ok  ] VM runtime: %s\n", d)
	if !d.On {
		return
	}
	fmt.Fprintf(out, "[ok  ] loopback aliases for Cloud SQL: %s\n", aliases)
	n := 0
	c := config.Config{PortRange: portRange}
	lo, hi, ranged, _ := c.ParsePortRange()
	for p := range published {
		if ranged && p >= lo && p <= hi {
			n++
		}
	}
	if n > 0 {
		fmt.Fprintf(out, "[ok  ] VM runtime: %d --port-range port(s) published for GKE LoadBalancer Services\n", n)
		return
	}
	fix := "start the image with --port-range 20000-20019 and publish it: -p 127.0.0.1:20000-20019:20000-20019"
	if ranged {
		fix = "publish the range: -p 127.0.0.1:" + portRange + ":" + portRange
	}
	fmt.Fprintln(out, "[WARN] VM runtime: no --port-range port is published, so the host cannot reach GKE LoadBalancer Services")
	fmt.Fprintf(out, "       fix: %s\n", fix)
}

// reportServiceLB flags the GKE clusters (server containers) whose
// servicelb setting does not match the VM-runtime decision vm: they were
// last started under another decision. Starting the emulator recreates
// their servers with the current one.
func reportServiceLB(out io.Writer, vm bool, servers []emuruntime.Container) {
	for _, ct := range servers {
		off := false
		for i := 1; i < len(ct.Cmd); i++ {
			off = off || ct.Cmd[i-1] == "--disable" && ct.Cmd[i] == "servicelb"
		}
		name := ct.Labels[emuruntime.LabelResource]
		switch {
		case vm && !off:
			fmt.Fprintf(out, "[WARN] GKE cluster %s runs servicelb, whose node addresses the host cannot reach on a VM runtime\n", name)
		case !vm && ct.Labels[emuruntime.LabelHostLB] == emuruntime.HostLBVM:
			fmt.Fprintf(out, "[WARN] GKE cluster %s was created for a VM runtime, without servicelb\n", name)
		default:
			continue
		}
		fmt.Fprintln(out, "       fix: restart the emulator with the "+emuruntime.VMRuntimeEnv+" setting you want; it recreates the cluster's server")
	}
}

// checkLoopback reports whether the host has the 127.0.0.x loopback
// aliases (ADR 0005). Missing ones are a note, not a failure: one Cloud SQL
// instance and the forwarding rules still work on 127.0.0.1.
func checkLoopback(out io.Writer, aliases bool) {
	if aliases {
		fmt.Fprintln(out, "[ok  ] loopback aliases 127.0.0.2-127.0.0.254 present")
		return
	}
	fmt.Fprintf(out, "[--  ] loopback aliases 127.0.0.2-127.0.0.254 missing: `%s` lets more than one Cloud SQL instance run, gives each load balancer forwarding rule its own address and lets host clients reach Kubernetes Services by name\n", loopback.InstallHint)
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
// load balancers and host mode need it. In a Containerized Instance
// (inContainer) trust is the host's, which the container cannot see, so
// it notes that the CA at caPath is to be trusted there.
func checkCATrust(out io.Writer, instance, caPath string, pemBytes []byte, inContainer bool) {
	if inContainer {
		fmt.Fprintf(out, "[--  ] emulator CA trust is the host's in a Containerized Instance: trust %s on the host, not in the container\n", caPath)
		return
	}
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

// checkLANBind reports the --lb-bind, --gke-bind, --ar-bind and --gfe-bind
// addresses: what the running instance serves on them, otherwise whether
// the configured ones can be bound (an address of this host; privileged
// --lb-bind ports need the lb-lan relay, and --gfe-bind's 443 an
// unprivileged port start at or below it, or the gfe-lan relay, which
// needs hostRuntime: a container runtime that is not a VM runtime, for an
// emulator not in a container). In a Containerized Instance (inContainer)
// the addresses and the sysctl are the container's.
func checkLANBind(out io.Writer, report func(bool, string, string), cfg *config.Config, running, hasRuntime, hostRuntime, inContainer bool) {
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
			if name, ok := strings.CutPrefix(n, "gke-lan:"); ok {
				fmt.Fprintf(out, "[ok  ] Kubernetes API of %s exposed beyond loopback on %s\n", name, i.Endpoints[n])
			}
			if n == "ar-lan" {
				fmt.Fprintf(out, "[ok  ] Artifact Registry registry exposed beyond loopback on %s (TLS, credentials required)\n", i.Endpoints[n])
			}
			if n == frontend.LANEndpoint {
				fmt.Fprintf(out, "[ok  ] Google front end exposed beyond loopback on %s (TLS, credentials required)\n", i.Endpoints[n])
			}
		}
		return
	}
	if len(cfg.LBBind)+len(cfg.GKEBind)+len(cfg.ARBind)+len(cfg.GFEBind) == 0 {
		return
	}
	local := map[string]bool{}
	var lan []string
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				local[ipn.IP.String()] = true
				if !ipn.IP.IsLoopback() {
					lan = append(lan, ipn.IP.String())
				}
			}
		}
	}
	whose, fix := "this host", "pick one from "+listAddrsCmd(runtime.GOOS)
	if inContainer {
		// The image has no `ip`, so the fix lists the addresses itself.
		whose, fix = "this container", "pick one of the container's addresses: "+strings.Join(lan, ", ")
		if len(lan) == 0 {
			fix = "attach the container to a network with an address beyond loopback (docker run --network)"
		}
	}
	for _, b := range []struct {
		flag  string
		addrs []string
	}{{"lb-bind", cfg.LBBind}, {"gke-bind", cfg.GKEBind}, {"ar-bind", cfg.ARBind}, {"gfe-bind", cfg.GFEBind}} {
		for _, a := range b.addrs {
			ip := net.ParseIP(a)
			report(ip != nil && local[ip.String()], fmt.Sprintf("--%s %s is an address of %s", b.flag, a, whose), fix)
		}
	}
	b, err := os.ReadFile("/proc/sys/net/ipv4/ip_unprivileged_port_start")
	if err != nil {
		return
	}
	start, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if _, set := cfg.PortSet(frontend.LANEndpoint); len(cfg.GFEBind) > 0 && !set {
		checkGFEPort(report, start, os.Geteuid() == 0, hostRuntime, inContainer)
	}
	if len(cfg.LBBind) > 0 {
		checkLBPort(report, start, hasRuntime, inContainer)
	}
}

// checkLBPort reports whether --lb-bind's ports below start, the
// unprivileged port start, are relayed by the lb-lan container, which
// needs hasRuntime.
func checkLBPort(report func(bool, string, string), start int, hasRuntime, inContainer bool) {
	if start <= 80 {
		return
	}
	fix := "install a container runtime, or let the emulator bind them: " + sysctlFix(80, inContainer)
	if inContainer {
		fix = "mount the container runtime's socket, or let the emulator bind them: " + sysctlFix(80, inContainer)
	}
	report(hasRuntime, fmt.Sprintf("--lb-bind ports below %d relayed by the lb-lan container", start), fix)
}

// sysctlFix is the command that lets the emulator bind ports from port
// on: on the host, or in a Containerized Instance (inContainer), where
// the sysctl is the container's network namespace's, when it starts.
func sysctlFix(port int, inContainer bool) string {
	if inContainer {
		return fmt.Sprintf("start the container with `docker run --sysctl net.ipv4.ip_unprivileged_port_start=%d`", port)
	}
	return fmt.Sprintf("sudo sysctl net.ipv4.ip_unprivileged_port_start=%d", port)
}

// checkGFEPort reports how --gfe-bind's port 443 is served on Linux,
// whose unprivileged port start is start: by the emulator, when it may bind
// it, otherwise by the gfe-lan relay (the LAN front-end relay) when there is
// a hostRuntime for it, otherwise not until the sysctl is lowered (the
// container's, in a Containerized Instance).
func checkGFEPort(report func(bool, string, string), start int, root, hostRuntime, inContainer bool) {
	if start > 443 && !root && hostRuntime {
		report(true, "--gfe-bind port 443 relayed by the gfe-lan container", "")
		return
	}
	report(start <= 443 || root, fmt.Sprintf("--gfe-bind port 443 can be bound (net.ipv4.ip_unprivileged_port_start is %d)", start),
		sysctlFix(443, inContainer))
}

// listAddrsCmd is the command that lists this host's addresses on goos.
func listAddrsCmd(goos string) string {
	switch goos {
	case "linux":
		return "`ip -br addr`"
	case "windows":
		return "`ipconfig`"
	}
	return "`ifconfig`"
}

// checkRuntimeDetection reports the runtime endpoint detection picked and
// why, the others it found, and any it skipped because it serves Windows
// containers.
func checkRuntimeDetection(out io.Writer, report func(ok bool, what, fix string), det emuruntime.Detection, err error) {
	if err != nil {
		fix := "install Docker Engine >= 24, Docker Desktop, OrbStack, Colima or Podman >= 4.9 (needed only for GKE, Cloud SQL and Cloud NAT)"
		switch {
		case errors.Is(err, emuruntime.ErrWindowsContainers) && det.Source != "":
			fix = "or choose a runtime serving Linux containers (unset DOCKER_HOST or DOCKER_CONTEXT, or `docker context use`)"
		case errors.Is(err, emuruntime.ErrWindowsContainers):
			fix = "" // the error says what to do
		case det.Source != "":
			fix = "start that runtime, or choose a running one (unset DOCKER_HOST or DOCKER_CONTEXT, or `docker context use`)"
		}
		report(false, "container runtime: "+err.Error(), fix)
	} else {
		report(true, fmt.Sprintf("container runtime %s (chosen by %s)", det.Endpoint, det.Source), "")
	}
	if others := slices.DeleteFunc(slices.Clone(det.Found), func(s string) bool {
		return s == det.Endpoint || slices.Contains(det.Skipped, s)
	}); len(others) > 0 {
		fmt.Fprintf(out, "       also found: %s\n", strings.Join(others, ", "))
	}
	for _, s := range det.Skipped {
		fmt.Fprintf(out, "[--  ] skipped %s: serves Windows containers\n", s)
	}
}

// checkRuntimeArch notes a container runtime whose architecture is not the
// host's. Everything still works, but on a VM runtime the VM is emulated,
// so every container runs several times slower.
func checkRuntimeArch(out io.Writer, goos, hostArch, rtArch string) {
	if rtArch == "" {
		return
	}
	if rtArch == hostArch {
		fmt.Fprintf(out, "[ok  ] container runtime architecture linux/%s matches the host\n", rtArch)
		return
	}
	if goos != "darwin" {
		fmt.Fprintf(out, "[--  ] container runtime is linux/%s but the host is %s/%s; Sidecar agents and images are pulled for linux/%s\n", rtArch, goos, hostArch, rtArch)
		return
	}
	fmt.Fprintf(out, "[--  ] container runtime VM is linux/%s but this Mac is %s: the VM is emulated, so every container runs slowly; "+
		"recreate it for %s (Colima: `colima delete`, then `colima start --arch %s`; Docker Desktop, OrbStack and Podman: install the build for this Mac)\n",
		rtArch, hostArch, hostArch, vmArch(hostArch))
}

// machineArch is the host's GOARCH. An amd64 build under Rosetta on an
// Apple silicon Mac reports arm64, the architecture a VM runs natively.
func machineArch() string {
	if runtime.GOOS == "darwin" && runtime.GOARCH == "amd64" {
		if b, err := exec.Command("sysctl", "-n", "sysctl.proc_translated").Output(); err == nil && strings.TrimSpace(string(b)) == "1" {
			return "arm64"
		}
	}
	return runtime.GOARCH
}

// vmArch is Colima's (QEMU's) name for a GOARCH.
func vmArch(goarch string) string {
	switch goarch {
	case "arm64":
		return "aarch64"
	case "amd64":
		return "x86_64"
	}
	return goarch
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

// checkAbandoned is doctor's read-only report of Abandoned Instance IDs;
// err is FindAbandoned's.
func checkAbandoned(out io.Writer, found []instance.Abandoned, err error) {
	if err != nil {
		fmt.Fprintf(out, "[--  ] abandoned Instance IDs: cannot list runtime objects: %v\n", err)
		return
	}
	if len(found) == 0 {
		fmt.Fprintln(out, "[ok  ] no abandoned Instance IDs on the container runtime")
		return
	}
	for _, a := range found {
		fmt.Fprintf(out, "[WARN] abandoned Instance ID %s: %d container(s), %d network(s), %d volume(s)%s\n",
			a.ID, len(a.Containers), len(a.Networks), len(a.Volumes), unverifiedNote(a))
	}
	fmt.Fprintln(out, "       fix: `gcpemu prune` lists and removes them; their networks can push new instances onto fallback ranges")
}

// findAbandoned is FindAbandoned with doctor's time limit.
func findAbandoned(ctx context.Context, cl *emuruntime.Client) ([]instance.Abandoned, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return instance.FindAbandoned(ctx, cl, config.StateHome())
}
