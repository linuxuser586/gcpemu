package build

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/linuxuser586/gcpemu/internal/agent"
	"github.com/linuxuser586/gcpemu/internal/frontend"
	"github.com/linuxuser586/gcpemu/internal/workload"
)

// The Build agent is the Sidecar agent that gives Build steps the
// environment a GKE pod gets from its node (services/gke/nodeagent.go),
// with the pieces it shares in internal/workload. One per Instance serves
// every Build: it is on the Build network, where steps reach it at its own
// address, and on the services network, where it reaches the emulator.
// Steps use it as their DNS server, and it serves:
//
//   - :80   the metadata server, forwarded to the emulator tagged with the
//     caller's address, which picks the step's Build and so its service
//     account (agentctl.go).
//   - :53   a DNS relay to the emulated Cloud DNS that answers
//     metadata.google.internal, the Google front end's hostnames and every
//     LOCATION-docker.pkg.dev with the agent's address.
//   - :443  a TCP relay to the Google front end, which terminates TLS for
//     those hostnames (FR-INT-007).
//   - :5000 the registry proxy to emulated Artifact Registry, adding the
//     calling step's Build service account's token, over plain HTTP. The
//     Build daemon uses it as its insecure registry (ADR 0016, daemon.go),
//     with the credentials steps give it.

const (
	agentName      = "build-agent"
	agentConfigEnv = "GCPEMU_BUILD_AGENT"
	// registryPort is the registry proxy's port at the agent's address.
	registryPort = 5000
)

// agentConfig is passed to the agent as JSON in GCPEMU_BUILD_AGENT.
type agentConfig struct {
	// Emu is the base URL of the agent's emulator endpoints.
	Emu string `json:"emu"`
	// DNS and Registry are emulator endpoints on the services network.
	DNS      string `json:"dns,omitempty"`
	Registry string `json:"registry,omitempty"`
	// Frontend is the Google front end on the services network, and
	// FrontendHosts the hostnames it serves.
	Frontend      string   `json:"frontend,omitempty"`
	FrontendHosts []string `json:"frontendHosts,omitempty"`
	// Subnet is the Build network's; the agent answers DNS with its
	// address in it.
	Subnet string `json:"subnet"`
}

func init() { agent.Register(agentName, runAgent) }

func runAgent(ctx context.Context, _ []string) error {
	var cfg agentConfig
	if err := json.Unmarshal([]byte(os.Getenv(agentConfigEnv)), &cfg); err != nil {
		return fmt.Errorf("bad %s: %w", agentConfigEnv, err)
	}
	ip, err := addrIn(cfg.Subnet)
	if err != nil {
		return err
	}
	regL, err := net.Listen("tcp", fmt.Sprintf(":%d", registryPort))
	if err != nil {
		return err
	}
	go func() { _ = http.Serve(regL, basicChallenge(workload.RegistryProxy(cfg.Registry, stepToken(cfg.Emu)))) }()
	if cfg.Frontend != "" {
		feL, err := net.Listen("tcp", ":443")
		if err != nil {
			return err
		}
		go func() { _ = frontend.Relay(feL, cfg.Frontend) }()
	}
	upstream := cfg.DNS
	if upstream == "" {
		upstream = "127.0.0.11:53" // the runtime's embedded resolver
	}
	stop, err := frontend.ListenDNS(":53", &frontend.DNS{
		TTL:       300,
		Answer:    workload.DNSAnswer(cfg.FrontendHosts, cfg.Frontend != "", ip),
		Upstreams: func(string) []string { return []string{upstream} },
	})
	if err != nil {
		return err
	}
	defer stop()
	// The metadata server listens last: the emulator waits for it to
	// know that the agent serves.
	mdL, err := net.Listen("tcp", ":80")
	if err != nil {
		return err
	}
	go func() { _ = http.Serve(mdL, workload.MetadataProxy(cfg.Emu)) }()
	<-ctx.Done()
	return nil
}

// basicChallenge turns Artifact Registry's Bearer challenges into Basic
// ones: their token endpoint is on the services network, which neither
// steps nor the Build daemon reach. The daemon then sends the credentials
// a step's docker CLI gave it with every request, which Artifact Registry
// accepts as they are.
func basicChallenge(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(&challengeWriter{ResponseWriter: w}, r)
	})
}

type challengeWriter struct{ http.ResponseWriter }

func (w *challengeWriter) WriteHeader(code int) {
	if code == http.StatusUnauthorized && strings.HasPrefix(w.Header().Get("WWW-Authenticate"), "Bearer ") {
		w.Header().Set("WWW-Authenticate", `Basic realm="gcpemu"`)
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *challengeWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// stepToken returns the access token of the calling step's Build service
// account, read from the metadata server as that step on every request:
// a step's address is reused by later steps, so it is never cached here
// (the IAM Service caches tokens by account).
func stepToken(emu string) func(*http.Request) string {
	return func(r *http.Request) string {
		src, _, _ := net.SplitHostPort(r.RemoteAddr)
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, emu+"/md/"+src+"/computeMetadata/v1/instance/service-accounts/default/token", nil)
		if err != nil {
			return ""
		}
		req.Header.Set("Metadata-Flavor", "Google")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return ""
		}
		defer resp.Body.Close()
		var v struct {
			AccessToken string `json:"access_token"`
		}
		if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&v) != nil {
			return ""
		}
		return v.AccessToken
	}
}

// addrIn returns this container's IPv4 address inside cidr.
func addrIn(cidr string) (net.IP, error) {
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("Build network subnet %q: %w", cidr, err)
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	for _, a := range addrs {
		if in, ok := a.(*net.IPNet); ok && n.Contains(in.IP) {
			return in.IP.To4(), nil
		}
	}
	return nil, fmt.Errorf("no address in %s", cidr)
}
