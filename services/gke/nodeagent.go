package gke

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/linuxuser586/gcpemu/internal/agent"
	"github.com/linuxuser586/gcpemu/internal/frontend"
	"github.com/linuxuser586/gcpemu/internal/trust"
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
	// nodes, the VPC egress gateway for private ones); NoDefaultRoute
	// removes it (private nodes without an egress gateway).
	DefaultVia     string `json:"defaultVia,omitempty"`
	NoDefaultRoute bool   `json:"noDefaultRoute,omitempty"`
	// PrivateVia routes the private address ranges through the VPC egress
	// gateway on public nodes, so pods reach private services access
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
	mdL, err := net.Listen("tcp", metadataIP+":80")
	if err != nil {
		return err
	}
	go func() { _ = http.Serve(mdL, metadataProxy(cfg)) }()
	regL, err := net.Listen("tcp", fmt.Sprintf("%s:%d", metadataIP, registryPort))
	if err != nil {
		return err
	}
	go func() { _ = http.Serve(regL, registryProxy(cfg)) }()
	if cfg.Frontend != "" {
		feL, err := net.Listen("tcp", metadataIP+":443")
		if err != nil {
			return err
		}
		go func() { _ = frontend.Relay(feL, cfg.Frontend) }()
	}
	if err := serveProvider(ctx, cfg.Emu); err != nil {
		return err
	}
	stopDNS, err := serveNodeDNS(cfg)
	if err != nil {
		return err
	}
	defer stopDNS()

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
	const root = "/sys/fs/cgroup"
	ctrl, err := os.ReadFile(root + "/cgroup.controllers")
	if err != nil {
		return nil // cgroup v1
	}
	if err := os.MkdirAll(root+"/init", 0o755); err != nil {
		return err
	}
	procs, _ := os.ReadFile(root + "/cgroup.procs")
	for _, pid := range strings.Fields(string(procs)) {
		_ = os.WriteFile(root+"/init/cgroup.procs", []byte(pid), 0)
	}
	var sub []string
	for _, c := range strings.Fields(string(ctrl)) {
		sub = append(sub, "+"+c)
	}
	if err := os.WriteFile(root+"/cgroup.subtree_control", []byte(strings.Join(sub, " ")), 0); err != nil {
		return fmt.Errorf("delegating cgroup controllers: %w", err)
	}
	return nil
}

// setupNodeNetwork adds the metadata address, fixes the default route,
// points registry hosts at the proxy and writes kubelet's resolv.conf.
func setupNodeNetwork(cfg nodeConfig) error {
	if out, err := exec.Command("ip", "addr", "add", metadataIP+"/32", "dev", "lo").CombinedOutput(); err != nil &&
		!strings.Contains(string(out), "File exists") {
		return fmt.Errorf("ip addr add: %v: %s", err, out)
	}
	if cfg.DefaultVia != "" {
		if out, err := exec.Command("ip", "route", "replace", "default", "via", cfg.DefaultVia).CombinedOutput(); err != nil {
			return fmt.Errorf("ip route replace default via %s: %v: %s", cfg.DefaultVia, err, out)
		}
	} else if cfg.NoDefaultRoute {
		_ = exec.Command("ip", "route", "del", "default").Run()
	}
	if cfg.PrivateVia != "" {
		for _, cidr := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
			if out, err := exec.Command("ip", "route", "replace", cidr, "via", cfg.PrivateVia).CombinedOutput(); err != nil {
				return fmt.Errorf("ip route replace %s via %s: %v: %s", cidr, cfg.PrivateVia, err, out)
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

// metadataProxy forwards metadata requests to the emulator, tagged with
// the caller's address. It does not add X-Forwarded-For (the metadata
// server rejects requests that carry it, like GCE's).
func metadataProxy(cfg nodeConfig) http.Handler {
	base, _ := url.Parse(cfg.Emu)
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			src, _, _ := net.SplitHostPort(pr.In.RemoteAddr)
			pr.Out.URL.Scheme = base.Scheme
			pr.Out.URL.Host = base.Host
			pr.Out.URL.Path = base.Path + "/md/" + src + pr.In.URL.Path
			pr.Out.URL.RawPath = ""
			pr.Out.Host = base.Host
			pr.Out.Header.Del("Authorization")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "metadata server unavailable: "+err.Error(), http.StatusServiceUnavailable)
		},
	}
}

// registryProxy forwards registry requests to Artifact Registry with the
// node service account's access token.
func registryProxy(cfg nodeConfig) http.Handler {
	if cfg.Registry == "" {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "Artifact Registry is not running", http.StatusServiceUnavailable)
		})
	}
	ts := &nodeTokenSource{url: cfg.Emu + "/token"}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = cfg.Registry
			pr.Out.Host = cfg.Registry
			if pr.Out.Header.Get("Authorization") == "" {
				if tok := ts.token(); tok != "" {
					pr.Out.Header.Set("Authorization", "Bearer "+tok)
				}
			}
		},
	}
}

// nodeTokenSource caches the node service account's access token.
type nodeTokenSource struct {
	url string
	mu  sync.Mutex
	tok string
	exp time.Time
}

func (t *nodeTokenSource) token() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tok != "" && time.Now().Before(t.exp) {
		return t.tok
	}
	resp, err := http.Get(t.url)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var v struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&v) != nil {
		return ""
	}
	t.tok = v.AccessToken
	t.exp = time.Now().Add(time.Duration(max(v.ExpiresIn-60, 30)) * time.Second)
	return t.tok
}

// serveNodeDNS relays DNS to the emulator over UDP and TCP. It answers
// metadata.google.internal and the frontend's hostnames itself.
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
	return frontend.ListenDNS(metadataIP+":53", h)
}

// nodeDNSAnswer returns the names the node relay answers with addr.
func nodeDNSAnswer(cfg nodeConfig, addr net.IP) func(string) net.IP {
	hosts := map[string]bool{}
	for _, h := range cfg.FrontendHosts {
		hosts[frontend.Normalize(h)] = true
	}
	return func(name string) net.IP {
		switch {
		case name == "metadata.google.internal" || name == "metadata":
			return addr
		case cfg.Frontend != "" && (hosts[name] || frontend.IsRegistryHost(name)):
			return addr
		}
		return nil
	}
}
