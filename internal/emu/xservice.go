package emu

import (
	"context"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/project"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Cross-service contracts. A service obtains a peer with env.Lookup(name)
// and a type assertion to one of these interfaces; a missing peer (service
// not running) must degrade gracefully.

// ServiceAccountKeys is provided by the "iam" service.
type ServiceAccountKeys interface {
	// PublicKeys returns every active public key of a service account
	// (used to verify V4 signed URLs, FR-GCS-006).
	PublicKeys(ctx context.Context, email string) ([]*rsa.PublicKey, error)
	// SignBlob signs data with one of the account's keys (RSA-SHA256).
	SignBlob(ctx context.Context, email string, data []byte) (keyID string, sig []byte, err error)
	// IDToken mints an OIDC ID token for email with the given audience,
	// verifiable against the emulator's JWKS (FR-IAM-007, FR-PS-006).
	IDToken(ctx context.Context, email, audience string) (string, error)
	// AccessToken mints an access token for a principal (e.g. "serviceAccount:x").
	AccessToken(ctx context.Context, principal Principal) (token string, expirySeconds int, err error)
}

// IAMPolicyStore is provided by the "iam" service: resource-level IAM
// policies for services that support get/setIamPolicy (FR-IAM-003).
// resource is a full resource name, e.g.
// "//storage.googleapis.com/projects/_/buckets/b".
type IAMPolicyStore interface {
	GetPolicyJSON(ctx context.Context, resource string) ([]byte, error)
	SetPolicyJSON(ctx context.Context, resource string, policy []byte) ([]byte, error)
	DeletePolicy(ctx context.Context, resource string) error
}

// IAMResourceParents is optionally provided by the "iam" service alongside
// IAMPolicyStore. Owning services call SetResourceParent when they create a
// resource whose full name does not contain its project (e.g. a bucket:
// SetResourceParent(ctx, "//storage.googleapis.com/projects/_/buckets/b",
// "projects/my-project")) so that project-level bindings apply to it.
// parent is "projects/P" or a full resource name. DeletePolicy also removes
// the parent mapping, so call it when the resource is deleted.
type IAMResourceParents interface {
	SetResourceParent(ctx context.Context, resource, parent string) error
}

// IAMPermissionTester is optionally provided by the "iam" service for
// resource-level testIamPermissions: it returns the subset of permissions
// the caller in ctx holds on resource (policy evaluation, ignoring mode).
type IAMPermissionTester interface {
	TestPermissions(ctx context.Context, resource string, permissions []string) []string
}

// Publisher is provided by the "pubsub" service (FR-GCS-007, FR-INT-010).
type Publisher interface {
	// PublishInternal publishes to topic ("projects/P/topics/T") and returns
	// the message ID.
	PublishInternal(ctx context.Context, topic string, data []byte, attrs map[string]string) (string, error)
	// TopicExists reports whether the topic exists.
	TopicExists(ctx context.Context, topic string) bool
}

// ObjectStore is provided by the "gcs" service: Cloud Storage objects for
// other Services in process, such as Cloud Build's sources and logs. It
// makes no IAM checks; the calling Service checks its own permissions.
// Errors are *apierr.Error.
type ObjectStore interface {
	// ReadObject opens generation gen of an object, or its live
	// generation when gen is 0, and returns the generation it opened. An
	// object or generation that does not exist is NOT_FOUND.
	ReadObject(ctx context.Context, bucket, name string, gen int64) (io.ReadCloser, int64, error)
	// AppendObject adds data to the end of an object, creating it with
	// contentType when absent. Each call makes a new generation holding
	// everything appended so far, so a reader polling the live object
	// sees it grow, as with a log written while it is read.
	AppendObject(ctx context.Context, bucket, name, contentType string, data []byte) error
	// WriteObject stores r as the object, replacing any live generation,
	// and returns the new generation and the data's MD5.
	WriteObject(ctx context.Context, bucket, name, contentType string, r io.Reader) (gen int64, md5 []byte, err error)
	// EnsureServiceBucket creates a bucket in project unless it exists,
	// without the naming rule users are held to: a bucket Google itself
	// names, such as Cloud Build's default logs bucket, may contain
	// "google".
	EnsureServiceBucket(ctx context.Context, project, name string) error
}

// SQLUsers is provided by the "sql" service for Secret Manager's managed
// rotation of Cloud SQL credentials.
type SQLUsers interface {
	// InstanceRegion returns a Cloud SQL instance's region.
	InstanceRegion(project, instance string) (string, error)
	// SetUserPassword sets a built-in user's password on a running
	// Cloud SQL instance.
	SetUserPassword(ctx context.Context, project, instance, user, password string) error
}

// SecretAccessor is provided by the "secrets" service for GKE's Secret
// Manager add-on and secret synchronization.
type SecretAccessor interface {
	// AccessSecretVersion returns the payload of a secret version
	// ("projects/P/[locations/L/]secrets/S/versions/V") and the version's
	// full name, checking secretmanager.versions.access for the Principal
	// in ctx.
	AccessSecretVersion(ctx context.Context, name string) (payload []byte, version string, err error)
}

const projectsNS = "core/projects"

// EnsureProject implements FR-CORE-020: any valid project ID is
// auto-created on first use unless --strict-projects is set, in which case
// it must be declared in config or the seed.
func (e *Env) EnsureProject(id string) error {
	if !project.ValidID(id) && !isNumeric(id) {
		return apierr.InvalidArgument("Invalid project ID %q.", id).WithReason("googleapis.com", "INVALID_PROJECT_ID")
	}
	for _, p := range e.Config.Projects {
		if p == id {
			return nil
		}
	}
	var known bool
	_ = e.Store.View(func(tx store.Tx) error { known = store.Exists(tx, projectsNS, id); return nil })
	if known {
		return nil
	}
	if e.Config.StrictProjects {
		return apierr.NotFound("Project %s not found.", id).WithReason("googleapis.com", "PROJECT_NOT_FOUND")
	}
	return e.Store.Update(func(tx store.Tx) error {
		return store.PutJSON(tx, projectsNS, id, map[string]any{
			"projectId":     id,
			"projectNumber": project.NumberString(id),
			"createTime":    e.Clock.Now(),
		})
	})
}

// ProjectByNumber maps a project number to a known project ID.
func (e *Env) ProjectByNumber(num string) (string, bool) {
	for _, p := range e.Config.Projects {
		if project.NumberString(p) == num {
			return p, true
		}
	}
	var id string
	_ = e.Store.View(func(tx store.Tx) error {
		tx.Scan(projectsNS, "", func(k string, _ []byte) bool {
			if project.NumberString(k) == num {
				id = k
				return false
			}
			return true
		})
		return nil
	})
	return id, id != ""
}

// Projects returns the IDs of every Project the Instance knows, in order.
func (e *Env) Projects() []string {
	var ids []string
	_ = e.Store.View(func(tx store.Tx) error {
		tx.Scan(projectsNS, "", func(k string, _ []byte) bool { ids = append(ids, k); return true })
		return nil
	})
	return ids
}

// DeclareProject records a project explicitly (seed files, config).
func (e *Env) DeclareProject(id string) error {
	return e.Store.Update(func(tx store.Tx) error {
		return store.PutJSON(tx, projectsNS, id, map[string]any{
			"projectId":     id,
			"projectNumber": project.NumberString(id),
			"createTime":    e.Clock.Now(),
		})
	})
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// SubnetNet is a VPC subnetwork realised as a container network.
//
// Addresses are reported from Subnet, and the emulator dials a container
// at its address on the container network (as the runtime reports it),
// never at a reported address: on a Translated network only workloads on
// the VPC can reach those (ADR 0004).
type SubnetNet struct {
	// Name is the runtime network name.
	Name string
	// Subnet is the range that addresses from AllocateIP come from, and
	// that the emulator reports: the Declared range (the subnetwork's
	// ipCidrRange or the private services access allocation). Only when
	// the Declared range overlapped another network on the host and the
	// network could not be Translated is it the Fallback range.
	Subnet string
	// ContainerSubnet is the CIDR the container network actually uses. It
	// differs from Subnet only on a Translated network.
	ContainerSubnet string
	// Translated is set when the container network is on a Fallback range
	// while its Managed containers hold Declared addresses: such a
	// container attaches without a fixed address, needs NET_ADMIN, and
	// runs AliasScript once started (and again after a restart).
	Translated bool
	// Gateway is the host-side address of the container network (the
	// .1 of ContainerSubnet, like GCP's gateway).
	Gateway string
	// SubnetworkName is "projects/P/regions/R/subnetworks/S".
	SubnetworkName string
	// NetworkName is "projects/P/global/networks/N".
	NetworkName string
	Region      string
	// SecondaryRanges maps rangeName to CIDR (GKE pods/services ranges).
	SecondaryRanges map[string]string
}

// AliasVLAN is the VLAN that carries Declared addresses on a Translated
// network's bridge. Tagged frames bypass the host's iptables (with the
// default bridge-nf-filter-vlan-tagged=0), so the host's rules for the
// network that holds the Declared range, such as its MASQUERADE, do not
// rewrite them.
const AliasVLAN = 4000

// AliasScript returns sh commands (iproute2 or busybox ip) that give a
// Managed container on a Translated network its Declared address ip: on a
// VLAN (AliasVLAN) of the interface holding cip, its address on the
// container network, with an on-link route for the Declared range sourced
// from ip. Each of routes ("default", or a CIDR) then goes via the NAT egress
// proxy's Declared address, also sourced from ip. The commands are
// idempotent. It returns "" when n is not Translated.
func (n SubnetNet) AliasScript(ip, cip string, routes ...string) string {
	if !n.Translated {
		return ""
	}
	_, ipn, err := net.ParseCIDR(n.Subnet)
	if err != nil || ipn.IP.To4() == nil {
		return ""
	}
	ones, bits := ipn.Mask.Size()
	tail := make(net.IP, 4)
	for i, v := range ipn.IP.To4() {
		tail[i] = v | ^ipn.Mask[i]
	}
	tail[3]-- // GCP's reserved second-to-last address, the NAT egress proxy
	if ones >= bits-1 {
		routes = nil
	}
	var b strings.Builder
	b.WriteString("set -e\n")
	fmt.Fprintf(&b, "IF=$(ip -o -4 addr show | awk -v a=%s '{split($4,x,\"/\"); if (x[1]==a) print $2}' | head -n1)\n", cip)
	fmt.Fprintf(&b, "[ -n \"$IF\" ] || { echo 'no interface holds %s' >&2; exit 1; }\n", cip)
	fmt.Fprintf(&b, "V=\"$IF.%d\"\n", AliasVLAN)
	fmt.Fprintf(&b, "ip link show \"$V\" >/dev/null 2>&1 || ip link add link \"$IF\" name \"$V\" type vlan id %d || { echo '%s' >&2; exit 1; }\n", AliasVLAN, aliasVLANFailed)
	b.WriteString("ip link set \"$V\" up\n")
	fmt.Fprintf(&b, "ip -o -4 addr show dev \"$V\" | grep -q ' inet %s/' || ip addr add %s/%d dev \"$V\"\n", ip, ip, ones)
	fmt.Fprintf(&b, "ip route replace %s dev \"$V\" src %s\n", ipn.String(), ip)
	for _, r := range routes {
		fmt.Fprintf(&b, "ip route replace %s via %s dev \"$V\" src %s\n", r, tail, ip)
	}
	return b.String()
}

// aliasVLANFailed marks AliasScript's output when creating the VLAN link
// failed, so AliasError can tell that step from the others.
const aliasVLANFailed = "gcpemu: creating the VLAN link failed"

// noVLAN are what ip prints when the kernel cannot create a VLAN link
// (no 8021q).
var noVLAN = []string{"Unknown device type", "Operation not supported", "RTNETLINK answers: Not supported"}

// NoVLANError reports that the kernel of the container runtime cannot
// create the VLAN link that a Translated network carries its Declared
// addresses on (ADR 0004). Only that network fails; every other feature
// works.
type NoVLANError struct {
	// Runtime names the container runtime (runtime.Info.String()).
	Runtime string
	// Network is the Translated network's runtime name.
	Network string
}

func (e *NoVLANError) Error() string {
	rt := e.Runtime
	if rt == "" {
		rt = "the container runtime"
	}
	return fmt.Sprintf("the kernel of %s has no VLAN (8021q) support, so Translated network %s can't be used on it: "+
		"give the subnetwork a range that doesn't overlap the host's networks, or use a runtime whose kernel has 8021q", rt, e.Network)
}

// AliasError classifies the failure of n's AliasScript, given its output
// and the error running it: a kernel without VLAN support becomes a
// NoVLANError naming runtime, and anything else is err unchanged.
func (n SubnetNet) AliasError(runtime, out string, err error) error {
	if err == nil || !strings.Contains(out, aliasVLANFailed) {
		return err
	}
	for _, s := range noVLAN {
		if strings.Contains(out, s) {
			return &NoVLANError{Runtime: runtime, Network: n.Name}
		}
	}
	return err
}

// PublicRouter is implemented by services whose Managed containers send
// traffic for public addresses over the external network: GKE nodes with
// external IPs, and the NAT egress proxies. The external network's
// gateway is the host, which cannot serve the load balancer's forwarding
// rule addresses, so the load balancer has its Edge relay serve them on
// the external network at via and asks for host routes to ips via it
// (#187). An empty via removes the routes. Implementations apply the
// routes to containers they start later, too.
type PublicRouter interface {
	RoutePublic(ctx context.Context, via string, ips []string) error
}

// PublicRouteProto tags the routes PublicRoutesScript programs, so that a
// later script finds and removes the stale ones.
const PublicRouteProto = 245

// PublicRoutesScript returns idempotent sh commands (iproute2) that make
// the host routes to ips via via the only routes tagged PublicRouteProto.
// An empty via removes them all.
func PublicRoutesScript(via string, ips []string) string {
	if via == "" {
		ips = nil
	}
	want := strings.Join(ips, " ")
	var b strings.Builder
	fmt.Fprintf(&b, "for r in $(ip -4 route show proto %d | awk '{print $1}'); do case \" %s \" in *\" $r \"*) ;; *) ip route del \"$r\" proto %d ;; esac; done\n", PublicRouteProto, want, PublicRouteProto)
	for _, ip := range ips {
		fmt.Fprintf(&b, "ip route replace %s/32 via %s proto %d\n", ip, via, PublicRouteProto)
	}
	return b.String()
}

// NEGEndpoint is a GCE_VM_IP_PORT network endpoint.
type NEGEndpoint struct {
	IP       string
	Port     int
	Instance string // node name
}

// VPC is provided by the "compute" service. It realises VPC subnetworks as
// container networks (Section 3.3), runs the NAT egress proxy
// (FR-NAT-002) and owns network endpoint groups (FR-GKE-008).
type VPC interface {
	// ResolveSubnetwork returns the canonical subnetwork name
	// ("projects/P/regions/R/subnetworks/S") for a workload in region that
	// names a network and/or subnetwork (short names, partial or full URLs).
	// With neither, the project's "default" auto-mode network is used and
	// created on first use, as in a new GCP project.
	ResolveSubnetwork(ctx context.Context, project, region, network, subnetwork string) (string, error)
	// SubnetNetwork realises a subnetwork as a container network on first use.
	SubnetNetwork(ctx context.Context, subnetwork string) (SubnetNet, error)
	// PrivateServicesNetwork realises a VPC's private services access range
	// (allocated by a servicenetworking connection) for Cloud SQL private IP.
	// It returns FAILED_PRECONDITION when the VPC has no such connection.
	PrivateServicesNetwork(ctx context.Context, network string) (SubnetNet, error)
	// AllocateIP reserves a free address in net for owner (idempotent per
	// owner); ReleaseIP frees it.
	AllocateIP(ctx context.Context, net SubnetNet, owner string) (string, error)
	ReleaseIP(ctx context.Context, net SubnetNet, owner string) error
	// EgressProxy returns the address workloads without external IPs on
	// subnetwork must use as their default route. Traffic is forwarded to the
	// internet only while a Cloud NAT covers the subnetwork (FR-NAT-002).
	EgressProxy(ctx context.Context, subnetwork string) (string, error)
	// UpsertNEG creates a zonal GCE_VM_IP_PORT NEG if absent; SetNEGEndpoints
	// replaces its endpoints; DeleteNEG removes it. Used by GKE NEG sync.
	UpsertNEG(ctx context.Context, project, zone, name, network, subnetwork string, defaultPort int, description string) error
	SetNEGEndpoints(ctx context.Context, project, zone, name string, eps []NEGEndpoint) error
	DeleteNEG(ctx context.Context, project, zone, name string) error
}

// LoadBalancerResources is provided by the "lb" service: GKE's Ingress and
// Gateway controllers (FR-GKE-010) manage load-balancing resources through
// it, as GKE's service agent does through the compute API. path is a
// resource's relative name ("projects/P/global/urlMaps/M" or
// "projects/P/regions/R/backendServices/B"); bodies are its compute v1 JSON
// form. Errors are *apierr.Error.
type LoadBalancerResources interface {
	// PutLBResource creates the resource, or replaces every mutable field
	// of an existing one, with the API's validation and defaults, and
	// returns its stored form. A replaced forwarding rule keeps its address
	// unless the body gives one.
	PutLBResource(ctx context.Context, path string, body []byte) ([]byte, error)
	// LBResource returns a resource's stored form, or NOT_FOUND.
	LBResource(ctx context.Context, path string) ([]byte, error)
	// DeleteLBResource removes a resource; a missing one is not an error.
	// It fails with FAILED_PRECONDITION while another resource uses it.
	DeleteLBResource(ctx context.Context, path string) error
}

// CertManager is provided by the "certs" service: Certificate Manager
// (certificates, certificate maps, trust configs, DNS authorizations) and
// Network Security (backend authentication configs, server TLS policies)
// as the load balancer data plane consumes them (FR-LB-004, FR-LB-006,
// FR-LB-009). Names are full resource names
// ("projects/P/locations/L/certificateMaps/M"); errors are *apierr.Error.
type CertManager interface {
	// MapCertificate selects the certificate for sni from a certificate map
	// using Certificate Manager's matching rules (exact hostname, wildcard,
	// then PRIMARY entry). It returns nil without error when no entry matches.
	MapCertificate(ctx context.Context, certificateMap, sni string) (*tls.Certificate, error)
	// Certificate returns an ACTIVE Certificate Manager certificate.
	Certificate(ctx context.Context, name string) (*tls.Certificate, error)
	// BackendAuthentication resolves a BackendAuthenticationConfig: the client
	// certificate the LB presents to backends (nil if none) and the roots used
	// to verify backend server certificates (nil means "public roots" per
	// wellKnownRoots; an empty non-nil pool means none configured).
	BackendAuthentication(ctx context.Context, name string) (client *tls.Certificate, roots *x509.CertPool, err error)
	// ServerTLSPolicy resolves frontend mTLS settings: roots that validate
	// client certificates and the clientValidationMode
	// ("ALLOW_INVALID_OR_MISSING_CLIENT_CERT" or "REJECT_INVALID").
	ServerTLSPolicy(ctx context.Context, name string) (clientRoots *x509.CertPool, mode string, err error)
}
