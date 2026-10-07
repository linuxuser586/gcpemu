package gke

import (
	"context"
	"encoding/json"
	"errors"
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

	mdns "github.com/miekg/dns"

	"github.com/linuxuser586/gcpemu/internal/agent"
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
	return os.WriteFile(nodeResolvConf, []byte("nameserver "+metadataIP+"\n"), 0o644)
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

// serveNodeDNS relays DNS to the emulator over UDP and TCP.
func serveNodeDNS(cfg nodeConfig) (func(), error) {
	upstream := cfg.DNS
	if upstream == "" {
		upstream = "127.0.0.11:53" // Docker's embedded resolver
	}
	h := mdns.HandlerFunc(func(w mdns.ResponseWriter, req *mdns.Msg) {
		if len(req.Question) == 1 {
			q := req.Question[0]
			if n := strings.ToLower(q.Name); n == "metadata.google.internal." || n == "metadata." {
				m := new(mdns.Msg)
				m.SetReply(req)
				m.Authoritative = true
				if q.Qtype == mdns.TypeA {
					m.Answer = append(m.Answer, &mdns.A{
						Hdr: mdns.RR_Header{Name: q.Name, Rrtype: mdns.TypeA, Class: mdns.ClassINET, Ttl: 300},
						A:   net.ParseIP(metadataIP),
					})
				}
				_ = w.WriteMsg(m)
				return
			}
		}
		proto := "udp"
		if _, ok := w.RemoteAddr().(*net.TCPAddr); ok {
			proto = "tcp"
		}
		c := &mdns.Client{Net: proto, Timeout: 10 * time.Second}
		resp, _, err := c.Exchange(req, upstream)
		if err == nil && resp.Truncated && proto == "udp" {
			c.Net = "tcp"
			resp, _, err = c.Exchange(req, upstream)
		}
		if err != nil {
			m := new(mdns.Msg)
			m.SetRcode(req, mdns.RcodeServerFailure)
			_ = w.WriteMsg(m)
			return
		}
		_ = w.WriteMsg(resp)
	})
	addr := metadataIP + ":53"
	udp := &mdns.Server{Addr: addr, Net: "udp", Handler: h}
	tcp := &mdns.Server{Addr: addr, Net: "tcp", Handler: h}
	errc := make(chan error, 2)
	var started sync.WaitGroup
	started.Add(2)
	udp.NotifyStartedFunc = started.Done
	tcp.NotifyStartedFunc = started.Done
	go func() { errc <- udp.ListenAndServe() }()
	go func() { errc <- tcp.ListenAndServe() }()
	ok := make(chan struct{})
	go func() { started.Wait(); close(ok) }()
	select {
	case <-ok:
	case err := <-errc:
		return nil, fmt.Errorf("dns relay: %w", err)
	case <-time.After(5 * time.Second):
		return nil, errors.New("dns relay did not start")
	}
	return func() { _ = udp.Shutdown(); _ = tcp.Shutdown() }, nil
}
