// Package runtime is the container runtime driver (Section 3.1): a small
// client for the Docker Engine API, which Docker Engine and Podman (its
// Docker-compatible socket) both serve. It creates, labels, starts, stops and
// reclaims the node and PostgreSQL containers the emulator runs, plus the
// networks and volumes they use.
package runtime

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Labels applied to everything the emulator creates.
const (
	LabelInstance = "gcpemu.instance" // instance ID
	LabelService  = "gcpemu.service"  // owning service, e.g. "sql"
	LabelResource = "gcpemu.resource" // owning resource name
	LabelRole     = "gcpemu.role"     // e.g. "server", "node", "nat"
	LabelDir      = "gcpemu.dir"      // absolute data dir of the instance
	// LabelHostLB is on a GKE cluster's server container that runs
	// without servicelb, and says why: HostLBRelay, HostLBTranslated or
	// HostLBVM. `gcpemu doctor` reads it (ADR 0012).
	LabelHostLB = "gcpemu.hostlb"
)

// LabelHostLB values.
const (
	HostLBRelay      = "vm-relay"
	HostLBTranslated = "translated"
	HostLBVM         = "vm-runtime"
)

// apiVersion is the Docker Engine API version used. 1.41 is served by
// Docker >= 20.10 and Podman >= 3 (compat API).
const apiVersion = "v1.41"

// ErrNotFound is returned when a container, network, volume or image is absent.
var ErrNotFound = errors.New("runtime: not found")

// Client talks to a Docker-compatible Engine API endpoint.
type Client struct {
	http *http.Client
	base string // e.g. "http://docker/v1.41"
	// Endpoint is the socket, pipe or URL in use.
	Endpoint string
	// dial connects to Endpoint; host is the Host header sent over it.
	dial func(context.Context) (net.Conn, error)
	host string
	// self is the emulator's own container (self.go).
	self string
	// member joins every network like self (SetMember).
	memberMu sync.Mutex
	member   string
	joined   func(Network, bool)
	// memberAddr pins the member's address per network (SetMemberAddr).
	memberAddr func(network string) string
	// starts records the containers the client started (Disrupted).
	startsMu sync.Mutex
	starts   map[string]start // by container ID
}

// start is a container's name and start time as the client last started it.
type start struct{ name, at string }

// Info describes the connected runtime.
type Info struct {
	Name     string // "docker" or "podman"
	Version  string
	Arch     string // GOARCH-style
	Rootless bool
	// OS is the runtime's operating system ("Docker Desktop", "OrbStack",
	// "Ubuntu 24.04 LTS" for Colima) and Kernel its kernel version: on a
	// VM runtime, the VM's.
	OS     string
	Kernel string
}

// String names the runtime for messages, e.g. "docker 27.3.1 (Docker
// Desktop, kernel 6.10.14-linuxkit)".
func (i Info) String() string {
	s := i.Name
	if i.Version != "" {
		s += " " + i.Version
	}
	var d []string
	if i.OS != "" {
		d = append(d, i.OS)
	}
	if i.Kernel != "" {
		d = append(d, "kernel "+i.Kernel)
	}
	if len(d) > 0 {
		s += " (" + strings.Join(d, ", ") + ")"
	}
	return s
}

// New returns a client for endpoint ("unix:///path", "tcp://host:port" or,
// on Windows, "npipe:////./pipe/name").
func New(endpoint string) (*Client, error) {
	c := &Client{Endpoint: endpoint, host: "docker"}
	var d net.Dialer
	switch scheme, addr, _ := strings.Cut(endpoint, "://"); scheme {
	case "unix":
		c.dial = func(ctx context.Context) (net.Conn, error) { return d.DialContext(ctx, "unix", addr) }
	case "tcp":
		c.host = addr
		c.dial = func(ctx context.Context) (net.Conn, error) { return d.DialContext(ctx, "tcp", addr) }
	case "npipe":
		dial, err := pipeDialer(addr)
		if err != nil {
			return nil, err
		}
		c.dial = dial
	default:
		return nil, fmt.Errorf("unsupported runtime endpoint %q", endpoint)
	}
	tr := &http.Transport{MaxIdleConnsPerHost: 8, IdleConnTimeout: 30 * time.Second,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return c.dial(ctx) }}
	c.http = &http.Client{Transport: tr}
	c.base = "http://" + c.host + "/" + apiVersion
	return c, nil
}

type apiError struct {
	Status  int
	Message string `json:"message"`
}

func (e *apiError) Error() string { return fmt.Sprintf("runtime API %d: %s", e.Status, e.Message) }

func (e *apiError) Is(target error) bool {
	return target == ErrNotFound && e.Status == http.StatusNotFound
}

// do performs a request; body may be nil, an io.Reader or a value to JSON-encode.
func (c *Client) do(ctx context.Context, method, p string, q url.Values, body any, out any) error {
	resp, err := c.raw(ctx, method, p, q, body, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) raw(ctx context.Context, method, p string, q url.Values, body any, contentType string) (*http.Response, error) {
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case io.Reader:
		rd = b
	default:
		buf, err := json.Marshal(b)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(buf)
		contentType = "application/json"
	}
	u := c.base + p
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotModified {
		defer resp.Body.Close()
		e := &apiError{Status: resp.StatusCode}
		b, _ := io.ReadAll(resp.Body)
		if json.Unmarshal(b, e) != nil || e.Message == "" {
			e.Message = strings.TrimSpace(string(b))
		}
		return nil, e
	}
	return resp, nil
}

// Info returns runtime identification.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var v struct {
		Version    string
		Arch       string
		Components []struct{ Name string }
	}
	if err := c.do(ctx, http.MethodGet, "/version", nil, nil, &v); err != nil {
		return Info{}, err
	}
	in := Info{Name: "docker", Version: v.Version, Arch: v.Arch}
	for _, comp := range v.Components {
		if strings.Contains(strings.ToLower(comp.Name), "podman") {
			in.Name = "podman"
		}
	}
	var info struct {
		SecurityOptions []string
		OperatingSystem string
		KernelVersion   string
	}
	if err := c.do(ctx, http.MethodGet, "/info", nil, nil, &info); err == nil {
		in.OS, in.Kernel = info.OperatingSystem, info.KernelVersion
		for _, o := range info.SecurityOptions {
			if strings.Contains(o, "rootless") {
				in.Rootless = true
			}
		}
	}
	return in, nil
}

// --- images ---

// ImageExists reports whether ref is present locally.
func (c *Client) ImageExists(ctx context.Context, ref string) (bool, error) {
	err := c.do(ctx, http.MethodGet, "/images/"+ref+"/json", nil, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// EnsureImage pulls ref unless it is already present.
func (c *Client) EnsureImage(ctx context.Context, ref string, offline bool) error {
	ok, err := c.ImageExists(ctx, ref)
	if err != nil || ok {
		return err
	}
	if offline {
		return fmt.Errorf("image %s is not present and --offline is set (pull it or use `gcpemu images load`)", ref)
	}
	return c.pull(ctx, ref, "")
}

// pull pulls ref, for platform ("linux/arm64") when it is set.
func (c *Client) pull(ctx context.Context, ref, platform string) error {
	name, tag := splitRef(ref)
	q := url.Values{"fromImage": {name}}
	if tag != "" {
		q.Set("tag", tag)
	}
	if platform != "" {
		q.Set("platform", platform)
	}
	resp, err := c.raw(ctx, http.MethodPost, "/images/create", q, nil, "")
	if err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	defer resp.Body.Close()
	// The pull streams JSON progress; errors arrive in-band.
	dec := json.NewDecoder(resp.Body)
	for {
		var m struct {
			Error string `json:"error"`
		}
		if err := dec.Decode(&m); err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("pull %s: %w", ref, err)
		}
		if m.Error != "" {
			return fmt.Errorf("pull %s: %s", ref, m.Error)
		}
	}
	return nil
}

// splitRef splits "repo:tag" or "repo@digest" for the pull API.
func splitRef(ref string) (name, tag string) {
	if i := strings.Index(ref, "@"); i >= 0 {
		return ref[:i], ref[i+1:]
	}
	slash := strings.LastIndex(ref, "/")
	if i := strings.LastIndex(ref, ":"); i > slash {
		return ref[:i], ref[i+1:]
	}
	return ref, "latest"
}

// --- networks ---

// NetworkSpec describes a bridge network.
type NetworkSpec struct {
	Name     string
	Subnet   string // CIDR
	Gateway  string // optional; defaults to the first host address
	Internal bool   // no external connectivity
	Labels   map[string]string
	// MTU sets com.docker.network.driver.mtu when non-zero.
	MTU int
	// Options are extra driver options (e.g. com.docker.network.bridge.*).
	Options map[string]string
	// Pin, without a Subnet, creates the network on the subnet the runtime
	// picks, but as a configured one: before Docker 29, containers can ask
	// for a fixed address only on networks created with a subnet.
	Pin bool
}

// Network is an inspected network.
type Network struct {
	ID       string
	Name     string
	Subnet   string
	Gateway  string
	Internal bool
	Labels   map[string]string
	// Bridge is the host interface name, when the runtime reports it.
	Bridge string
}

// CreateNetwork creates a bridge network and returns its ID. Inside a
// container the emulator joins it (AttachSelf), and so does the member
// container (SetMember).
func (c *Client) CreateNetwork(ctx context.Context, s NetworkSpec) (string, error) {
	id, err := c.createNetwork(ctx, s)
	if err != nil {
		return "", err
	}
	if err := c.AttachSelf(ctx, s.Name); err != nil {
		return id, fmt.Errorf("connect the emulator to network %s: %w", s.Name, err)
	}
	if err := c.AttachMember(ctx, s.Name, false); err != nil {
		return id, fmt.Errorf("connect the VM relay to network %s: %w", s.Name, err)
	}
	return id, nil
}

func (c *Client) createNetwork(ctx context.Context, s NetworkSpec) (string, error) {
	if s.Pin && s.Subnet == "" {
		return c.createPinned(ctx, s)
	}
	opts := map[string]string{}
	for k, v := range s.Options {
		opts[k] = v
	}
	if s.MTU > 0 {
		opts["com.docker.network.driver.mtu"] = strconv.Itoa(s.MTU)
	}
	body := map[string]any{
		"Name":           s.Name,
		"Driver":         "bridge",
		"Internal":       s.Internal,
		"Labels":         s.Labels,
		"CheckDuplicate": true,
		"Options":        opts,
	}
	if s.Subnet != "" {
		cfg := map[string]string{"Subnet": s.Subnet}
		if s.Gateway != "" {
			cfg["Gateway"] = s.Gateway
		}
		body["IPAM"] = map[string]any{"Driver": "default", "Config": []map[string]string{cfg}}
	}
	var out struct {
		ID string `json:"Id"`
	}
	err := c.do(ctx, http.MethodPost, "/networks/create", nil, body, &out)
	if err != nil && s.Subnet == "" && poolsExhausted(err) {
		return c.createInFallbackRange(ctx, s, err)
	}
	if err != nil {
		return "", fmt.Errorf("create network %s: %w", s.Name, err)
	}
	return out.ID, nil
}

// pinMu serializes createPinned within the process: between deleting the
// probe network and creating the pinned one, another creation would be
// handed the same (lowest free) subnet.
var pinMu sync.Mutex

// createPinned creates s on the subnet the runtime picks (see
// NetworkSpec.Pin): it creates the network, reads the subnet and creates
// it again with that subnet. If other processes keep taking the subnet in
// between, the network is left unpinned.
func (c *Client) createPinned(ctx context.Context, s NetworkSpec) (string, error) {
	pinMu.Lock()
	defer pinMu.Unlock()
	s.Pin = false
	for attempt := 0; attempt < 10; attempt++ {
		id, err := c.createNetwork(ctx, s)
		if err != nil {
			return "", err
		}
		n, err := c.InspectNetwork(ctx, id)
		if err != nil {
			return "", err
		}
		if n.Subnet == "" {
			return id, nil
		}
		if err := c.do(ctx, http.MethodDelete, "/networks/"+url.PathEscape(id), nil, nil, nil); err != nil {
			return "", fmt.Errorf("create network %s: %w", s.Name, err)
		}
		p := s
		p.Subnet, p.Gateway = n.Subnet, n.Gateway
		id, err = c.createNetwork(ctx, p)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "overlap") {
			return id, err
		}
		// Another process took the subnet in between: pick again.
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Duration(rand.IntN(100)) * time.Millisecond):
		}
	}
	return c.createNetwork(ctx, s)
}

// poolsExhausted reports a runtime that has no default address pool left
// for a network without an explicit subnet (Docker: "all predefined
// address pools have been fully subnetted"; Podman: "could not find free
// subnet").
func poolsExhausted(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "address pools have been fully subnetted") || strings.Contains(m, "could not find free subnet") ||
		strings.Contains(m, "no available network")
}

// fallbackRange holds the /24s used when the runtime's default pools are
// used up: many instances side by side (FR-CORE-033) each need several
// networks, more than Docker's ~31 default ones. It is carrier-grade NAT
// space, which neither the runtimes' pools nor the emulator's VPC fallback
// (10.240.0.0/12) use.
var fallbackRange = netip.MustParsePrefix("100.64.0.0/10")

// createInFallbackRange creates s on a /24 of fallbackRange that overlaps
// no existing network, starting at a random /24 so that concurrent
// instances rarely race for the same one.
func (c *Client) createInFallbackRange(ctx context.Context, s NetworkSpec, cause error) (string, error) {
	var nets []struct {
		IPAM struct {
			Config []struct{ Subnet string }
		}
	}
	if err := c.do(ctx, http.MethodGet, "/networks", nil, nil, &nets); err != nil {
		return "", fmt.Errorf("create network %s: %w", s.Name, cause)
	}
	var used []netip.Prefix
	for _, n := range nets {
		for _, cfg := range n.IPAM.Config {
			if p, err := netip.ParsePrefix(cfg.Subnet); err == nil {
				used = append(used, p.Masked())
			}
		}
	}
	const blocks = 1 << (24 - 10)
	base := fallbackRange.Addr().As4()
	off := rand.IntN(blocks)
	for i := range blocks {
		k := (off + i) % blocks
		a := base
		a[1] += byte(k >> 8)
		a[2] = byte(k)
		p := netip.PrefixFrom(netip.AddrFrom4(a), 24)
		if overlapsAny(p, used) {
			continue
		}
		t := s
		t.Subnet = p.String()
		a[3] = 1
		t.Gateway = netip.AddrFrom4(a).String()
		id, err := c.CreateNetwork(ctx, t)
		if err == nil {
			return id, nil
		}
		if !strings.Contains(strings.ToLower(err.Error()), "overlap") {
			return "", err
		}
		used = append(used, p) // taken concurrently; try the next one
	}
	return "", fmt.Errorf("create network %s: %w (and no free /24 in %s)", s.Name, cause, fallbackRange)
}

func overlapsAny(p netip.Prefix, used []netip.Prefix) bool {
	for _, u := range used {
		if u.Overlaps(p) {
			return true
		}
	}
	return false
}

// InspectNetwork returns a network by name or ID.
func (c *Client) InspectNetwork(ctx context.Context, name string) (Network, error) {
	var v struct {
		ID       string `json:"Id"`
		Name     string
		Internal bool
		Labels   map[string]string
		Options  map[string]string
		IPAM     struct {
			Config []struct{ Subnet, Gateway string }
		}
	}
	if err := c.do(ctx, http.MethodGet, "/networks/"+url.PathEscape(name), nil, nil, &v); err != nil {
		return Network{}, err
	}
	n := Network{ID: v.ID, Name: v.Name, Internal: v.Internal, Labels: v.Labels, Bridge: v.Options["com.docker.network.bridge.name"]}
	for _, cfg := range v.IPAM.Config {
		if strings.Contains(cfg.Subnet, ".") { // first IPv4 config
			n.Subnet, n.Gateway = cfg.Subnet, cfg.Gateway
			break
		}
	}
	if n.Bridge == "" && len(v.ID) >= 12 {
		n.Bridge = "br-" + v.ID[:12]
	}
	return n, nil
}

// NetworkAddrs returns the IPv4 addresses held on a network: those of
// attached containers and those that created (not yet started) containers
// asked for.
func (c *Client) NetworkAddrs(ctx context.Context, network string) (map[string]bool, error) {
	var nv struct {
		Containers map[string]struct{ IPv4Address string }
	}
	if err := c.do(ctx, http.MethodGet, "/networks/"+url.PathEscape(network), nil, nil, &nv); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, ep := range nv.Containers {
		if ip, _, _ := strings.Cut(ep.IPv4Address, "/"); ip != "" {
			out[ip] = true
		}
	}
	var cv []struct {
		NetworkSettings struct {
			Networks map[string]struct {
				IPAMConfig *struct{ IPv4Address string }
				IPAddress  string
			}
		}
	}
	q := url.Values{"all": {"true"}, "filters": {`{"network":[` + strconv.Quote(network) + `]}`}}
	if err := c.do(ctx, http.MethodGet, "/containers/json", q, nil, &cv); err != nil {
		return nil, err
	}
	for _, ct := range cv {
		for _, ep := range ct.NetworkSettings.Networks {
			if ep.IPAMConfig != nil && ep.IPAMConfig.IPv4Address != "" {
				out[ep.IPAMConfig.IPv4Address] = true
			}
			if ep.IPAddress != "" {
				out[ep.IPAddress] = true
			}
		}
	}
	return out, nil
}

// ListNetworks returns networks carrying all the given labels.
func (c *Client) ListNetworks(ctx context.Context, labels map[string]string) ([]Network, error) {
	var v []struct {
		ID   string `json:"Id"`
		Name string
	}
	if err := c.do(ctx, http.MethodGet, "/networks", url.Values{"filters": {labelFilter(labels)}}, nil, &v); err != nil {
		return nil, err
	}
	var out []Network
	for _, n := range v {
		if in, err := c.InspectNetwork(ctx, n.ID); err == nil {
			out = append(out, in)
		}
	}
	return out, nil
}

// RemoveNetwork deletes a network; a missing network is not an error.
func (c *Client) RemoveNetwork(ctx context.Context, name string) error {
	if c.self != "" {
		_ = c.Disconnect(ctx, name, c.self)
	}
	c.memberMu.Lock()
	member, joined := c.member, c.joined
	c.memberMu.Unlock()
	if member != "" {
		if n, err := c.InspectNetwork(ctx, name); err == nil {
			_ = c.Disconnect(ctx, name, member)
			joined(n, false)
		}
	}
	err := c.do(ctx, http.MethodDelete, "/networks/"+url.PathEscape(name), nil, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// Connect attaches a container to a network with an optional static IP.
func (c *Client) Connect(ctx context.Context, network, container, ip string, aliases ...string) error {
	ep := map[string]any{}
	if ip != "" {
		ep["IPAMConfig"] = map[string]string{"IPv4Address": ip}
	}
	if len(aliases) > 0 {
		ep["Aliases"] = aliases
	}
	return c.do(ctx, http.MethodPost, "/networks/"+url.PathEscape(network)+"/connect", nil,
		map[string]any{"Container": container, "EndpointConfig": ep}, nil)
}

// Disconnect detaches a container from a network.
func (c *Client) Disconnect(ctx context.Context, network, container string) error {
	return c.do(ctx, http.MethodPost, "/networks/"+url.PathEscape(network)+"/disconnect", nil,
		map[string]any{"Container": container, "Force": true}, nil)
}

// --- volumes ---

// CreateVolume creates (or returns the existing) named volume.
func (c *Client) CreateVolume(ctx context.Context, name string, labels map[string]string) error {
	return c.do(ctx, http.MethodPost, "/volumes/create", nil, map[string]any{"Name": name, "Labels": labels}, nil)
}

// RemoveVolume deletes a volume; a missing volume is not an error.
func (c *Client) RemoveVolume(ctx context.Context, name string) error {
	err := c.do(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(name), url.Values{"force": {"true"}}, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// Volume is a listed volume.
type Volume struct {
	Name   string
	Labels map[string]string
}

// Volumes returns volumes carrying all the given labels.
func (c *Client) Volumes(ctx context.Context, labels map[string]string) ([]Volume, error) {
	var v struct{ Volumes []Volume }
	if err := c.do(ctx, http.MethodGet, "/volumes", url.Values{"filters": {labelFilter(labels)}}, nil, &v); err != nil {
		return nil, err
	}
	return v.Volumes, nil
}

// ListVolumes returns names of volumes carrying all the given labels.
func (c *Client) ListVolumes(ctx context.Context, labels map[string]string) ([]string, error) {
	vs, err := c.Volumes(ctx, labels)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, vol := range vs {
		out = append(out, vol.Name)
	}
	return out, nil
}

func labelFilter(labels map[string]string) string {
	var ls []string
	for k, v := range labels {
		if v == "" {
			ls = append(ls, k)
		} else {
			ls = append(ls, k+"="+v)
		}
	}
	b, _ := json.Marshal(map[string][]string{"label": ls})
	return string(b)
}

// --- containers ---

// Mount is a volume, bind or tmpfs mount.
type Mount struct {
	Type     string // "volume", "bind" or "tmpfs"
	Source   string
	Target   string
	ReadOnly bool
}

// Port publishes a container port on the host.
type Port struct {
	HostIP        string
	HostPort      int // 0 = runtime picks
	ContainerPort int
	Proto         string // "tcp" (default) or "udp"
}

// Attachment connects the container to a network at creation.
type Attachment struct {
	Network string
	IP      string
	Aliases []string
}

// ContainerSpec describes a container to create.
type ContainerSpec struct {
	Name       string
	Image      string
	Entrypoint []string
	Cmd        []string
	Env        []string
	Labels     map[string]string
	Hostname   string
	Privileged bool
	CapAdd     []string
	Sysctls    map[string]string
	// Networks: the first is used at create time, the rest are connected
	// before start.
	Networks []Attachment
	Ports    []Port
	Mounts   []Mount
	// Tmpfs maps paths to mount options.
	Tmpfs map[string]string
	// CgroupnsHost runs in the host cgroup namespace (needed by some
	// nested-container setups).
	CgroupnsHost bool
	// Memory limit in bytes (0 = unlimited).
	Memory int64
	// Init runs a tiny init as PID 1 to reap zombies.
	Init bool
	// Platform picks the image variant ("linux/arm64"); "" lets the
	// runtime choose.
	Platform string
}

// Container is an inspected container.
type Container struct {
	ID      string
	Name    string
	Image   string
	Labels  map[string]string
	Running bool
	Status  string // created, running, exited, ...
	// StartedAt is the runtime's time of the last start, RFC 3339.
	StartedAt string
	// ExitCode of the last run.
	ExitCode int
	// IPs maps network name to IPv4 address.
	IPs map[string]string
	// Ports maps "5432/tcp" to the published host address.
	Ports map[string]string
	// Cmd is the command the container was created with.
	Cmd []string
}

// CreateContainer creates (but does not start) a container.
func (c *Client) CreateContainer(ctx context.Context, s ContainerSpec) (string, error) {
	exposed := map[string]struct{}{}
	bindings := map[string][]map[string]string{}
	for _, p := range s.Ports {
		proto := p.Proto
		if proto == "" {
			proto = "tcp"
		}
		k := fmt.Sprintf("%d/%s", p.ContainerPort, proto)
		exposed[k] = struct{}{}
		hp := ""
		if p.HostPort > 0 {
			hp = strconv.Itoa(p.HostPort)
		}
		bindings[k] = append(bindings[k], map[string]string{"HostIp": p.HostIP, "HostPort": hp})
	}
	var mounts []map[string]any
	for _, m := range s.Mounts {
		mounts = append(mounts, map[string]any{"Type": m.Type, "Source": m.Source, "Target": m.Target, "ReadOnly": m.ReadOnly})
	}
	host := map[string]any{
		"Privileged":   s.Privileged,
		"CapAdd":       s.CapAdd,
		"Sysctls":      s.Sysctls,
		"PortBindings": bindings,
		"Mounts":       mounts,
		"Tmpfs":        s.Tmpfs,
		"Memory":       s.Memory,
		"Init":         s.Init,
	}
	if s.CgroupnsHost {
		host["CgroupnsMode"] = "host"
	}
	body := map[string]any{
		"Image":        s.Image,
		"Env":          s.Env,
		"Labels":       s.Labels,
		"Hostname":     s.Hostname,
		"ExposedPorts": exposed,
		"HostConfig":   host,
	}
	if s.Entrypoint != nil {
		body["Entrypoint"] = s.Entrypoint
	}
	if s.Cmd != nil {
		body["Cmd"] = s.Cmd
	}
	if len(s.Networks) > 0 {
		a := s.Networks[0]
		ep := map[string]any{}
		if a.IP != "" {
			ep["IPAMConfig"] = map[string]string{"IPv4Address": a.IP}
		}
		if len(a.Aliases) > 0 {
			ep["Aliases"] = a.Aliases
		}
		host["NetworkMode"] = a.Network
		body["NetworkingConfig"] = map[string]any{"EndpointsConfig": map[string]any{a.Network: ep}}
	}
	var out struct {
		ID string `json:"Id"`
	}
	q := url.Values{"name": {s.Name}}
	if s.Platform != "" {
		q.Set("platform", s.Platform)
	}
	if err := c.do(ctx, http.MethodPost, "/containers/create", q, body, &out); err != nil {
		return "", fmt.Errorf("create container %s: %w", s.Name, err)
	}
	for _, a := range s.Networks[min(1, len(s.Networks)):] {
		if err := c.Connect(ctx, a.Network, out.ID, a.IP, a.Aliases...); err != nil {
			_ = c.RemoveContainer(ctx, out.ID, true)
			return "", fmt.Errorf("connect %s to %s: %w", s.Name, a.Network, err)
		}
	}
	return out.ID, nil
}

// StartContainer starts a created or stopped container.
func (c *Client) StartContainer(ctx context.Context, id string) error {
	if err := c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil, nil, nil); err != nil {
		return err
	}
	c.recordStart(ctx, id)
	return nil
}

// recordStart records when the runtime started container id for the
// client. A container whose start time no longer matches was stopped or
// started again by something else (Disrupted).
func (c *Client) recordStart(ctx context.Context, id string) {
	ct, err := c.InspectContainer(ctx, id)
	if err != nil {
		return
	}
	c.startsMu.Lock()
	defer c.startsMu.Unlock()
	if c.starts == nil {
		c.starts = map[string]start{}
	}
	c.starts[ct.ID] = start{name: ct.Name, at: ct.StartedAt}
}

// forgetStart drops the record of container id, by ID or name, before the
// client stops or removes it.
func (c *Client) forgetStart(id string) {
	c.startsMu.Lock()
	defer c.startsMu.Unlock()
	for k, s := range c.starts {
		if k == id || s.name == id {
			delete(c.starts, k)
		}
	}
}

// Disrupted returns, as "name (exited)" or "name (restarted)", the
// containers among cts that the client started and that have since
// stopped or been started again by something else, such as a restart of
// the runtime daemon (#318). They lost what was set up after the start.
// Each candidate is inspected again, since the client may have started,
// stopped or removed it after cts was listed.
func (c *Client) Disrupted(ctx context.Context, cts []Container) []string {
	var out []string
	for _, ct := range cts {
		if c.disruption(ct) == "" {
			continue
		}
		if now, err := c.InspectContainer(ctx, ct.ID); err == nil {
			ct = now
		}
		if why := c.disruption(ct); why != "" {
			out = append(out, ct.Name+" ("+why+")")
		}
	}
	sort.Strings(out)
	return out
}

// disruption returns why ct, as listed, differs from the client's record
// of starting it ("exited", "dead" or "restarted"), or "".
func (c *Client) disruption(ct Container) string {
	c.startsMu.Lock()
	s, ok := c.starts[ct.ID]
	c.startsMu.Unlock()
	switch {
	case !ok:
		return ""
	case ct.Status == "exited" || ct.Status == "dead":
		return ct.Status
	case ct.StartedAt != s.at:
		return "restarted"
	}
	return ""
}

// StopContainer stops a container, killing it after timeout.
func (c *Client) StopContainer(ctx context.Context, id string, timeout time.Duration) error {
	c.forgetStart(id)
	err := c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/stop",
		url.Values{"t": {strconv.Itoa(int(timeout.Seconds()))}}, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// RestartContainer restarts a container.
func (c *Client) RestartContainer(ctx context.Context, id string, timeout time.Duration) error {
	c.forgetStart(id)
	if err := c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/restart",
		url.Values{"t": {strconv.Itoa(int(timeout.Seconds()))}}, nil, nil); err != nil {
		return err
	}
	c.recordStart(ctx, id)
	return nil
}

// PauseContainer freezes a running container's processes.
func (c *Client) PauseContainer(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/pause", nil, nil, nil)
}

// UnpauseContainer resumes a paused container.
func (c *Client) UnpauseContainer(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/unpause", nil, nil, nil)
}

// RemoveContainer deletes a container (with anonymous volumes); a missing
// container is not an error.
func (c *Client) RemoveContainer(ctx context.Context, id string, force bool) error {
	c.forgetStart(id)
	err := c.do(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id),
		url.Values{"force": {strconv.FormatBool(force)}, "v": {"true"}}, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// InspectContainer returns container state.
func (c *Client) InspectContainer(ctx context.Context, id string) (Container, error) {
	var v struct {
		ID     string `json:"Id"`
		Name   string
		Config struct {
			Image  string
			Labels map[string]string
			Cmd    []string
		}
		State struct {
			Status    string
			Running   bool
			ExitCode  int
			StartedAt string
		}
		NetworkSettings struct {
			Networks map[string]struct{ IPAddress string }
			Ports    map[string][]struct{ HostIp, HostPort string }
		}
	}
	if err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil, nil, &v); err != nil {
		return Container{}, err
	}
	ct := Container{
		ID: v.ID, Name: strings.TrimPrefix(v.Name, "/"), Image: v.Config.Image, Labels: v.Config.Labels,
		Running: v.State.Running, Status: v.State.Status, ExitCode: v.State.ExitCode, StartedAt: v.State.StartedAt,
		IPs: map[string]string{}, Ports: map[string]string{}, Cmd: v.Config.Cmd,
	}
	for n, ep := range v.NetworkSettings.Networks {
		ct.IPs[n] = ep.IPAddress
	}
	for p, bs := range v.NetworkSettings.Ports {
		for _, b := range bs {
			if strings.Contains(b.HostIp, ".") || b.HostIp == "" {
				ip := b.HostIp
				if ip == "" || ip == "0.0.0.0" {
					ip = "127.0.0.1"
				}
				ct.Ports[p] = net.JoinHostPort(ip, b.HostPort)
				break
			}
		}
	}
	return ct, nil
}

// ListContainers returns all containers (running or not) carrying all labels.
func (c *Client) ListContainers(ctx context.Context, labels map[string]string) ([]Container, error) {
	var v []struct {
		ID string `json:"Id"`
	}
	q := url.Values{"all": {"true"}, "filters": {labelFilter(labels)}}
	if err := c.do(ctx, http.MethodGet, "/containers/json", q, nil, &v); err != nil {
		return nil, err
	}
	var out []Container
	for _, s := range v {
		if ct, err := c.InspectContainer(ctx, s.ID); err == nil {
			out = append(out, ct)
		}
	}
	return out, nil
}

// File is a file to copy into a container.
type File struct {
	Name string // path relative to the destination directory
	Mode int64
	Data []byte
	// Path, if set, is read from the local filesystem instead of Data.
	Path string
}

// CopyTo writes files into dir inside the container (which may be created
// but not started).
func (c *Client) CopyTo(ctx context.Context, id, dir string, files []File) error {
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		err := func() error {
			for _, f := range files {
				var r io.Reader = bytes.NewReader(f.Data)
				size := int64(len(f.Data))
				if f.Path != "" {
					fh, err := os.Open(f.Path)
					if err != nil {
						return err
					}
					defer fh.Close()
					fi, err := fh.Stat()
					if err != nil {
						return err
					}
					r, size = fh, fi.Size()
				}
				mode := f.Mode
				if mode == 0 {
					mode = 0o644
				}
				if err := tw.WriteHeader(&tar.Header{Name: f.Name, Mode: mode, Size: size, ModTime: time.Now(), Typeflag: tar.TypeReg}); err != nil {
					return err
				}
				if _, err := io.Copy(tw, r); err != nil {
					return err
				}
			}
			return tw.Close()
		}()
		pw.CloseWithError(err)
	}()
	return c.ArchiveTo(ctx, id, dir, pr)
}

// ArchiveTo extracts the tar stream r into dir inside the container (which
// may be created but not started), volumes mounted there included.
func (c *Client) ArchiveTo(ctx context.Context, id, dir string, r io.Reader) error {
	resp, err := c.raw(ctx, http.MethodPut, "/containers/"+url.PathEscape(id)+"/archive",
		url.Values{"path": {dir}}, r, "application/x-tar")
	if err != nil {
		return fmt.Errorf("copy to %s:%s: %w", id, dir, err)
	}
	resp.Body.Close()
	return nil
}

// ArchiveFrom writes p (a file or a whole directory) inside the container
// to w as a tar stream whose entries are named under p's base name.
func (c *Client) ArchiveFrom(ctx context.Context, id, p string, w io.Writer) error {
	resp, err := c.raw(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/archive",
		url.Values{"path": {p}}, nil, "")
	if err != nil {
		return fmt.Errorf("copy from %s:%s: %w", id, p, err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(w, resp.Body); err != nil {
		return fmt.Errorf("copy from %s:%s: %w", id, p, err)
	}
	return nil
}

// CopyFrom reads a single file from the container.
func (c *Client) CopyFrom(ctx context.Context, id, file string) ([]byte, error) {
	resp, err := c.raw(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/archive",
		url.Values{"path": {file}}, nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	tr := tar.NewReader(resp.Body)
	for {
		h, err := tr.Next()
		if err != nil {
			return nil, fmt.Errorf("copy from %s:%s: %w", id, file, err)
		}
		if h.Typeflag == tar.TypeReg && path.Base(h.Name) == path.Base(file) {
			return io.ReadAll(tr)
		}
	}
}

// ExecResult is the outcome of Exec.
type ExecResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// Exec runs cmd in a running container and waits for it.
func (c *Client) Exec(ctx context.Context, id string, cmd []string, stdin []byte) (ExecResult, error) {
	var created struct {
		ID string `json:"Id"`
	}
	body := map[string]any{"Cmd": cmd, "AttachStdout": true, "AttachStderr": true, "AttachStdin": stdin != nil}
	if err := c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/exec", nil, body, &created); err != nil {
		return ExecResult{}, err
	}
	res, err := c.execStart(ctx, created.ID, stdin)
	if err != nil {
		return res, err
	}
	var st struct{ ExitCode int }
	if err := c.do(ctx, http.MethodGet, "/exec/"+created.ID+"/json", nil, nil, &st); err != nil {
		return res, err
	}
	res.ExitCode = st.ExitCode
	return res, nil
}

// execStart starts an exec and demultiplexes its output. Stdin requires a
// hijacked connection, so it is done over a raw connection.
func (c *Client) execStart(ctx context.Context, execID string, stdin []byte) (ExecResult, error) {
	var res ExecResult
	payload, _ := json.Marshal(map[string]any{"Detach": false, "Tty": false})
	if stdin == nil {
		resp, err := c.raw(ctx, http.MethodPost, "/exec/"+execID+"/start", nil, bytes.NewReader(payload), "application/json")
		if err != nil {
			return res, err
		}
		defer resp.Body.Close()
		err = demux(resp.Body, &res)
		return res, err
	}
	conn, br, stop, err := c.hijack(ctx, "/exec/"+execID+"/start", payload)
	if err != nil {
		return res, err
	}
	defer stop()
	defer conn.Close()
	go func() {
		_, _ = conn.Write(stdin)
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	if err = demux(br, &res); err != nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	return res, err
}

// hijack issues an upgrade request and returns the raw connection, which is
// closed when ctx is done until stop is called.
func (c *Client) hijack(ctx context.Context, p string, payload []byte) (net.Conn, *bufio.Reader, func() bool, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	fail := func(err error) (net.Conn, *bufio.Reader, func() bool, error) {
		stop()
		conn.Close()
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, nil, nil, err
	}
	req := fmt.Sprintf("POST /%s%s HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: %d\r\n\r\n",
		apiVersion, p, c.host, len(payload))
	if _, err := conn.Write(append([]byte(req), payload...)); err != nil {
		return fail(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return fail(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols && resp.StatusCode != http.StatusOK {
		return fail(fmt.Errorf("exec start: HTTP %d", resp.StatusCode))
	}
	return conn, br, stop, nil
}

// demux splits Docker's multiplexed stdout/stderr stream.
func demux(r io.Reader, res *ExecResult) error {
	var stdout, stderr bytes.Buffer
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return err
		}
		n := binary.BigEndian.Uint32(hdr[4:])
		dst := &stdout
		if hdr[0] == 2 {
			dst = &stderr
		}
		if _, err := io.CopyN(dst, r, int64(n)); err != nil {
			return err
		}
	}
	res.Stdout, res.Stderr = stdout.Bytes(), stderr.Bytes()
	return nil
}

// Logs returns the last tail lines of a container's combined output.
func (c *Client) Logs(ctx context.Context, id string, tail int) (string, error) {
	q := url.Values{"stdout": {"true"}, "stderr": {"true"}, "tail": {strconv.Itoa(tail)}}
	resp, err := c.raw(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/logs", q, nil, "")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var res ExecResult
	if err := demux(resp.Body, &res); err != nil {
		return "", err
	}
	return string(res.Stdout) + string(res.Stderr), nil
}
