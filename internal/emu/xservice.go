package emu

import (
	"context"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"fmt"
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
// from ip. Each of routes ("default", or a CIDR) then goes via the egress
// gateway's Declared address, also sourced from ip. The commands are
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
	tail[3]-- // GCP's reserved second-to-last address, the egress gateway
	if ones >= bits-1 {
		routes = nil
	}
	var b strings.Builder
	b.WriteString("set -e\n")
	fmt.Fprintf(&b, "IF=$(ip -o -4 addr show | awk -v a=%s '{split($4,x,\"/\"); if (x[1]==a) print $2}' | head -n1)\n", cip)
	fmt.Fprintf(&b, "[ -n \"$IF\" ] || { echo 'no interface holds %s' >&2; exit 1; }\n", cip)
	fmt.Fprintf(&b, "V=\"$IF.%d\"\n", AliasVLAN)
	fmt.Fprintf(&b, "ip link show \"$V\" >/dev/null 2>&1 || ip link add link \"$IF\" name \"$V\" type vlan id %d\n", AliasVLAN)
	b.WriteString("ip link set \"$V\" up\n")
	fmt.Fprintf(&b, "ip -o -4 addr show dev \"$V\" | grep -q ' inet %s/' || ip addr add %s/%d dev \"$V\"\n", ip, ip, ones)
	fmt.Fprintf(&b, "ip route replace %s dev \"$V\" src %s\n", ipn.String(), ip)
	for _, r := range routes {
		fmt.Fprintf(&b, "ip route replace %s via %s dev \"$V\" src %s\n", r, tail, ip)
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
// container networks (Section 3.3), runs the Cloud NAT egress gateway
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
	// EgressGateway returns the address workloads without external IPs on
	// subnetwork must use as their default route. Traffic is forwarded to the
	// internet only while a Cloud NAT covers the subnetwork (FR-NAT-002).
	EgressGateway(ctx context.Context, subnetwork string) (string, error)
	// UpsertNEG creates a zonal GCE_VM_IP_PORT NEG if absent; SetNEGEndpoints
	// replaces its endpoints; DeleteNEG removes it. Used by GKE NEG sync.
	UpsertNEG(ctx context.Context, project, zone, name, network, subnetwork string, defaultPort int, description string) error
	SetNEGEndpoints(ctx context.Context, project, zone, name string, eps []NEGEndpoint) error
	DeleteNEG(ctx context.Context, project, zone, name string) error
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
