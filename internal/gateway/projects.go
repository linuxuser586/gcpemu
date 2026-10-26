package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/linuxuser586/gcpemu/internal/project"
)

// projectSegment matches the Project in a resource name or REST path:
// projects/P, where P is a Project ID or number.
var projectSegment = regexp.MustCompile(`(?:^|/)projects/([^/:?]+)`)

// accountSegment matches a service account named under projects/-, whose
// Project is in its email.
var accountSegment = regexp.MustCompile(`(?:^|/)projects/-/serviceAccounts/([^/:?]+)`)

// accountCerts matches a service account's public certificates (a key
// file's client_x509_cert_url), whose Project is in its email.
var accountCerts = regexp.MustCompile(`^/service_accounts/v1/metadata/(?:x509|jwk)/([^/]+)$`)

// bucketCollection matches Cloud Storage's bucket collection, whose
// project parameter names the Project of the buckets it lists or creates.
var bucketCollection = regexp.MustCompile(`^(/storage)?/storage/v1/b/?$`)

// requestProjects returns the Projects a request names, as IDs or numbers,
// in order and each once: by which the API gateway forwards it to the
// Linked Instance they are delegated to, and by which a Linked Instance
// checks that they are shared with the link it came over (ADR 0027).
//
// A REST request names the Projects of the projects/P in its path, that
// of a service account's email under projects/- or in its certificates'
// path, the bucket collection's project parameter, an XML API bucket's
// x-goog-project-id, or the service account whose key signed a token
// endpoint's JWT-bearer assertion, or the provider an STS token
// exchange's audience names. A gRPC call names those its request
// message's string fields, and those of the messages nested in it, start
// with projects/P, read by the message's registered descriptor, so that
// fields it does not have, bytes and maps name none. The Admin API, the Web console and in-container agents'
// callbacks name none, nor do other projects/-, a bucket, or a gRPC method
// this binary does not know.
func requestProjects(r *http.Request) []string {
	if isGRPC(r) {
		return messageProjects(r)
	}
	if internalPath(r.URL.Path) {
		return nil
	}
	var out []string
	for _, m := range projectSegment.FindAllStringSubmatch(r.URL.EscapedPath(), -1) {
		out = addProject(out, m[1])
	}
	for _, m := range accountSegment.FindAllStringSubmatch(r.URL.EscapedPath(), -1) {
		out = addAccountProject(out, m[1])
	}
	if m := accountCerts.FindStringSubmatch(r.URL.EscapedPath()); m != nil {
		out = addAccountProject(out, m[1])
	}
	switch {
	case len(out) > 0:
	case bucketCollection.MatchString(r.URL.Path):
		out = addProject(out, r.URL.Query().Get("project"))
	case tokenEndpoint(r):
		out = addProject(out, project.OfServiceAccount(assertionIssuer(r)))
	case stsExchange(r):
		out = addProject(out, audienceProject(r))
	case xmlBucket(r.URL.Path):
		out = addProject(out, r.Header.Get("X-Goog-Project-Id"))
	}
	return out
}

// addAccountProject adds to ids the Project in the path-escaped email of
// a service account.
func addAccountProject(ids []string, escaped string) []string {
	email, _ := url.PathUnescape(escaped)
	return addProject(ids, project.OfServiceAccount(email))
}

// xmlBucket reports whether path is an XML API request on a bucket, or
// on the bucket collection, rather than on an object or the JSON API.
func xmlBucket(path string) bool {
	return isXMLPath(path) && !strings.Contains(strings.Trim(path, "/"), "/")
}

// oauth2Hosts serve the token endpoint at their root in Host mode.
var oauth2Hosts = []string{"oauth2.googleapis.com", "accounts.google.com", "openidconnect.googleapis.com"}

// tokenEndpoint reports whether r is a call to the OAuth2 token endpoint.
func tokenEndpoint(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	p := strings.TrimSuffix(r.URL.Path, "/")
	if !slices.Contains(oauth2Hosts, hostOnly(r.Host)) {
		var ok bool
		if p, ok = strings.CutPrefix(p, "/oauth2"); !ok {
			return false
		}
	}
	return p == "/token" || p == "/o/oauth2/token" || p == "/oauth2/v4/token" || p == "/v4/token"
}

// assertionIssuer returns the issuer of the JWT-bearer assertion that the
// token endpoint call r carries, as a form or JSON, unverified, since the
// Instance that holds the key verifies it; "" when it carries none.
func assertionIssuer(r *http.Request) string {
	b := peekBody(r)
	var assertion string
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var m struct {
			GrantType string `json:"grant_type"`
			Assertion string `json:"assertion"`
		}
		if json.Unmarshal(b, &m) != nil || m.GrantType != jwtBearer {
			return ""
		}
		assertion = m.Assertion
	} else {
		q, err := url.ParseQuery(string(b))
		if err != nil || q.Get("grant_type") != jwtBearer {
			return ""
		}
		assertion = q.Get("assertion")
	}
	var claims jwt.RegisteredClaims
	if _, _, err := jwt.NewParser().ParseUnverified(assertion, &claims); err != nil {
		return ""
	}
	return claims.Issuer
}

const jwtBearer = "urn:ietf:params:oauth:grant-type:jwt-bearer"

// stsExchange reports whether r is a call to STS's token exchange.
func stsExchange(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	p := strings.TrimSuffix(r.URL.Path, "/")
	if hostOnly(r.Host) != "sts.googleapis.com" {
		var ok bool
		if p, ok = strings.CutPrefix(p, "/sts"); !ok {
			return false
		}
	}
	return p == "/v1/token" || p == "/v1beta/token"
}

// providerAudience matches the Project of a workload identity pool
// provider an STS exchange's audience names.
var providerAudience = regexp.MustCompile(`^(?:https:)?//iam\.googleapis\.com/projects/([^/]+)/locations/global/workloadIdentityPools/[^/]+/providers/[^/]+$`)

// audienceProject returns the Project of the provider that the STS
// exchange r's audience names, as a form or JSON; "" when it names none.
func audienceProject(r *http.Request) string {
	b := peekBody(r)
	var audience string
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var m struct {
			Audience string `json:"audience"`
		}
		if json.Unmarshal(b, &m) != nil {
			return ""
		}
		audience = m.Audience
	} else {
		q, err := url.ParseQuery(string(b))
		if err != nil {
			return ""
		}
		audience = q.Get("audience")
	}
	if m := providerAudience.FindStringSubmatch(audience); m != nil {
		return m[1]
	}
	return ""
}

// peekBody returns r's body, reading it and putting it back; nil when it
// is larger than maxPeek.
func peekBody(r *http.Request) []byte {
	if r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(r.Body, maxPeek+1))
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(b), r.Body), r.Body}
	if len(b) > maxPeek {
		return nil
	}
	return b
}

// internalPath reports whether path, as unescaped, is the Admin API's, the
// Web console's, or an in-container agent's callback rather than a GCP
// API's.
func internalPath(path string) bool {
	return !isAPIRequest(path) || path == "/console" || strings.HasPrefix(path, "/console/")
}

// addProject adds the Project id to ids unless it is there already or is
// no Project ID or number.
func addProject(ids []string, id string) []string {
	if (project.ValidID(id) || isDigits(id)) && !slices.Contains(ids, id) {
		return append(ids, id)
	}
	return ids
}

// maxNesting bounds how deep messageProjects looks into nested messages.
const maxNesting = 8

// messageProjects returns the Projects that the request message of the
// gRPC call r names.
func messageProjects(r *http.Request) []string {
	service, method, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if !ok {
		return nil
	}
	d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(service))
	if err != nil {
		return nil
	}
	sd, ok := d.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil
	}
	md := sd.Methods().ByName(protoreflect.Name(method))
	if md == nil {
		return nil
	}
	b := firstMessage(r)
	if b == nil {
		return nil
	}
	m := dynamicpb.NewMessage(md.Input())
	if err := (proto.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, m); err != nil {
		return nil
	}
	var out []string
	walkProjects(m, 0, &out)
	return out
}

// walkProjects adds to out the Projects that m's string fields, and those
// of the messages nested in it, name, in the order the fields are
// declared.
func walkProjects(m protoreflect.Message, depth int, out *[]string) {
	fields := m.Descriptor().Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		if !m.Has(fd) {
			continue
		}
		v := m.Get(fd)
		switch {
		case fd.IsMap():
		case fd.IsList():
			l := v.List()
			for i := range l.Len() {
				fieldProjects(fd, l.Get(i), depth, out)
			}
		default:
			fieldProjects(fd, v, depth, out)
		}
	}
}

// fieldProjects adds to out the Project that v, a value of the field fd,
// names.
func fieldProjects(fd protoreflect.FieldDescriptor, v protoreflect.Value, depth int, out *[]string) {
	switch fd.Kind() {
	case protoreflect.StringKind:
		if m := accountSegment.FindStringSubmatch(v.String()); m != nil && strings.HasPrefix(v.String(), "projects/-/") {
			*out = addProject(*out, project.OfServiceAccount(m[1]))
		} else if m := projectSegment.FindStringSubmatch(v.String()); m != nil && strings.HasPrefix(v.String(), "projects/") {
			*out = addProject(*out, m[1])
		}
	case protoreflect.MessageKind, protoreflect.GroupKind:
		if depth < maxNesting {
			walkProjects(v.Message(), depth+1, out)
		}
	}
}

func isDigits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}
