package certs

import (
	"context"
	"encoding/base64"
	"regexp"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/locations"
)

// API hosts.
const (
	cmHost = "certificatemanager.googleapis.com"
	nsHost = "networksecurity.googleapis.com"
)

// kind describes one resource collection of either API. Resources are
// stored as their REST JSON representation (google.golang.org/api types),
// which follows the current googleapis protos more closely than the Go
// gRPC stubs (e.g. TrustConfig.allowlistedCertificates, scope CLIENT_AUTH);
// the gRPC surface converts to and from it.
type kind struct {
	api  string // "certificatemanager" | "networksecurity"
	coll string // collection segment, e.g. "certificates"
	// parentColl is the parent collection for nested kinds
	// ("certificateMaps" for certificateMapEntries).
	parentColl string
	// idParam is the create request's ID field (REST query parameter).
	idParam string
	// perm is the IAM permission prefix, e.g. "certificatemanager.certs".
	perm string
	// title names the kind in messages.
	title string
	// globalOnly kinds exist only in location "global".
	globalOnly bool
	// etag kinds carry a server-computed etag checked on update/delete.
	etag bool
	// output lists output-only top-level JSON fields ignored on input.
	output []string
	// inputOnly lists fields accepted on input but never returned.
	inputOnly []string
	// immutable lists dotted JSON paths that cannot change on update.
	immutable []string

	// newREST returns the google.golang.org/api struct of the resource
	// (validation and normalisation); newProto the gRPC message.
	newREST  func() any
	newProto func() proto.Message
}

func (k *kind) host() string {
	if k.api == "networksecurity" {
		return nsHost
	}
	return cmHost
}

var (
	kCert = &kind{api: "certificatemanager", coll: "certificates", idParam: "certificateId", perm: "certificatemanager.certs", title: "Certificate",
		output: []string{"sanDnsnames", "pemCertificate", "expireTime", "usedBy"}, inputOnly: []string{"selfManaged", "tags"}}
	kMap = &kind{api: "certificatemanager", coll: "certificateMaps", idParam: "certificateMapId", perm: "certificatemanager.certmaps", title: "CertificateMap",
		globalOnly: true, output: []string{"gclbTargets"}, inputOnly: []string{"tags"}}
	kEntry = &kind{api: "certificatemanager", coll: "certificateMapEntries", parentColl: "certificateMaps", idParam: "certificateMapEntryId",
		perm: "certificatemanager.certmapentries", title: "CertificateMapEntry", globalOnly: true, output: []string{"state"}}
	kDNSAuth = &kind{api: "certificatemanager", coll: "dnsAuthorizations", idParam: "dnsAuthorizationId", perm: "certificatemanager.dnsauthorizations",
		title: "DnsAuthorization", output: []string{"dnsResourceRecord"}, inputOnly: []string{"tags"}}
	kTrust = &kind{api: "certificatemanager", coll: "trustConfigs", idParam: "trustConfigId", perm: "certificatemanager.trustconfigs", title: "TrustConfig",
		etag: true, inputOnly: []string{"tags"}}
	kIssuance = &kind{api: "certificatemanager", coll: "certificateIssuanceConfigs", idParam: "certificateIssuanceConfigId",
		perm: "certificatemanager.certissuanceconfigs", title: "CertificateIssuanceConfig", inputOnly: []string{"tags"}}

	kBAC = &kind{api: "networksecurity", coll: "backendAuthenticationConfigs", idParam: "backendAuthenticationConfigId",
		perm: "networksecurity.backendAuthenticationConfigs", title: "BackendAuthenticationConfig", etag: true}
	kServerTLS = &kind{api: "networksecurity", coll: "serverTlsPolicies", idParam: "serverTlsPolicyId",
		perm: "networksecurity.serverTlsPolicies", title: "ServerTlsPolicy"}
	kClientTLS = &kind{api: "networksecurity", coll: "clientTlsPolicies", idParam: "clientTlsPolicyId",
		perm: "networksecurity.clientTlsPolicies", title: "ClientTlsPolicy"}

	allKinds = []*kind{kCert, kMap, kEntry, kDNSAuth, kTrust, kIssuance, kBAC, kServerTLS, kClientTLS}
)

// kindFor returns the kind of a collection segment in api.
func kindFor(api, coll string) *kind {
	for _, k := range allKinds {
		if k.api == api && k.coll == coll {
			return k
		}
	}
	return nil
}

// resName is a parsed resource or collection name.
type resName struct {
	Project, Location string
	// Parent is the parent resource ID for nested kinds (certificate map).
	Parent string
	ID     string
}

// parent returns "projects/P/locations/L" (plus the parent resource for
// nested kinds).
func (n resName) parent(k *kind) string {
	p := "projects/" + n.Project + "/locations/" + n.Location
	if k.parentColl != "" {
		p += "/" + k.parentColl + "/" + n.Parent
	}
	return p
}

// name returns the full relative resource name.
func (n resName) name(k *kind) string { return n.parent(k) + "/" + k.coll + "/" + n.ID }

// fullName returns the IAM full resource name.
func (n resName) fullName(k *kind) string { return "//" + k.host() + "/" + n.name(k) }

// locationFull returns the IAM full name of the location (create/list checks).
func (n resName) locationFull(k *kind) string {
	return "//" + k.host() + "/projects/" + n.Project + "/locations/" + n.Location
}

// canonical strips URL and full-resource-name prefixes from a reference
// ("//certificatemanager.googleapis.com/projects/...",
// "https://certificatemanager.googleapis.com/v1/projects/...").
func canonical(ref string) string {
	ref = strings.TrimSpace(ref)
	for _, p := range []string{"https://", "http://"} {
		if rest, ok := strings.CutPrefix(ref, p); ok {
			if i := strings.Index(rest, "/projects/"); i >= 0 {
				return rest[i+1:]
			}
		}
	}
	if rest, ok := strings.CutPrefix(ref, "//"); ok {
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			return rest[i+1:]
		}
	}
	return strings.TrimPrefix(ref, "/")
}

// parseName parses a resource name of kind k.
func parseName(k *kind, name string) (resName, error) {
	segs := strings.Split(canonical(name), "/")
	want := 6
	if k.parentColl != "" {
		want = 8
	}
	bad := apierr.InvalidArgument("Invalid resource name %q; expected %s.", name, namePattern(k))
	if len(segs) != want || segs[0] != "projects" || segs[2] != "locations" || segs[want-2] != k.coll {
		return resName{}, bad
	}
	n := resName{Project: segs[1], Location: segs[3], ID: segs[want-1]}
	if k.parentColl != "" {
		if segs[4] != k.parentColl {
			return resName{}, bad
		}
		n.Parent = segs[5]
	}
	if n.Project == "" || n.Location == "" || n.ID == "" || (k.parentColl != "" && n.Parent == "") {
		return resName{}, bad
	}
	return n, nil
}

// parseParent parses the parent of a collection of kind k.
func parseParent(k *kind, parent string) (resName, error) {
	segs := strings.Split(canonical(parent), "/")
	want := 4
	if k.parentColl != "" {
		want = 6
	}
	if len(segs) != want || segs[0] != "projects" || segs[2] != "locations" || segs[1] == "" || segs[3] == "" ||
		(k.parentColl != "" && (segs[4] != k.parentColl || segs[5] == "")) {
		return resName{}, apierr.InvalidArgument("Invalid parent %q.", parent)
	}
	n := resName{Project: segs[1], Location: segs[3]}
	if k.parentColl != "" {
		n.Parent = segs[5]
	}
	return n, nil
}

func namePattern(k *kind) string {
	if k.parentColl != "" {
		return "projects/*/locations/*/" + k.parentColl + "/*/" + k.coll + "/*"
	}
	return "projects/*/locations/*/" + k.coll + "/*"
}

var idRE = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

// validID checks a user-supplied resource ID (RFC 1035 label, as the
// Certificate Manager and Network Security APIs require).
func validID(k *kind, id string) error {
	if id == "" {
		return apierr.InvalidArgument("Field %s is required.", k.idParam)
	}
	if !idRE.MatchString(id) {
		return apierr.InvalidArgument("Invalid %s %q: must be 1-63 characters long, start with a lowercase letter and contain only lowercase letters, digits and hyphens.", k.idParam, id)
	}
	return nil
}

// checkLocation validates a location of kind k ("global" or a region; "-"
// is accepted by list when allowWild).
func checkLocation(k *kind, loc string, allowWild bool) error {
	if loc == "-" && allowWild {
		return nil
	}
	if loc == "global" || (!k.globalOnly && locations.IsRegion(loc)) {
		return nil
	}
	if k.globalOnly && locations.IsRegion(loc) {
		return apierr.InvalidArgument("%s resources are only supported in location \"global\", got %q.", k.title, loc)
	}
	return apierr.InvalidArgument("Location %q is not supported.", loc)
}

// access validates the project and location and checks perm on resource.
func (s *Service) access(ctx context.Context, n resName, k *kind, perm, resource string, allowWild bool) error {
	if err := s.env.EnsureProject(n.Project); err != nil {
		return err
	}
	if err := checkLocation(k, n.Location, allowWild); err != nil {
		return err
	}
	return s.env.Auth.Check(ctx, perm, resource)
}

func notFound(name string) error {
	return apierr.NotFound("Resource '%s' was not found", name)
}

// ---- pagination ----

const maxPageSize = 1000

func pageBounds(token string, size int32, total int) (int, int, error) {
	start := 0
	if token != "" {
		b, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil {
			return 0, 0, apierr.InvalidArgument("Invalid page token.")
		}
		if start, err = strconv.Atoi(string(b)); err != nil || start < 0 {
			return 0, 0, apierr.InvalidArgument("Invalid page token.")
		}
	}
	n := int(size)
	if n <= 0 || n > maxPageSize {
		n = maxPageSize
	}
	return min(start, total), n, nil
}

// page slices items by token and size.
func page[T any](items []T, token string, size int32) ([]T, string, error) {
	start, n, err := pageBounds(token, size, len(items))
	if err != nil {
		return nil, "", err
	}
	end := min(start+n, len(items))
	next := ""
	if end < len(items) {
		next = base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(end)))
	}
	return items[start:end], next, nil
}
