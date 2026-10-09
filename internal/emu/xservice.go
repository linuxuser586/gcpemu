package emu

import (
	"context"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"

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
type SubnetNet struct {
	// Name is the runtime network name.
	Name string
	// Subnet is the CIDR actually used by the container network. It equals
	// the subnetwork's ipCidrRange unless that range overlapped another
	// network on the host, in which case the runtime chose one.
	Subnet string
	// Gateway is the host-side address (the subnet's .1, like GCP's gateway).
	Gateway string
	// SubnetworkName is "projects/P/regions/R/subnetworks/S".
	SubnetworkName string
	// NetworkName is "projects/P/global/networks/N".
	NetworkName string
	Region      string
	// SecondaryRanges maps rangeName to CIDR (GKE pods/services ranges).
	SecondaryRanges map[string]string
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
