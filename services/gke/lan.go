package gke

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/netplane"
)

// --gke-bind (Config.GKEBind): every cluster's Kubernetes API server is
// also served on each configured address, on a port of its own that is
// kept across restarts when it is free (clusterInternal.LANPort), and
// recorded as endpoint "gke-lan:<context name>".
//
// The emulator relays TCP there, so TLS ends at the API server: its
// certificate names every --gke-bind address (--tls-san), and client
// certificates and service account tokens reach it unchanged. k3s turns
// anonymous requests away. So that no other credential gets in either,
// the TokenReview webhook stops mapping unknown tokens to the default
// principal on an Instance with --gke-bind, at every IAM enforcement
// level (serveTokenReview).
//
// <instance-dir>/kubeconfig-lan, and GET /container/_emu/kubeconfig?lan=
// true (or lan=ADDRESS), point at the first (or that) address, for use on
// another machine.

// lanKubeconfigFile is the kubeconfig for another machine.
const lanKubeconfigFile = "kubeconfig-lan"

// lanStrict reports whether the API servers must accept only real
// credentials, because they are reachable beyond this host.
func (s *Service) lanStrict() bool { return len(s.env.Config.GKEBind) > 0 }

// lanSANs are the --tls-san arguments that cover the --gke-bind addresses.
func (s *Service) lanSANs() []string {
	var args []string
	for _, ip := range s.env.Config.GKEBind {
		args = append(args, "--tls-san", ip)
	}
	return args
}

// serveLAN serves the API server at target on every --gke-bind address,
// trying port first, and returns the port to try next time. An address it
// cannot serve is logged and skipped. It replaces the cluster's previous
// listeners.
func (s *Service) serveLAN(ctx context.Context, d *deps, key, cluster string, port int, target string) int {
	rt := s.rtFor(key)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	s.closeLAN(rt)
	binds := s.env.Config.GKEBind
	if len(binds) == 0 {
		return port
	}
	for _, ip := range binds {
		ex, err := d.np.Expose(net.JoinHostPort(ip, strconv.Itoa(port)), target, true)
		if err != nil {
			s.env.Log.Warn("gke: cannot serve the Kubernetes API on --gke-bind address", "cluster", cluster, "address", ip, "err", s.env.ExplainBind(ctx, err))
			continue
		}
		if len(rt.lan) == 0 {
			_, port = netplane.HostPort(ex.Addr())
		}
		rt.lan = append(rt.lan, ex)
		rt.lanAddrs = append(rt.lanAddrs, ex.Addr())
	}
	if len(rt.lanAddrs) > 0 {
		rt.lanName = "gke-lan:" + cluster
		s.env.Endpoints.Set(rt.lanName, strings.Join(rt.lanAddrs, ","))
		s.env.Log.Info("gke: Kubernetes API exposed", "cluster", cluster, "address", strings.Join(rt.lanAddrs, ","))
	}
	return port
}

// closeLAN stops serving the API server on the --gke-bind addresses.
// Callers hold rt.mu.
func (s *Service) closeLAN(rt *clusterRT) {
	for _, ex := range rt.lan {
		_ = ex.Close()
	}
	if rt.lanName != "" {
		s.env.Endpoints.Delete(rt.lanName)
	}
	rt.lan, rt.lanAddrs, rt.lanName = nil, nil, ""
}

// lanServer returns the cluster's address on the --gke-bind address ip.
// Callers hold rt.mu.
func (rt *clusterRT) lanServer(ip string) (string, bool) {
	for _, a := range rt.lanAddrs {
		if h, _, _ := net.SplitHostPort(a); h == ip {
			return a, true
		}
	}
	return "", false
}

// forCaller returns c, the cluster stored under key, as the caller in ctx
// is told it. A LAN caller, over a link (ADR 0027), the Google front end
// or a --bind address beyond loopback, is told the --gke-bind address of
// its API server (emu.LANServed) as every endpoint, since the loopback
// addresses would be wrong on its machine; when it is served on none, no
// endpoint and a status message naming --gke-bind.
func (s *Service) forCaller(ctx context.Context, key string, c *containerpb.Cluster) *containerpb.Cluster {
	if c.Endpoint == "" {
		return c
	}
	rt := s.rtFor(key)
	rt.mu.Lock()
	served := slices.Clone(rt.lanAddrs)
	rt.mu.Unlock()
	addr, lan := emu.LANServed(ctx, served, s.lanIPs())
	if !lan {
		return c
	}
	c.Endpoint = addr
	if pc := c.PrivateClusterConfig; pc != nil {
		pc.PrivateEndpoint, pc.PublicEndpoint = addr, addr
	}
	if ipc := c.GetControlPlaneEndpointsConfig().GetIpEndpointsConfig(); ipc != nil {
		ipc.PrivateEndpoint, ipc.PublicEndpoint = addr, addr
	}
	if addr == "" && c.StatusMessage == "" {
		c.StatusMessage = "The Kubernetes API is not served on the LAN: start gcpemu with --gke-bind."
		if len(s.env.Config.GKEBind) > 0 {
			c.StatusMessage = "The Kubernetes API is not served on any --gke-bind address; gcpemu's log says why."
		}
	}
	return c
}

// lanIPs are the Instance's LAN addresses on which the API servers or the
// GKE API may be reached: the --gke-bind addresses, and --bind unless it is
// loopback.
func (s *Service) lanIPs() []string {
	cfg := s.env.Config
	ips := slices.Clip(cfg.GKEBind)
	if !config.IsLoopback(cfg.Bind) {
		ips = append(ips, cfg.Bind)
	}
	return ips
}

// lanAddress resolves the kubeconfig endpoint's lan parameter to a
// --gke-bind address: "" when absent.
func (s *Service) lanAddress(v string) (string, error) {
	binds := s.env.Config.GKEBind
	switch {
	case v == "" || v == "false" || v == "0":
		return "", nil
	case len(binds) == 0:
		return "", apierr.FailedPrecondition("the Kubernetes API is not served beyond loopback; start gcpemu with --gke-bind")
	case v == "true" || v == "1":
		return binds[0], nil
	case slices.Contains(binds, v):
		return v, nil
	}
	return "", apierr.InvalidArgument("lan=%s is not a --gke-bind address (%s)", v, strings.Join(binds, ", "))
}

// writeLANKubeconfig rewrites <instance-dir>/kubeconfig-lan, or removes
// it from an Instance that no longer has --gke-bind.
func (s *Service) writeLANKubeconfig() {
	p := filepath.Join(s.env.Config.InstanceDir(), lanKubeconfigFile)
	if len(s.env.Config.GKEBind) == 0 {
		_ = os.Remove(p)
		return
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, s.buildKubeconfig(nil, s.env.Config.GKEBind[0]), 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p)
}
