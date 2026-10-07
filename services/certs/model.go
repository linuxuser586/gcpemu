package certs

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode"

	"cloud.google.com/go/certificatemanager/apiv1/certificatemanagerpb"
	"cloud.google.com/go/networksecurity/apiv1/networksecuritypb"
	cmv1 "google.golang.org/api/certificatemanager/v1"
	nsv1 "google.golang.org/api/networksecurity/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

func init() {
	kCert.newREST = func() any { return new(cmv1.Certificate) }
	kCert.newProto = func() proto.Message { return new(certificatemanagerpb.Certificate) }
	kCert.immutable = []string{"scope", "managed.domains", "managed.dnsAuthorizations", "managed.issuanceConfig"}
	kMap.newREST = func() any { return new(cmv1.CertificateMap) }
	kMap.newProto = func() proto.Message { return new(certificatemanagerpb.CertificateMap) }
	kEntry.newREST = func() any { return new(cmv1.CertificateMapEntry) }
	kEntry.newProto = func() proto.Message { return new(certificatemanagerpb.CertificateMapEntry) }
	kEntry.immutable = []string{"hostname", "matcher"}
	kDNSAuth.newREST = func() any { return new(cmv1.DnsAuthorization) }
	kDNSAuth.newProto = func() proto.Message { return new(certificatemanagerpb.DnsAuthorization) }
	kDNSAuth.immutable = []string{"domain", "type"}
	kTrust.newREST = func() any { return new(cmv1.TrustConfig) }
	kTrust.newProto = func() proto.Message { return new(certificatemanagerpb.TrustConfig) }
	kIssuance.newREST = func() any { return new(cmv1.CertificateIssuanceConfig) }
	kIssuance.newProto = func() proto.Message { return new(certificatemanagerpb.CertificateIssuanceConfig) }
	kIssuance.immutable = []string{"certificateAuthorityConfig", "lifetime", "rotationWindowPercentage", "keyAlgorithm"}

	kBAC.newREST = func() any { return new(nsv1.BackendAuthenticationConfig) }
	kBAC.newProto = func() proto.Message { return new(networksecuritypb.BackendAuthenticationConfig) }
	kServerTLS.newREST = func() any { return new(nsv1.ServerTlsPolicy) }
	kServerTLS.newProto = func() proto.Message { return new(networksecuritypb.ServerTlsPolicy) }
	kClientTLS.newREST = func() any { return new(nsv1.ClientTlsPolicy) }
	kClientTLS.newProto = func() proto.Message { return new(networksecuritypb.ClientTlsPolicy) }
}

// obj is a resource in its REST JSON form.
type obj = map[string]any

// decodeREST parses a REST request body into a resource of kind k,
// rejecting unknown fields like ESF does, and returns the normalised JSON
// object (unset fields dropped).
func decodeREST(k *kind, body []byte) (obj, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return obj{}, nil
	}
	v := k.newREST()
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		msg := err.Error()
		if f, ok := strings.CutPrefix(msg, "json: unknown field "); ok {
			return nil, apierr.InvalidArgument("Invalid JSON payload received. Unknown name %s: Cannot find field.", f)
		}
		return nil, apierr.InvalidArgument("Invalid JSON payload received. %s", msg)
	}
	return toObj(v)
}

// fromProto converts a gRPC message into the REST JSON object of kind k.
func fromProto(k *kind, m proto.Message) (obj, error) {
	if m == nil {
		return obj{}, nil
	}
	b, err := protojson.Marshal(m)
	if err != nil {
		return nil, apierr.InvalidArgument("%v", err)
	}
	v := k.newREST()
	if err := json.Unmarshal(b, v); err != nil {
		return nil, apierr.InvalidArgument("%v", err)
	}
	return toObj(v)
}

// toProto converts a REST JSON object into a gRPC message. Fields the Go
// gRPC stubs do not know yet are dropped.
func toProto(o obj, m proto.Message) error {
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(b, m)
}

// toObj re-encodes a typed REST value as a JSON object.
func toObj(v any) (obj, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	o := obj{}
	if err := json.Unmarshal(b, &o); err != nil {
		return nil, err
	}
	return o, nil
}

// as decodes a JSON object into a typed REST value.
func as[T any](o obj) (*T, error) {
	b, err := json.Marshal(o)
	if err != nil {
		return nil, err
	}
	v := new(T)
	if err := json.Unmarshal(b, v); err != nil {
		return nil, apierr.InvalidArgument("%v", err)
	}
	return v, nil
}

// mustObj is toObj for values that always encode.
func mustObj(v any) obj {
	o, err := toObj(v)
	if err != nil {
		panic(err)
	}
	return o
}

// clone deep-copies an object.
func clone(o obj) obj {
	if o == nil {
		return nil
	}
	b, _ := json.Marshal(o)
	out := obj{}
	_ = json.Unmarshal(b, &out)
	return out
}

// camel converts a snake_case field mask segment to lowerCamel JSON form.
func camel(s string) string {
	if !strings.Contains(s, "_") {
		return s
	}
	var b strings.Builder
	up := false
	for _, r := range s {
		if r == '_' {
			up = true
			continue
		}
		if up {
			r = unicode.ToUpper(r)
			up = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

// getPath returns the value at a dotted path.
func getPath(o obj, path string) (any, bool) {
	parts := strings.Split(path, ".")
	var cur any = o
	for _, p := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[p]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// setPath sets (or with ok=false deletes) the value at a dotted path.
func setPath(o obj, path string, v any, ok bool) {
	parts := strings.Split(path, ".")
	m := o
	for _, p := range parts[:len(parts)-1] {
		next, isMap := m[p].(map[string]any)
		if !isMap {
			if !ok {
				return
			}
			next = map[string]any{}
			m[p] = next
		}
		m = next
	}
	last := parts[len(parts)-1]
	if ok {
		m[last] = v
	} else {
		delete(m, last)
	}
}

// jsonEqual compares two decoded JSON values.
func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
