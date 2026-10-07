package hostmode

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/linuxuser586/gcpemu/internal/agent"
	"github.com/linuxuser586/gcpemu/internal/frontend"
)

// The "host-frontend" agent runs in the host-mode container.

const (
	agentName = "host-frontend"
	configEnv = "GCPEMU_HOSTMODE"
)

// agentConfig is passed as JSON in GCPEMU_HOSTMODE.
type agentConfig struct {
	// Frontend and Gateway are emulator endpoints on the services network.
	Frontend string `json:"frontend"`
	Gateway  string `json:"gateway"`
	// DNS is the emulated Cloud DNS on the services network (optional).
	DNS string `json:"dns,omitempty"`
	// Resolvers are the host's real upstream resolvers.
	Resolvers []string `json:"resolvers,omitempty"`
	// Hosts are the gateway's mounted hostnames.
	Hosts []string `json:"hosts"`
	// AnswerCIDR selects the container address served in DNS answers
	// (the external network's subnet, which the host reaches).
	AnswerCIDR string `json:"answerCIDR"`
}

func init() { agent.Register(agentName, runAgent) }

func runAgent(ctx context.Context, _ []string) error {
	var cfg agentConfig
	if err := json.Unmarshal([]byte(os.Getenv(configEnv)), &cfg); err != nil {
		return fmt.Errorf("bad %s: %w", configEnv, err)
	}
	ip, err := addrIn(cfg.AnswerCIDR)
	if err != nil {
		return err
	}
	for port, target := range map[string]string{"443": cfg.Frontend, "80": cfg.Gateway} {
		l, err := net.Listen("tcp", ":"+port)
		if err != nil {
			return err
		}
		defer l.Close()
		go func() { _ = frontend.Relay(l, target) }()
	}
	stop, err := frontend.ListenDNS(":53", agentDNS(cfg, ip))
	if err != nil {
		return err
	}
	defer stop()
	<-ctx.Done()
	return nil
}

// agentDNS answers served names with ip; see the package documentation.
func agentDNS(cfg agentConfig, ip net.IP) *frontend.DNS {
	return &frontend.DNS{
		Answer: func(name string) net.IP {
			if frontend.Serves(cfg.Hosts, name) {
				return ip
			}
			return nil
		},
		Upstreams: func(name string) []string {
			if cfg.DNS == "" || routed(name) {
				return cfg.Resolvers
			}
			return []string{cfg.DNS}
		},
	}
}

// routed reports whether name lies under a routed domain.
func routed(name string) bool {
	for _, d := range RoutedDomains {
		if name == d || strings.HasSuffix(name, "."+d) {
			return true
		}
	}
	return false
}

// addrIn returns this container's IPv4 address inside cidr.
func addrIn(cidr string) (net.IP, error) {
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("answer network %q: %w", cidr, err)
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
