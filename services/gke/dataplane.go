package gke

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/internal/agent"
	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/frontend"
	"github.com/linuxuser586/gcpemu/internal/netplane"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/internal/store"
	"github.com/linuxuser586/gcpemu/services/ar"
)

// Data plane: k3s server and node containers (Section 3.3).

const (
	k3sDataDir    = "/var/lib/rancher/k3s"
	metadataIP    = "169.254.169.254"
	registryPort  = 5000
	webhookDir    = "/etc/gcpemu"
	nodeReadyWait = 3 * time.Minute
	// Pod and service ranges used when the cluster names none: pod ranges
	// are unique per instance (10.64.0.0/16 upwards) so pod IPs stay
	// unambiguous for NEG backends; service ranges are cluster-local.
	defaultServiceCIDR = "34.118.224.0/20"
)

// clusterRT is a cluster's in-memory runtime state.
type clusterRT struct {
	mu     sync.Mutex
	kube   *kubeClient
	creds  adminCreds
	cancel context.CancelFunc // controllers (NEG sync)
	wi     sync.Map           // pod IP → wiEntry
}

func (rt *clusterRT) stop() {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.cancel != nil {
		rt.cancel()
		rt.cancel = nil
	}
}

func (rt *clusterRT) client() *kubeClient {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.kube
}

// rtFor returns (creating) the runtime state of a cluster.
func (s *Service) rtFor(key string) *clusterRT {
	s.mu.Lock()
	defer s.mu.Unlock()
	rt, ok := s.clusters[key]
	if !ok {
		rt = &clusterRT{}
		s.clusters[key] = rt
	}
	return rt
}

// kube returns an admin client for a running cluster.
func (s *Service) kube(key string) (*kubeClient, error) {
	if k := s.rtFor(key).client(); k != nil {
		return k, nil
	}
	return nil, apierr.FailedPrecondition("cluster %s is not running", key)
}

// deps bundles the runtime objects provisioning needs.
type deps struct {
	rt  *runtime.Manager
	np  *netplane.Plane
	vpc emu.VPC
	svc netplane.Net
}

func (s *Service) deps(ctx context.Context) (*deps, error) {
	if s.env.Containers == nil {
		return nil, apierr.FailedPrecondition("GKE needs a container runtime (Docker or Podman); none is configured")
	}
	np, err := s.env.Containers.Netplane(ctx)
	if err != nil {
		return nil, apierr.FailedPrecondition("GKE needs a container runtime (Docker or Podman): %v", err)
	}
	svc, err := np.Services(ctx)
	if err != nil {
		return nil, err
	}
	return &deps{rt: np.Runtime(), np: np, vpc: s.vpc(), svc: svc}, nil
}

func (s *Service) loadKey(key string) (*clusterRecord, error) {
	var rec *clusterRecord
	err := s.env.Store.View(func(tx store.Tx) error {
		var err error
		rec, err = getCluster(tx, key)
		return err
	})
	return rec, err
}

// shortID is the cluster's runtime object prefix.
func shortID(c *containerpb.Cluster) string {
	id := c.Id
	if len(id) > 10 {
		id = id[:10]
	}
	return trunc(c.Name, 20) + "-" + id
}

func (s *Service) labels(d *deps, key, role string) map[string]string {
	return d.rt.Labels("gke", key, role)
}

// provision brings a new cluster up and marks it RUNNING (or ERROR).
func (s *Service) provision(ctx context.Context, key string) error {
	err := s.bringUp(ctx, key)
	if err != nil {
		s.markError(key, err)
	}
	return err
}

// restore recreates a persisted cluster's containers after an emulator
// restart (FR-CORE-031). Volumes keep the Kubernetes state.
// The caller marks the cluster RECONCILING first (markRestoring).
func (s *Service) restore(ctx context.Context, key string) {
	if err := s.bringUp(ctx, key); err != nil && ctx.Err() == nil {
		s.env.Log.Warn("gke: restoring cluster failed", "cluster", key, "err", err)
		s.markError(key, err)
	}
}

// markRestoring reports a cluster whose containers are being recreated as
// RECONCILING until it is back.
func (s *Service) markRestoring(key string) {
	_ = s.updateCluster(key, func(rec *clusterRecord, c *containerpb.Cluster) error {
		if c.Status == containerpb.Cluster_RUNNING || c.Status == containerpb.Cluster_ERROR {
			c.Status = containerpb.Cluster_RECONCILING
		}
		return nil
	})
}

func (s *Service) markError(key string, err error) {
	if s.ctx.Err() != nil {
		return // shutting down; keep the persisted status
	}
	_ = s.updateCluster(key, func(rec *clusterRecord, c *containerpb.Cluster) error {
		if c.Status == containerpb.Cluster_STOPPING {
			return nil
		}
		c.Status = containerpb.Cluster_ERROR
		c.StatusMessage = apierr.From(err).Message
		for _, np := range c.NodePools {
			if np.Status != containerpb.NodePool_RUNNING {
				np.Status = containerpb.NodePool_ERROR
			}
		}
		return nil
	})
}

// bringUp starts the server and every recorded node, waits until they are
// ready and records the endpoints. It is idempotent.
func (s *Service) bringUp(ctx context.Context, key string) error {
	d, err := s.deps(ctx)
	if err != nil {
		return err
	}
	rec, err := s.loadKey(key)
	if err != nil {
		return err
	}
	c := rec.cluster()
	sn, err := d.vpc.SubnetNetwork(ctx, rec.Int.Subnetwork)
	if err != nil {
		return err
	}
	if rec.Int.PodCIDR == "" {
		if err := s.assignRanges(key, sn); err != nil {
			return err
		}
	}
	if err := s.startServer(ctx, d, key, sn, c.CurrentMasterVersion); err != nil {
		return err
	}
	s.installCAInjector(ctx, d, key)
	if err := s.startNodes(ctx, d, key, sn, ""); err != nil {
		return err
	}
	if err := s.waitNodes(ctx, key, ""); err != nil {
		return err
	}
	err = s.updateCluster(key, func(rec *clusterRecord, c *containerpb.Cluster) error {
		if c.Status != containerpb.Cluster_STOPPING {
			c.Status = containerpb.Cluster_RUNNING
			c.StatusMessage = ""
		}
		for _, np := range c.NodePools {
			np.Status = containerpb.NodePool_RUNNING
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.writeKubeconfig()
	s.startControllers(key)
	return nil
}

// assignRanges picks the cluster's pod and service ranges: the subnet's
// secondary ranges when named, the requested blocks, or defaults.
func (s *Service) assignRanges(key string, sn emu.SubnetNet) error {
	return s.env.Store.Update(func(tx store.Tx) error {
		rec, err := getCluster(tx, key)
		if err != nil {
			return err
		}
		c := rec.cluster()
		ip := c.IpAllocationPolicy
		pod, svc := "", ""
		if n := ip.GetClusterSecondaryRangeName(); n != "" {
			if pod = sn.SecondaryRanges[n]; pod == "" {
				return apierr.InvalidArgument("Secondary range %q does not exist in subnetwork %q.", n, rec.Int.Subnetwork)
			}
		}
		if n := ip.GetServicesSecondaryRangeName(); n != "" {
			if svc = sn.SecondaryRanges[n]; svc == "" {
				return apierr.InvalidArgument("Secondary range %q does not exist in subnetwork %q.", n, rec.Int.Subnetwork)
			}
		}
		if pod == "" {
			pod = firstNonEmpty(ip.GetClusterIpv4CidrBlock(), ip.GetClusterIpv4Cidr(), c.ClusterIpv4Cidr)
		}
		if svc == "" {
			svc = firstNonEmpty(ip.GetServicesIpv4CidrBlock(), ip.GetServicesIpv4Cidr(), defaultServiceCIDR)
		}
		if pod == "" || strings.HasPrefix(pod, "/") {
			used := map[string]bool{}
			for _, r := range listClusters(tx, "") {
				used[r.Int.PodCIDR] = true
			}
			for i := 64; i < 128; i++ {
				cand := fmt.Sprintf("10.%d.0.0/16", i)
				if !used[cand] {
					pod = cand
					break
				}
			}
		}
		for _, cidr := range []string{pod, svc} {
			if _, _, err := net.ParseCIDR(cidr); err != nil {
				return apierr.InvalidArgument("Invalid CIDR range %q.", cidr)
			}
		}
		rec.Int.PodCIDR, rec.Int.ServiceCIDR = pod, svc
		c.ClusterIpv4Cidr = pod
		c.ServicesIpv4Cidr = svc
		ip.ClusterIpv4Cidr, ip.ClusterIpv4CidrBlock = pod, pod
		ip.ServicesIpv4Cidr, ip.ServicesIpv4CidrBlock = svc, svc
		for _, np := range c.NodePools {
			if np.NetworkConfig == nil {
				np.NetworkConfig = &containerpb.NodeNetworkConfig{}
			}
			np.NetworkConfig.PodRange = ip.ClusterSecondaryRangeName
			np.NetworkConfig.PodIpv4CidrBlock = pod
		}
		rec.setCluster(c)
		return putCluster(tx, rec)
	})
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// privateEndpoint reports whether the control plane has no public endpoint.
func privateEndpoint(c *containerpb.Cluster) bool {
	if c.GetPrivateClusterConfig().GetEnablePrivateEndpoint() {
		return true
	}
	if ipc := c.GetControlPlaneEndpointsConfig().GetIpEndpointsConfig(); ipc != nil && ipc.EnablePublicEndpoint != nil && !*ipc.EnablePublicEndpoint {
		return true
	}
	return false
}

// privateNodes reports whether a pool's nodes lack external IPs.
func privateNodes(c *containerpb.Cluster, np *containerpb.NodePool) bool {
	if nc := np.GetNetworkConfig(); nc != nil && nc.EnablePrivateNodes != nil {
		return *nc.EnablePrivateNodes
	}
	if c.GetPrivateClusterConfig().GetEnablePrivateNodes() {
		return true
	}
	if nc := c.GetNetworkConfig(); nc != nil && nc.DefaultEnablePrivateNodes != nil {
		return *nc.DefaultEnablePrivateNodes
	}
	return false
}

// ---- server ----

// startServer (re)creates the control plane container running version and
// waits for the API server.
func (s *Service) startServer(ctx context.Context, d *deps, key string, sn emu.SubnetNet, version string) error {
	rec, err := s.loadKey(key)
	if err != nil {
		return err
	}
	c := rec.cluster()
	ver, ok := lookupVersion(version)
	if !ok {
		return apierr.FailedPrecondition("unknown version %q", version)
	}
	if err := d.rt.EnsureImage(ctx, ver.Image, s.env.Config.Offline); err != nil {
		return err
	}
	ip := rec.Int.ServerIP
	if ip == "" {
		if ip, err = d.vpc.AllocateIP(ctx, sn, key+"/control-plane"); err != nil {
			return err
		}
	}
	name := d.rt.Name("gke", shortID(c), "cp")
	if err := d.rt.RemoveContainer(ctx, name, true); err != nil {
		return err
	}
	if err := d.rt.CreateVolume(ctx, name, s.labels(d, key, "server")); err != nil {
		return err
	}
	gw, err := d.np.Addr(ctx, "gateway")
	if err != nil {
		return err
	}
	hooks := "http://" + gw + "/container/_emu/hooks/" + c.Id + "/" + rec.Int.Secret
	nets := []runtime.Attachment{{Network: sn.Name, IP: ip}, {Network: d.svc.Name}}
	var ext netplane.Net
	if !privateEndpoint(c) {
		if ext, err = d.np.External(ctx); err != nil {
			return err
		}
		nets = append(nets, runtime.Attachment{Network: ext.Name})
	}
	args := []string{
		"server", "--disable-agent", "--https-listen-port", "443",
		"--egress-selector-mode", "cluster",
		"--disable", "traefik", "--disable", "metrics-server",
		"--cluster-cidr", rec.Int.PodCIDR, "--service-cidr", rec.Int.ServiceCIDR,
		"--kube-apiserver-arg=authentication-token-webhook-config-file=" + webhookDir + "/authn.yaml",
		"--kube-apiserver-arg=authentication-token-webhook-version=v1",
		"--kube-apiserver-arg=authentication-token-webhook-cache-ttl=30s",
		"--kube-apiserver-arg=authorization-mode=Node,RBAC,Webhook",
		"--kube-apiserver-arg=authorization-webhook-config-file=" + webhookDir + "/authz.yaml",
		"--kube-apiserver-arg=authorization-webhook-version=v1",
		"--kube-apiserver-arg=authorization-webhook-cache-authorized-ttl=10s",
		"--kube-apiserver-arg=authorization-webhook-cache-unauthorized-ttl=5s",
	}
	if ip != "" {
		args = append(args, "--advertise-address", ip, "--tls-san", ip)
	}
	if rec.Int.ExternalIP != "" {
		args = append(args, "--tls-san", rec.Int.ExternalIP)
	}
	id, err := d.rt.CreateContainer(ctx, runtime.ContainerSpec{
		Name:     name,
		Image:    ver.Image,
		Cmd:      args,
		Env:      []string{"K3S_TOKEN=" + rec.Int.Token},
		Labels:   s.labels(d, key, "server"),
		Hostname: trunc("gke-"+c.Name, 40) + "-cp",
		Networks: nets,
		Mounts:   []runtime.Mount{{Type: "volume", Source: name, Target: k3sDataDir}},
		Tmpfs:    map[string]string{"/run": ""},
	})
	if err != nil {
		return err
	}
	files := []runtime.File{
		{Name: strings.TrimPrefix(webhookDir, "/") + "/authn.yaml", Mode: 0o600, Data: webhookKubeconfig(hooks + "/authn")},
		{Name: strings.TrimPrefix(webhookDir, "/") + "/authz.yaml", Mode: 0o600, Data: webhookKubeconfig(hooks + "/authz")},
	}
	if err := d.rt.CopyTo(ctx, id, "/", files); err != nil {
		return err
	}
	if err := d.rt.StartContainer(ctx, id); err != nil {
		return err
	}
	if err := d.rt.WaitRunning(ctx, id); err != nil {
		return err
	}
	ct, err := d.rt.InspectContainer(ctx, id)
	if err != nil {
		return err
	}
	extIP := ""
	if ext.Name != "" {
		extIP = ct.IPs[ext.Name]
	}
	if ip == "" {
		ip = ct.IPs[sn.Name]
	}
	if err := s.updateCluster(key, func(rec *clusterRecord, c *containerpb.Cluster) error {
		rec.Int.ServerIP, rec.Int.ExternalIP = ip, extIP
		return nil
	}); err != nil {
		return err
	}
	creds, err := s.waitAdmin(ctx, d, id)
	if err != nil {
		return err
	}
	kc, err := newKubeClient("https://"+ip+":443", creds)
	if err != nil {
		return err
	}
	if err := waitFor(ctx, 2*time.Minute, func() (bool, error) { return kc.ready(ctx), nil }); err != nil {
		logs, _ := d.rt.Logs(context.Background(), id, 20)
		return fmt.Errorf("control plane did not become ready: %w\n%s", err, logs)
	}
	rt := s.rtFor(key)
	rt.mu.Lock()
	rt.kube, rt.creds = kc, creds
	rt.mu.Unlock()
	return s.updateCluster(key, func(rec *clusterRecord, c *containerpb.Cluster) error {
		pub := extIP
		if pub == "" {
			pub = ip
		}
		c.Endpoint = pub
		if privateEndpoint(c) {
			c.Endpoint = ip
		}
		c.MasterAuth.ClusterCaCertificate = base64.StdEncoding.EncodeToString(creds.CA)
		if c.PrivateClusterConfig != nil {
			c.PrivateClusterConfig.PrivateEndpoint = ip
			c.PrivateClusterConfig.PublicEndpoint = extIP
		}
		if c.ControlPlaneEndpointsConfig == nil {
			c.ControlPlaneEndpointsConfig = &containerpb.ControlPlaneEndpointsConfig{}
		}
		ipc := c.ControlPlaneEndpointsConfig.IpEndpointsConfig
		if ipc == nil {
			ipc = &containerpb.ControlPlaneEndpointsConfig_IPEndpointsConfig{}
			c.ControlPlaneEndpointsConfig.IpEndpointsConfig = ipc
		}
		t, pubOn := true, extIP != ""
		ipc.Enabled = &t
		ipc.EnablePublicEndpoint = &pubOn
		ipc.PublicEndpoint = extIP
		ipc.PrivateEndpoint = ip
		return nil
	})
}

// waitAdmin waits for k3s to write its admin kubeconfig and parses it.
func (s *Service) waitAdmin(ctx context.Context, d *deps, id string) (adminCreds, error) {
	var creds adminCreds
	err := waitFor(ctx, 2*time.Minute, func() (bool, error) {
		ct, err := d.rt.InspectContainer(ctx, id)
		if err != nil {
			return false, err
		}
		if !ct.Running {
			logs, _ := d.rt.Logs(ctx, id, 30)
			return false, fmt.Errorf("k3s server exited (code %d): %s", ct.ExitCode, strings.TrimSpace(logs))
		}
		b, err := d.rt.CopyFrom(ctx, id, "/etc/rancher/k3s/k3s.yaml")
		if err != nil || len(b) == 0 {
			return false, nil
		}
		creds, err = parseK3sKubeconfig(b)
		return err == nil, nil
	})
	return creds, err
}

// webhookKubeconfig is the kubeconfig kube-apiserver uses to call the
// emulator's TokenReview / SubjectAccessReview webhooks.
func webhookKubeconfig(url string) []byte {
	return []byte(`apiVersion: v1
kind: Config
clusters:
- name: gcpemu
  cluster:
    server: ` + url + `
users:
- name: kube-apiserver
contexts:
- name: webhook
  context:
    cluster: gcpemu
    user: kube-apiserver
current-context: webhook
`)
}

// upgradeServer recreates the control plane with a new version.
func (s *Service) upgradeServer(ctx context.Context, key, version string) error {
	d, err := s.deps(ctx)
	if err != nil {
		return err
	}
	rec, err := s.loadKey(key)
	if err != nil {
		return err
	}
	sn, err := d.vpc.SubnetNetwork(ctx, rec.Int.Subnetwork)
	if err != nil {
		return err
	}
	if err := s.startServer(ctx, d, key, sn, version); err != nil {
		return err
	}
	s.installCAInjector(ctx, d, key)
	return s.waitNodes(ctx, key, "")
}

// ---- nodes ----

// startNodes starts every recorded node of pool ("" = all pools) whose
// container is not running.
func (s *Service) startNodes(ctx context.Context, d *deps, key string, sn emu.SubnetNet, pool string) error {
	rec, err := s.loadKey(key)
	if err != nil {
		return err
	}
	var nodes []nodeRecord
	for _, n := range rec.Int.Nodes {
		if pool == "" || n.Pool == pool {
			nodes = append(nodes, n)
		}
	}
	errs := make([]error, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.startNode(ctx, d, key, sn, n, false)
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// nodeName returns the node container (and volume) name.
func nodeContainer(d *deps, c *containerpb.Cluster, node string) string {
	return d.rt.Name("gke", shortID(c), node)
}

// startNode creates and starts one node container. With recreate set, a
// running container is replaced (upgrades); otherwise it is kept.
func (s *Service) startNode(ctx context.Context, d *deps, key string, sn emu.SubnetNet, node nodeRecord, recreate bool) error {
	rec, err := s.loadKey(key)
	if err != nil {
		return err
	}
	c := rec.cluster()
	np := findPool(c, node.Pool)
	if np == nil {
		return fmt.Errorf("node pool %s not found", node.Pool)
	}
	name := nodeContainer(d, c, node.Name)
	if !recreate {
		if ct, err := d.rt.InspectContainer(ctx, name); err == nil && ct.Running {
			return nil
		}
	}
	ver, ok := lookupVersion(np.Version)
	if !ok {
		return apierr.FailedPrecondition("unknown node version %q", np.Version)
	}
	if err := d.rt.EnsureImage(ctx, ver.Image, s.env.Config.Offline); err != nil {
		return err
	}
	preload, err := s.preloadTar(ctx, d, ver)
	if err != nil {
		s.env.Log.Warn("gke: node image preload unavailable; nodes will pull system images", "err", err)
	}
	bin, err := agent.Binary()
	if err != nil {
		return apierr.FailedPrecondition("GKE nodes need a linux gcpemu binary: %v", err)
	}
	ip := node.IP
	if ip == "" {
		if ip, err = d.vpc.AllocateIP(ctx, sn, key+"/"+node.Name); err != nil {
			return err
		}
	}
	if err := d.rt.RemoveContainer(ctx, name, true); err != nil {
		return err
	}
	if err := d.rt.CreateVolume(ctx, name, s.labels(d, key, "node")); err != nil {
		return err
	}
	cfg, err := s.nodeAgentConfig(ctx, d, rec, c, np, node, sn)
	if err != nil {
		return err
	}
	nets := []runtime.Attachment{{Network: sn.Name, IP: ip}, {Network: d.svc.Name}}
	private := privateNodes(c, np)
	var ext netplane.Net
	if !private {
		if ext, err = d.np.External(ctx); err != nil {
			return err
		}
		nets = append(nets, runtime.Attachment{Network: ext.Name})
		cfg.DefaultVia = ext.Gateway
	}
	cfgJSON, _ := json.Marshal(cfg)
	args := []string{"agent", "--node-name", node.Name, "--node-ip", ip,
		"--resolv-conf", nodeResolvConf, "--kubelet-arg=max-pods=" + strconv.FormatInt(maxPods(c, np), 10)}
	for _, l := range nodeLabels(c, np, node) {
		args = append(args, "--node-label", l)
	}
	if private {
		// No egress: never fall back to upstream registries (FR-GKE-007).
		args = append(args, "--disable-default-registry-endpoint")
	}
	for _, t := range np.Config.Taints {
		args = append(args, "--node-taint", t.Key+"="+t.Value+":"+taintEffect(t.Effect))
	}
	labels := s.labels(d, key, "node")
	labels["gcpemu.gke.node"] = node.Name
	id, err := d.rt.CreateContainer(ctx, runtime.ContainerSpec{
		Name:       name,
		Image:      ver.Image,
		Entrypoint: []string{agent.ContainerPath},
		Cmd:        args,
		Env: []string{
			agent.EnvVar + "=" + nodeAgentName,
			nodeConfigEnv + "=" + string(cfgJSON),
			"K3S_URL=https://" + rec.Int.ServerIP + ":443",
			"K3S_TOKEN=" + rec.Int.Token,
		},
		Labels:     labels,
		Hostname:   node.Name,
		Privileged: true,
		Init:       true,
		Networks:   nets,
		Mounts:     []runtime.Mount{{Type: "volume", Source: name, Target: k3sDataDir}},
		Tmpfs:      map[string]string{"/run": ""},
	})
	if err != nil {
		return err
	}
	files := []runtime.File{
		{Name: strings.TrimPrefix(agent.ContainerPath, "/"), Mode: 0o755, Path: bin},
		{Name: "etc/rancher/node/password", Mode: 0o600, Data: []byte(node.Password)},
		{Name: "etc/rancher/k3s/registries.yaml", Mode: 0o600, Data: ar.RegistriesYAML(fmt.Sprintf("%s:%d", metadataIP, registryPort), nil)},
	}
	if preload != "" {
		files = append(files, runtime.File{Name: strings.TrimPrefix(k3sDataDir, "/") + "/agent/images/gcpemu-preload.tar", Mode: 0o644, Path: preload})
	}
	if err := d.rt.CopyTo(ctx, id, "/", files); err != nil {
		return err
	}
	if err := d.rt.StartContainer(ctx, id); err != nil {
		return err
	}
	if err := d.rt.WaitRunning(ctx, id); err != nil {
		return err
	}
	extIP := ""
	if ext.Name != "" {
		if ct, err := d.rt.InspectContainer(ctx, id); err == nil {
			extIP = ct.IPs[ext.Name]
		}
	}
	return s.updateCluster(key, func(rec *clusterRecord, c *containerpb.Cluster) error {
		for i := range rec.Int.Nodes {
			if rec.Int.Nodes[i].Name == node.Name {
				rec.Int.Nodes[i].IP = ip
				rec.Int.Nodes[i].ExternalIP = extIP
			}
		}
		return nil
	})
}

// nodeAgentConfig builds the gke-node agent's configuration.
func (s *Service) nodeAgentConfig(ctx context.Context, d *deps, rec *clusterRecord, c *containerpb.Cluster, np *containerpb.NodePool, node nodeRecord, sn emu.SubnetNet) (nodeConfig, error) {
	gw, err := d.np.Addr(ctx, "gateway")
	if err != nil {
		return nodeConfig{}, err
	}
	cfg := nodeConfig{
		Emu:         "http://" + gw + "/container/_emu/node/" + c.Id + "/" + rec.Int.Secret + "/" + node.Name,
		MirrorHosts: arRegistryHosts(),
		Network:     c.GetNetworkConfig().GetNetwork(),
	}
	if dns, err := d.np.Addr(ctx, "dns"); err == nil {
		cfg.DNS = dns
	}
	if reg, err := d.np.Addr(ctx, "ar"); err == nil {
		cfg.Registry = reg
	}
	if fe, err := d.np.Addr(ctx, frontend.Endpoint); err == nil && s.hosts != nil {
		cfg.Frontend = fe
		cfg.FrontendHosts = s.hosts()
	}
	if s.env.CA != nil {
		cfg.CA = string(s.env.CA.PEM())
	}
	egw, err := d.vpc.EgressGateway(ctx, rec.Int.Subnetwork)
	if err != nil {
		egw = ""
	}
	if privateNodes(c, np) {
		if egw != "" {
			cfg.DefaultVia = egw
		} else {
			cfg.NoDefaultRoute = true
		}
	} else {
		cfg.PrivateVia = egw
	}
	return cfg, nil
}

func maxPods(c *containerpb.Cluster, np *containerpb.NodePool) int64 {
	if n := np.GetMaxPodsConstraint().GetMaxPodsPerNode(); n > 0 {
		return n
	}
	if n := c.GetDefaultMaxPodsConstraint().GetMaxPodsPerNode(); n > 0 {
		return n
	}
	return 110
}

// nodeLabels returns the labels GKE puts on a node.
func nodeLabels(c *containerpb.Cluster, np *containerpb.NodePool, n nodeRecord) []string {
	cfg := np.Config
	m := map[string]string{
		"cloud.google.com/gke-nodepool":              np.Name,
		"cloud.google.com/gke-os-distribution":       osDistribution(cfg.ImageType),
		"cloud.google.com/gke-container-runtime":     "containerd",
		"cloud.google.com/gke-boot-disk":             cfg.DiskType,
		"cloud.google.com/machine-family":            strings.SplitN(cfg.MachineType, "-", 2)[0],
		"cloud.google.com/gke-provisioning":          "standard",
		"topology.kubernetes.io/zone":                n.Zone,
		"topology.kubernetes.io/region":              regionOf(n.Zone),
		"failure-domain.beta.kubernetes.io/zone":     n.Zone,
		"failure-domain.beta.kubernetes.io/region":   regionOf(n.Zone),
		"node.kubernetes.io/instance-type":           cfg.MachineType,
		"cloud.google.com/private-node":              strconv.FormatBool(privateNodes(c, np)),
		"iam.gke.io/gke-metadata-server-enabled":     strconv.FormatBool(wiEnabled(c, np)),
		"cloud.google.com/gke-logging-variant":       "DEFAULT",
		"cloud.google.com/gke-max-pods-per-node":     strconv.FormatInt(maxPods(c, np), 10),
		"cloud.google.com/gke-cpu-scaling-level":     "2",
		"cloud.google.com/gke-stack-type":            "IPV4",
		"cloud.google.com/gke-security-posture":      "standard",
		"cloud.google.com/gke-netd-ready":            "true",
		"addon.gke.io/node-local-dns-ds-ready":       "true",
		"cloud.google.com/gke-preemptible":           "",
		"cloud.google.com/gke-spot":                  "",
		"cloud.google.com/gke-ephemeral-storage-ssd": "",
	}
	for k, v := range cfg.Labels {
		m[k] = v
	}
	if cfg.Spot {
		m["cloud.google.com/gke-spot"] = "true"
	}
	if cfg.Preemptible {
		m["cloud.google.com/gke-preemptible"] = "true"
	}
	keys := make([]string, 0, len(m))
	for k, v := range m {
		if v != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
}

func osDistribution(imageType string) string {
	if strings.HasPrefix(strings.ToUpper(imageType), "UBUNTU") {
		return "ubuntu"
	}
	return "cos"
}

func taintEffect(e containerpb.NodeTaint_Effect) string {
	switch e {
	case containerpb.NodeTaint_PREFER_NO_SCHEDULE:
		return "PreferNoSchedule"
	case containerpb.NodeTaint_NO_EXECUTE:
		return "NoExecute"
	}
	return "NoSchedule"
}

// wiEnabled reports whether Workload Identity is active on a pool.
func wiEnabled(c *containerpb.Cluster, np *containerpb.NodePool) bool {
	if c.GetWorkloadIdentityConfig().GetWorkloadPool() == "" {
		return false
	}
	return np.GetConfig().GetWorkloadMetadataConfig().GetMode() == containerpb.WorkloadMetadataConfig_GKE_METADATA
}

// waitNodes waits until every recorded node of pool ("" = all) is Ready.
func (s *Service) waitNodes(ctx context.Context, key, pool string) error {
	kc, err := s.kube(key)
	if err != nil {
		return err
	}
	var missing []string
	err = waitFor(ctx, nodeReadyWait, func() (bool, error) {
		rec, err := s.loadKey(key)
		if err != nil {
			return false, err
		}
		var list kubeList[kubeNode]
		if err := kc.get(ctx, "/api/v1/nodes", &list); err != nil {
			return false, nil
		}
		ready := map[string]bool{}
		for _, n := range list.Items {
			ready[n.Metadata.Name] = n.ready()
		}
		missing = missing[:0]
		for _, n := range rec.Int.Nodes {
			if (pool == "" || n.Pool == pool) && !ready[n.Name] {
				missing = append(missing, n.Name)
			}
		}
		return len(missing) == 0, nil
	})
	if err != nil {
		return fmt.Errorf("nodes %v did not become ready: %w", missing, err)
	}
	return nil
}

// reconcilePool starts a pool's missing node containers and waits for them.
func (s *Service) reconcilePool(ctx context.Context, key, pool string) error {
	d, err := s.deps(ctx)
	if err != nil {
		return err
	}
	rec, err := s.loadKey(key)
	if err != nil {
		return err
	}
	sn, err := d.vpc.SubnetNetwork(ctx, rec.Int.Subnetwork)
	if err != nil {
		return err
	}
	if err := s.startNodes(ctx, d, key, sn, pool); err != nil {
		return err
	}
	if err := s.waitNodes(ctx, key, pool); err != nil {
		return err
	}
	s.writeKubeconfig()
	return s.updateCluster(key, func(rec *clusterRecord, c *containerpb.Cluster) error {
		if np := findPool(c, pool); np != nil {
			np.Status = containerpb.NodePool_RUNNING
		}
		return nil
	})
}

// recreatePool replaces a pool's node containers one at a time (rolling
// upgrade), keeping their volumes and names.
func (s *Service) recreatePool(ctx context.Context, key, pool string) error {
	d, err := s.deps(ctx)
	if err != nil {
		return err
	}
	rec, err := s.loadKey(key)
	if err != nil {
		return err
	}
	sn, err := d.vpc.SubnetNetwork(ctx, rec.Int.Subnetwork)
	if err != nil {
		return err
	}
	for _, n := range rec.nodesOf(pool) {
		s.drain(ctx, key, n.Name)
		if err := s.startNode(ctx, d, key, sn, n, true); err != nil {
			return err
		}
		if kc, err := s.kube(key); err == nil {
			_ = kc.mergePatch(ctx, "/api/v1/nodes/"+n.Name, map[string]any{"spec": map[string]any{"unschedulable": nil}})
		}
	}
	return s.waitNodes(ctx, key, pool)
}

// drain cordons a node and deletes its pods (except DaemonSet pods).
func (s *Service) drain(ctx context.Context, key, node string) {
	kc, err := s.kube(key)
	if err != nil {
		return
	}
	_ = kc.mergePatch(ctx, "/api/v1/nodes/"+node, map[string]any{"spec": map[string]any{"unschedulable": true}})
	var pods kubeList[kubePod]
	if err := kc.get(ctx, "/api/v1/pods?fieldSelector=spec.nodeName%3D"+node, &pods); err != nil {
		return
	}
	for _, p := range pods.Items {
		ds := false
		for _, o := range p.Metadata.OwnerReferences {
			ds = ds || o.Kind == "DaemonSet"
		}
		if !ds {
			_ = kc.delete(ctx, "/api/v1/namespaces/"+p.Metadata.Namespace+"/pods/"+p.Metadata.Name)
		}
	}
}

// removeNodes drains and deletes nodes and their containers and volumes.
func (s *Service) removeNodes(ctx context.Context, key string, nodes []nodeRecord) error {
	if len(nodes) == 0 {
		return nil
	}
	d, err := s.deps(ctx)
	if err != nil {
		return err
	}
	rec, err := s.loadKey(key)
	if err != nil {
		return err
	}
	c := rec.cluster()
	sn, snErr := d.vpc.SubnetNetwork(ctx, rec.Int.Subnetwork)
	var errs []error
	for _, n := range nodes {
		s.drain(ctx, key, n.Name)
		if kc, err := s.kube(key); err == nil {
			_ = kc.delete(ctx, "/api/v1/nodes/"+n.Name)
		}
		name := nodeContainer(d, c, n.Name)
		if err := d.rt.RemoveContainer(ctx, name, true); err != nil {
			errs = append(errs, err)
		}
		if err := d.rt.RemoveVolume(ctx, name); err != nil {
			errs = append(errs, err)
		}
		if snErr == nil {
			_ = d.vpc.ReleaseIP(ctx, sn, key+"/"+n.Name)
		}
	}
	return errors.Join(errs...)
}

// syncPoolLabels applies a pool's labels and taints to its existing nodes
// (k3s applies --node-label/--node-taint only at registration).
func (s *Service) syncPoolLabels(ctx context.Context, key, pool string) error {
	kc, err := s.kube(key)
	if err != nil {
		return err
	}
	rec, err := s.loadKey(key)
	if err != nil {
		return err
	}
	c := rec.cluster()
	np := findPool(c, pool)
	if np == nil {
		return nil
	}
	for _, n := range rec.nodesOf(pool) {
		var node kubeNode
		if err := kc.get(ctx, "/api/v1/nodes/"+n.Name, &node); err != nil {
			return err
		}
		labels := map[string]any{}
		prev := strings.Split(node.Metadata.Annotations[annoUserLabels], ",")
		for _, k := range prev {
			if k != "" {
				labels[k] = nil
			}
		}
		var keys []string
		for k, v := range np.Config.Labels {
			labels[k] = v
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var taints []kubeTaint
		for _, t := range node.Spec.Taints {
			if !strings.HasPrefix(t.Key, "node.kubernetes.io/") && !strings.HasPrefix(t.Key, "node.cloudprovider.kubernetes.io/") {
				continue // drop previous user taints
			}
			taints = append(taints, t)
		}
		for _, t := range np.Config.Taints {
			taints = append(taints, kubeTaint{Key: t.Key, Value: t.Value, Effect: taintEffect(t.Effect)})
		}
		patch := map[string]any{
			"metadata": map[string]any{
				"labels":      labels,
				"annotations": map[string]any{annoUserLabels: strings.Join(keys, ",")},
			},
			"spec": map[string]any{"taints": taints},
		}
		if err := kc.mergePatch(ctx, "/api/v1/nodes/"+n.Name, patch); err != nil {
			return err
		}
	}
	return nil
}

// annoUserLabels records which node labels came from the node pool config.
const annoUserLabels = "gcpemu.io/node-pool-labels"

// ---- teardown ----

// teardown deletes a cluster: NEGs, containers, volumes, addresses and
// finally the record.
func (s *Service) teardown(ctx context.Context, key string) error {
	if err := s.destroyDataPlane(ctx, key); err != nil {
		var ae *apierr.Error
		if !errors.As(err, &ae) {
			return err
		}
		// No runtime: there is nothing to remove.
	}
	var id string
	err := s.env.Store.Update(func(tx store.Tx) error {
		if rec, err := getCluster(tx, key); err == nil {
			id = rec.cluster().Id
		}
		return tx.Delete(nsClusters, key)
	})
	s.mu.Lock()
	delete(s.clusters, key)
	delete(s.byID, id)
	s.mu.Unlock()
	s.writeKubeconfig()
	return err
}

// destroyDataPlane removes everything a cluster runs.
func (s *Service) destroyDataPlane(ctx context.Context, key string) error {
	s.rtFor(key).stop()
	rec, err := s.loadKey(key)
	if err != nil {
		return nil
	}
	s.deleteNEGs(ctx, key)
	d, err := s.deps(ctx)
	if err != nil {
		return err
	}
	sel := map[string]string{runtime.LabelInstance: d.rt.InstanceID, runtime.LabelService: "gke", runtime.LabelResource: key}
	cs, err := d.rt.ListContainers(ctx, sel)
	if err != nil {
		return err
	}
	var errs []error
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, ct := range cs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := d.rt.RemoveContainer(ctx, ct.ID, true); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	vols, err := d.rt.ListVolumes(ctx, sel)
	if err != nil {
		errs = append(errs, err)
	}
	for _, v := range vols {
		if err := d.rt.RemoveVolume(ctx, v); err != nil {
			errs = append(errs, err)
		}
	}
	if sn, err := d.vpc.SubnetNetwork(ctx, rec.Int.Subnetwork); err == nil {
		_ = d.vpc.ReleaseIP(ctx, sn, key+"/control-plane")
		for _, n := range rec.Int.Nodes {
			_ = d.vpc.ReleaseIP(ctx, sn, key+"/"+n.Name)
		}
	}
	return errors.Join(errs...)
}

// waitIdle waits until no operation holds the cluster.
func (s *Service) waitIdle(ctx context.Context, key string) error {
	return waitFor(ctx, 2*time.Minute, func() (bool, error) {
		s.mu.Lock()
		_, busy := s.busy[key]
		s.mu.Unlock()
		return !busy, nil
	})
}

// ---- image preload ----

var preloadMu sync.Mutex

// preloadTar returns a cached `docker save` tarball of a version's system
// images, exporting it from the host image cache on first use (pulling the
// images on the host if needed). Nodes import it at start, so they need no
// registry access for system pods (FR-GKE-006, FR-GKE-007).
func (s *Service) preloadTar(ctx context.Context, d *deps, v k8sVersion) (string, error) {
	if len(v.Preload) == 0 {
		return "", nil
	}
	dir, err := s.env.ServiceDir("gke")
	if err != nil {
		return "", err
	}
	tag := strings.NewReplacer("/", "_", ":", "_").Replace(v.Image)
	p := filepath.Join(dir, "images", tag+".tar")
	preloadMu.Lock()
	defer preloadMu.Unlock()
	if fi, err := os.Stat(p); err == nil && fi.Size() > 0 {
		return p, nil
	}
	for _, img := range v.Preload {
		if err := d.rt.EnsureImage(ctx, img, s.env.Config.Offline); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	tmp := p + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	err = d.rt.SaveImages(ctx, v.Preload, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	return p, os.Rename(tmp, p)
}

// waitFor polls cond until it returns true, an error, or timeout.
func waitFor(ctx context.Context, timeout time.Duration, cond func() (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		ok, err := cond()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
