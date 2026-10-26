package gke

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	mdns "github.com/miekg/dns"

	"github.com/linuxuser586/gcpemu/internal/agent"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/frontend"
	"github.com/linuxuser586/gcpemu/internal/trust"
	"github.com/linuxuser586/gcpemu/internal/workload"
)

// The gke-node agent is the entrypoint of every node container. It
// prepares the node's networking, serves the node-local endpoints on
// 169.254.169.254 and then runs the k3s agent:
//
//   - :80   GKE metadata server: requests are forwarded to the emulator
//     tagged with the caller's (pod) IP, which resolves the pod's
//     Kubernetes service account to its Workload Identity (FR-GKE-005).
//   - :53   DNS relay to the emulated Cloud DNS, used as the upstream of
//     CoreDNS (FR-INT-005); it answers metadata.google.internal itself.
//   - :5000 registry proxy to emulated Artifact Registry, adding the node
//     service account's token (FR-INT-006); containerd mirrors every
//     LOCATION-docker.pkg.dev host to it.
//   - :443  TCP relay to the emulator's Google frontend (internal/frontend),
//     which terminates TLS for real hostnames. The DNS relay answers the
//     hostnames the emulator serves (storage.googleapis.com,
//     pubsub.googleapis.com, LOCATION-docker.pkg.dev, ...) with
//     169.254.169.254, so unmodified clients in pods reach emulated
//     services by real hostname (FR-INT-007). Names the emulator does not
//     serve (logging.googleapis.com, ...) are relayed to the emulated
//     Cloud DNS and resolve as usual.
//   - :4317 (OTLP/gRPC) and :4318 (OTLP/HTTP), relayed to the emulator's
//     managed collector tagged with the caller's (pod) IP (collector.go,
//     FR-GKE-019); the opentelemetry-collector Service in
//     gke-managed-otel names metadata.google.internal.
//   - 169.254.169.253:53 the googleapis resolver (googleAPIsDNSIP) for pods
//     labelled gcpemu.dev/googleapis=real (cainject.go, ADR 0019).
//   - :15090 on every address, the ClusterIP relay (clusteriprelay.go), through
//     which the emulator connects host clients to Services' ClusterIPs
//     (ADR 0014).

// googleAPIsDNSIP is the node's googleapis resolver: *.googleapis.com
// resolves as on GCP (through Cloud DNS, to real Google) and every other
// name through the cluster's DNS. Pods labelled gcpemu.dev/googleapis=real
// use it (cainject.go, ADR 0019).
const googleAPIsDNSIP = "169.254.169.253"

const (
	nodeAgentName  = "gke-node"
	nodeConfigEnv  = "GCPEMU_GKE_NODE"
	nodeResolvConf = "/etc/gcpemu/resolv.conf"
)

// nodeConfig is passed to the agent as JSON in GCPEMU_GKE_NODE.
type nodeConfig struct {
	// Emu is the emulator's per-node endpoint base URL.
	Emu string `json:"emu"`
	// DNS and Registry are emulator endpoints on the services network.
	DNS      string `json:"dns,omitempty"`
	Registry string `json:"registry,omitempty"`
	// Network is the node's VPC network ("projects/P/global/networks/N");
	// the DNS relay tags queries with it so that private zones bound to
	// the network resolve (FR-DNS-004, FR-INT-005).
	Network string `json:"network,omitempty"`
	// DefaultVia replaces the default route (external gateway for public
	// nodes, the VPC's NAT egress proxy for private ones); NoDefaultRoute
	// removes it (private nodes without a NAT egress proxy).
	DefaultVia     string `json:"defaultVia,omitempty"`
	NoDefaultRoute bool   `json:"noDefaultRoute,omitempty"`
	// PrivateVia routes the private address ranges through the VPC's NAT egress
	// proxy on public nodes, so pods reach private services access
	// ranges (Cloud SQL private IP, FR-INT-008) like on GCP; connected
	// and pod routes stay more specific.
	PrivateVia string `json:"privateVia,omitempty"`
	// MirrorHosts are the registry hosts served by the registry proxy;
	// they resolve to the proxy so that pulls never reach Google.
	MirrorHosts []string `json:"mirrorHosts,omitempty"`
	// Frontend is the emulator's Google frontend on the services network
	// (TLS for real hostnames, FR-INT-007); the node relays
	// 169.254.169.254:443 to it.
	Frontend string `json:"frontend,omitempty"`
	// FrontendHosts are the real hostnames the frontend serves (the
	// gateway's mounted hosts); the DNS relay answers them, and every
	// LOCATION-docker.pkg.dev, with 169.254.169.254.
	FrontendHosts []string `json:"frontendHosts,omitempty"`
	// CA is the emulator's root CA (PEM), added to the node's system
	// trust store (Section 7.4: GKE nodes trust it automatically).
	CA string `json:"ca,omitempty"`
	// VPC is set when the node's subnetwork is Translated: the agent
	// aliases the node's Declared address IP onto its VPC interface
	// before k3s starts, and routes via the VPC's NAT egress proxy go via
	// the proxy's Declared address, sourced from IP (ADR 0004).
	VPC *emu.SubnetNet `json:"vpc,omitempty"`
	IP  string         `json:"ip,omitempty"`
	// Runtime names the container runtime, for errors about its kernel.
	Runtime string `json:"runtime,omitempty"`
	// Secret and ServiceCIDR configure the ClusterIP relay (clusteriprelay.go):
	// the cluster's secret, and the Service range it connects to. The
	// googleapis resolver finds the cluster's DNS in ServiceCIDR too.
	Secret      string `json:"secret,omitempty"`
	ServiceCIDR string `json:"serviceCidr,omitempty"`
	// Allocatable is what an Autopilot node advertises, whatever the host
	// has: the agent reserves the rest of the host for the kubelet (ADR
	// 0031).
	Allocatable *nodeAllocatable `json:"allocatable,omitempty"`
}

func init() { agent.Register(nodeAgentName, runNodeAgent) }

func runNodeAgent(ctx context.Context, args []string) error {
	var cfg nodeConfig
	if err := json.Unmarshal([]byte(os.Getenv(nodeConfigEnv)), &cfg); err != nil {
		return fmt.Errorf("bad %s: %w", nodeConfigEnv, err)
	}
	if err := evacuateCgroup(); err != nil {
		return err
	}
	// CSI drivers (the Secret Manager add-on) mount into pods with
	// Bidirectional propagation, which needs shared mounts.
	if out, err := exec.Command("mount", "--make-rshared", "/").CombinedOutput(); err != nil {
		return fmt.Errorf("make / rshared: %v: %s", err, out)
	}
	if err := setupNodeNetwork(cfg); err != nil {
		return err
	}
	if err := bridgeNetfilter(); err != nil {
		fmt.Fprintf(os.Stderr, "gke-node: pods may not reach Services on their own node: %v\n", err)
	}
	mdL, err := net.Listen("tcp", metadataIP+":80")
	if err != nil {
		return err
	}
	go func() { _ = http.Serve(mdL, workload.MetadataProxy(cfg.Emu)) }()
	regL, err := net.Listen("tcp", fmt.Sprintf("%s:%d", metadataIP, registryPort))
	if err != nil {
		return err
	}
	ts := &workload.TokenSource{URL: cfg.Emu + "/token"}
	go func() {
		_ = http.Serve(regL, workload.RegistryProxy(cfg.Registry, func(*http.Request) string { return ts.Token() }))
	}()
	if cfg.Frontend != "" {
		feL, err := net.Listen("tcp", metadataIP+":443")
		if err != nil {
			return err
		}
		go func() { _ = frontend.Relay(feL, cfg.Frontend) }()
	}
	otlpHTTPL, err := net.Listen("tcp", fmt.Sprintf("%s:%d", metadataIP, otlpHTTPPort))
	if err != nil {
		return err
	}
	otlpGRPCL, err := net.Listen("tcp", fmt.Sprintf("%s:%d", metadataIP, otlpGRPCPort))
	if err != nil {
		return err
	}
	defer serveNodeOTLP(otlpHTTPL, otlpGRPCL, cfg.Emu)()
	if _, cidr, err := net.ParseCIDR(cfg.ServiceCIDR); err == nil && cfg.Secret != "" {
		svcL, err := net.Listen("tcp", fmt.Sprintf(":%d", clusterIPRelayPort))
		if err != nil {
			return err
		}
		go serveClusterIPRelay(svcL, cfg.Secret, cidr)
	}
	if err := serveProvider(ctx, cfg.Emu); err != nil {
		return err
	}
	stopDNS, err := serveNodeDNS(cfg)
	if err != nil {
		return err
	}
	defer stopDNS()

	if cfg.Allocatable != nil {
		reserve, err := reserveArgs(*cfg.Allocatable)
		if err != nil {
			return err
		}
		args = append(args, reserve...)
	}
	cmd := exec.Command("/bin/k3s", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			_ = cmd.Process.Kill()
		}
		return nil
	}
}

// evacuateCgroup moves every process of the container out of the root
// cgroup (v2) and delegates all controllers, so kubelet can create its
// kubepods hierarchy. k3s does this itself only when it runs as PID 1.
func evacuateCgroup() error {
	return evacuate("/sys/fs/cgroup", os.WriteFile)
}

// evacuate is evacuateCgroup on the cgroup root, writing with write.
// A process that joins root meanwhile (the emulator's exec into a node
// that just started, #230) makes delegating fail with EBUSY: cgroup v2
// enables no controllers on a cgroup with processes. Move it and retry.
func evacuate(root string, write func(string, []byte, os.FileMode) error) error {
	ctrl, err := os.ReadFile(root + "/cgroup.controllers")
	if err != nil {
		return nil // cgroup v1
	}
	if err := os.MkdirAll(root+"/init", 0o755); err != nil {
		return err
	}
	var sub []string
	for _, c := range strings.Fields(string(ctrl)) {
		sub = append(sub, "+"+c)
	}
	for try := 0; ; try++ {
		procs, _ := os.ReadFile(root + "/cgroup.procs")
		for _, pid := range strings.Fields(string(procs)) {
			_ = write(root+"/init/cgroup.procs", []byte(pid), 0)
		}
		err = write(root+"/cgroup.subtree_control", []byte(strings.Join(sub, " ")), 0)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EBUSY) || try == 50 {
			return fmt.Errorf("delegating cgroup controllers: %w", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// setupNodeNetwork adds the metadata and googleapis resolver addresses,
// fixes the default route, points registry hosts at the proxy and writes
// kubelet's resolv.conf.
func setupNodeNetwork(cfg nodeConfig) error {
	for _, ip := range []string{metadataIP, googleAPIsDNSIP} {
		if out, err := exec.Command("ip", "addr", "add", ip+"/32", "dev", "lo").CombinedOutput(); err != nil &&
			!strings.Contains(string(out), "File exists") {
			return fmt.Errorf("ip addr add: %v: %s", err, out)
		}
	}
	private := []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}
	defaultVia, privateVia := cfg.DefaultVia, cfg.PrivateVia
	if cfg.VPC != nil && cfg.VPC.Translated {
		// Routes via the VPC's NAT egress proxy go via its Declared address,
		// sourced from the node's (ADR 0004).
		cip, err := addrIn(cfg.VPC.ContainerSubnet)
		if err != nil {
			return err
		}
		var routes []string
		if defaultVia != "" && inCIDR(cfg.VPC.ContainerSubnet, defaultVia) {
			routes, defaultVia = append(routes, "default"), ""
		}
		if privateVia != "" && inCIDR(cfg.VPC.ContainerSubnet, privateVia) {
			routes, privateVia = append(routes, private...), ""
		}
		if out, err := exec.Command("sh", "-c", cfg.VPC.AliasScript(cfg.IP, cip, routes...)).CombinedOutput(); err != nil {
			return fmt.Errorf("aliasing %s: %w", cfg.IP, cfg.VPC.AliasError(cfg.Runtime, string(out), fmt.Errorf("%v: %s", err, out)))
		}
	}
	if defaultVia != "" {
		if out, err := exec.Command("ip", "route", "replace", "default", "via", defaultVia).CombinedOutput(); err != nil {
			return fmt.Errorf("ip route replace default via %s: %v: %s", defaultVia, err, out)
		}
	} else if cfg.NoDefaultRoute {
		_ = exec.Command("ip", "route", "del", "default").Run()
	}
	if privateVia != "" {
		for _, cidr := range private {
			if out, err := exec.Command("ip", "route", "replace", cidr, "via", privateVia).CombinedOutput(); err != nil {
				return fmt.Errorf("ip route replace %s via %s: %v: %s", cidr, privateVia, err, out)
			}
		}
	}
	hosts := "\n# gcpemu\n" + metadataIP + " metadata.google.internal metadata\n"
	for _, h := range cfg.MirrorHosts {
		hosts += metadataIP + " " + h + "\n"
	}
	f, err := os.OpenFile("/etc/hosts", os.O_APPEND|os.O_WRONLY, 0)
	if err == nil {
		_, _ = f.WriteString(hosts)
		f.Close()
	}
	if err := os.MkdirAll("/etc/gcpemu", 0o755); err != nil {
		return err
	}
	if err := trustNodeCA(cfg.CA); err != nil {
		return err
	}
	return os.WriteFile(nodeResolvConf, []byte("nameserver "+metadataIP+"\n"), 0o644)
}

// bridgeNetfilter makes bridged traffic pass iptables, so that a pod's
// reply to another pod on the node that reached it through a Service
// (CoreDNS above all) is un-DNATed. It needs br_netfilter, which the node
// cannot modprobe; adding a physdev rule loads it through the kernel, as
// kube-router's rules do when NetworkPolicy is enforced (FR-GKE-011).
func bridgeNetfilter() error {
	const ipt, chain = "/bin/aux/iptables", "GCPEMU-BRNF"
	_ = exec.Command(ipt, "-N", chain).Run()
	out, err := exec.Command(ipt, "-A", chain, "-m", "physdev", "--physdev-is-bridged", "-j", "RETURN").CombinedOutput()
	_ = exec.Command(ipt, "-F", chain).Run()
	_ = exec.Command(ipt, "-X", chain).Run()
	if err != nil {
		return fmt.Errorf("physdev rule: %v: %s", err, out)
	}
	return os.WriteFile("/proc/sys/net/bridge/bridge-nf-call-iptables", []byte("1"), 0o644)
}

// trustNodeCA adds the emulator CA to the node's system bundle (read by
// k3s, containerd and kubelet) and to /usr/local/share/ca-certificates.
func trustNodeCA(caPEM string) error {
	if caPEM == "" {
		return nil
	}
	const bundle = "/etc/ssl/certs/ca-certificates.crt"
	cur, err := os.ReadFile(bundle)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll("/etc/ssl/certs", 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(bundle, trust.Append(cur, []byte(caPEM)), 0o644); err != nil {
		return fmt.Errorf("trusting the emulator CA: %w", err)
	}
	if err := os.MkdirAll("/usr/local/share/ca-certificates", 0o755); err == nil {
		_ = os.WriteFile("/usr/local/share/ca-certificates/gcpemu.crt", []byte(caPEM), 0o644)
	}
	return nil
}

// serveNodeDNS relays DNS to the emulator over UDP and TCP. It answers
// metadata.google.internal and the frontend's hostnames itself. It also
// serves the googleapis resolver when the cluster's DNS address is known.
func serveNodeDNS(cfg nodeConfig) (func(), error) {
	upstream := cfg.DNS
	if upstream == "" {
		upstream = "127.0.0.11:53" // Docker's embedded resolver
	}
	md := net.ParseIP(metadataIP)
	h := &frontend.DNS{
		TTL:       300,
		Answer:    nodeDNSAnswer(cfg, md),
		Upstreams: func(string) []string { return []string{upstream} },
		Network:   cfg.Network,
	}
	stop, err := frontend.ListenDNS(metadataIP+":53", h)
	if err != nil {
		return nil, err
	}
	kubeDNS, err := clusterDNS(cfg.ServiceCIDR)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gke-node: pods labelled %s=real resolve nothing: %v\n", googleAPIsLabel, err)
		return stop, nil
	}
	stopGoogleAPIs, err := frontend.ListenDNS(googleAPIsDNSIP+":53", googleAPIsDNS(upstream, net.JoinHostPort(kubeDNS, "53"), cfg.Network))
	if err != nil {
		stop()
		return nil, err
	}
	return func() { stopGoogleAPIs(); stop() }, nil
}

// nodeDNSAnswer returns the names the node relay answers with addr.
func nodeDNSAnswer(cfg nodeConfig, addr net.IP) func(string) net.IP {
	return workload.DNSAnswer(cfg.FrontendHosts, cfg.Frontend != "", addr)
}

// addrIn returns this host's IPv4 address inside cidr.
func addrIn(cidr string) (string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && inCIDR(cidr, ipn.IP.String()) {
			return ipn.IP.String(), nil
		}
	}
	return "", fmt.Errorf("no interface has an address in %s", cidr)
}

func inCIDR(cidr, ip string) bool {
	_, n, err := net.ParseCIDR(cidr)
	return err == nil && n.Contains(net.ParseIP(ip))
}

// clusterDNS returns the cluster DNS (kube-dns) address k3s picks in
// serviceCIDR when, as in startServer (dataplane.go), it is given no
// --cluster-dns: the range's 10th address.
func clusterDNS(serviceCIDR string) (string, error) {
	_, n, err := net.ParseCIDR(serviceCIDR)
	if err != nil {
		return "", err
	}
	ip := n.IP.To4()
	if ip == nil {
		return "", fmt.Errorf("service range %s is not IPv4", serviceCIDR)
	}
	v := uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3]) + 10
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v)).String(), nil
}

// googleAPIsDNS is the googleapis resolver's handler: *.googleapis.com
// goes to Cloud DNS (cloud) tagged with the node's VPC network, which
// resolves it to real Google as on GCP, and every other name to the
// cluster's DNS (cluster), whose upstream is the node relay.
func googleAPIsDNS(cloud, cluster, network string) mdns.Handler {
	google := &frontend.DNS{Upstreams: func(string) []string { return []string{cloud} }, Network: network}
	rest := &frontend.DNS{Upstreams: func(string) []string { return []string{cluster} }}
	return mdns.HandlerFunc(func(w mdns.ResponseWriter, req *mdns.Msg) {
		if len(req.Question) == 1 {
			if n := frontend.Normalize(req.Question[0].Name); n == "googleapis.com" || strings.HasSuffix(n, ".googleapis.com") {
				google.ServeDNS(w, req)
				return
			}
		}
		rest.ServeDNS(w, req)
	})
}

// reserveArgs returns the k3s agent arguments that leave alloc allocatable
// on this host: the kubelet reserves the rest of the CPU and memory the
// kubelet sees, and evicts at 100 MiB of memory left as on GKE (k3s sets
// no memory threshold). A host smaller than alloc reserves nothing, so
// its node advertises less (ADR 0031).
func reserveArgs(alloc nodeAllocatable) ([]string, error) {
	cpus, err := onlineCPUs()
	if err != nil {
		return nil, err
	}
	memKi, err := memTotalKi()
	if err != nil {
		return nil, err
	}
	cpu := max(cpus*1000-alloc.CPUMilli, 0)
	mem := max(memKi-alloc.MemoryKi-evictionKi, 0)
	if cpu == 0 || mem == 0 {
		fmt.Fprintf(os.Stderr, "gke-node: the host (%d CPUs, %d KiB) is smaller than the node's machine shape; the node advertises less\n", cpus, memKi)
	}
	return []string{
		fmt.Sprintf("--kubelet-arg=kube-reserved=cpu=%dm,memory=%dKi", cpu, mem),
		"--kubelet-arg=eviction-hard=memory.available<100Mi,nodefs.available<5%,imagefs.available<5%",
	}, nil
}

// onlineCPUs counts the host's online CPUs, as the kubelet does.
func onlineCPUs() (int64, error) {
	b, err := os.ReadFile("/sys/devices/system/cpu/online")
	if err != nil {
		return 0, err
	}
	var n int64
	for _, r := range strings.Split(strings.TrimSpace(string(b)), ",") {
		lo, hi, found := strings.Cut(r, "-")
		a, err := strconv.ParseInt(lo, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("online CPUs %q: %w", b, err)
		}
		last := a
		if found {
			if last, err = strconv.ParseInt(hi, 10, 64); err != nil {
				return 0, fmt.Errorf("online CPUs %q: %w", b, err)
			}
		}
		n += last - a + 1
	}
	return n, nil
}

// memTotalKi is the host's memory in KiB, from /proc/meminfo as the
// kubelet reads it.
func memTotalKi() (int64, error) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "MemTotal:"); ok {
			return strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
		}
	}
	return 0, errors.New("no MemTotal in /proc/meminfo")
}
