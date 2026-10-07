package lb

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	computev1 "google.golang.org/api/compute/v1"
)

// Custom header variables (FR-LB-008, FR-LB-009, FR-CDN-004) and header
// actions. Values follow GCP's documented formats; geography is reported
// for an unknown location (CLDR "ZZ") since clients are local.

// expand substitutes {variable} references in a header value.
func expand(v string, rs *reqState) string {
	if !strings.Contains(v, "{") {
		return v
	}
	var b strings.Builder
	rest := v
	for {
		i := strings.IndexByte(rest, '{')
		if i < 0 {
			b.WriteString(rest)
			break
		}
		j := strings.IndexByte(rest[i:], '}')
		if j < 0 {
			b.WriteString(rest)
			break
		}
		b.WriteString(rest[:i])
		name := rest[i+1 : i+j]
		if val, ok := variable(name, rs); ok {
			b.WriteString(val)
		} else {
			b.WriteString(rest[i : i+j+1])
		}
		rest = rest[i+j+1:]
	}
	return b.String()
}

// variable returns the value of a GCP header variable.
func variable(name string, rs *reqState) (string, bool) {
	r := rs.req
	switch name {
	case "cdn_cache_status":
		return rs.cacheStatusNow(), true
	case "cdn_cache_id":
		return "gcpemu", true
	case "client_region":
		return "ZZ", true
	case "client_region_subdivision", "client_city":
		return "", true
	case "client_city_lat_long":
		return "0.000000,0.000000", true
	case "client_rtt_msec":
		return "0", true
	case "client_encrypted":
		return boolStr(r.TLS != nil), true
	case "client_protocol":
		return r.Proto, true
	case "client_ip_address":
		return rs.clientIP, true
	case "client_port":
		return rs.clientPort, true
	case "server_ip":
		return rs.fe.ip, true
	case "server_port":
		return itoa(rs.fe.port), true
	case "origin_request_header":
		return r.Header.Get("Origin"), true
	case "device_request_type":
		return deviceType(r.UserAgent()), true
	case "user_agent_family":
		return uaFamily(r.UserAgent()), true
	case "tls_sni_hostname":
		if r.TLS != nil {
			return r.TLS.ServerName, true
		}
		return "", true
	case "tls_version":
		if r.TLS != nil {
			return tlsVersionName(r.TLS.Version), true
		}
		return "", true
	case "tls_cipher_suite":
		if r.TLS != nil {
			return tls.CipherSuiteName(r.TLS.CipherSuite), true
		}
		return "", true
	case "tls_ja3_fingerprint", "tls_ja4_fingerprint":
		return "", true
	}
	if strings.HasPrefix(name, "client_cert_") {
		return clientCertVar(strings.TrimPrefix(name, "client_cert_"), rs), true
	}
	return "", false
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func tlsVersionName(v uint16) string {
	switch v {
	case tls.VersionTLS10:
		return "TLSv1"
	case tls.VersionTLS11:
		return "TLSv1.1"
	case tls.VersionTLS12:
		return "TLSv1.2"
	case tls.VersionTLS13:
		return "TLSv1.3"
	}
	return ""
}

func deviceType(ua string) string {
	l := strings.ToLower(ua)
	switch {
	case strings.Contains(l, "ipad") || strings.Contains(l, "tablet"):
		return "TABLET"
	case strings.Contains(l, "mobile") || strings.Contains(l, "android") || strings.Contains(l, "iphone"):
		return "MOBILE"
	case ua == "":
		return "UNDETERMINED"
	}
	return "DESKTOP"
}

func uaFamily(ua string) string {
	l := strings.ToLower(ua)
	switch {
	case strings.Contains(l, "edg/"):
		return "EDGE"
	case strings.Contains(l, "chrome/"):
		return "CHROME"
	case strings.Contains(l, "firefox/"):
		return "FIREFOX"
	case strings.Contains(l, "safari/"):
		return "SAFARI"
	case strings.Contains(l, "msie") || strings.Contains(l, "trident/"):
		return "INTERNET_EXPLORER"
	case strings.Contains(l, "opera") || strings.Contains(l, "opr/"):
		return "OPERA"
	}
	return "OTHER"
}

// clientCertVar renders the {client_cert_*} variables (frontend mTLS).
func clientCertVar(name string, rs *reqState) string {
	r := rs.req
	var leaf *x509.Certificate
	var chain []*x509.Certificate
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		chain = r.TLS.PeerCertificates
		leaf = chain[0]
	}
	switch name {
	case "present":
		return boolStr(leaf != nil)
	case "chain_verified":
		return boolStr(leaf != nil && rs.certErr == "")
	case "error":
		if leaf == nil && rs.fe.mtlsMode != "" {
			return "client_cert_not_provided"
		}
		return rs.certErr
	}
	if leaf == nil {
		return ""
	}
	switch name {
	case "sha256_fingerprint":
		sum := sha256.Sum256(leaf.Raw)
		return base64.StdEncoding.EncodeToString(sum[:])
	case "serial_number":
		return hex.EncodeToString(leaf.SerialNumber.Bytes())
	case "spiffe_id":
		for _, u := range leaf.URIs {
			if u.Scheme == "spiffe" {
				return u.String()
			}
		}
		return ""
	case "uri_sans":
		var out []string
		for _, u := range leaf.URIs {
			out = append(out, base64.StdEncoding.EncodeToString([]byte(u.String())))
		}
		return strings.Join(out, ",")
	case "dnsname_sans":
		var out []string
		for _, d := range leaf.DNSNames {
			out = append(out, base64.StdEncoding.EncodeToString([]byte(d)))
		}
		return strings.Join(out, ",")
	case "valid_not_before":
		return leaf.NotBefore.UTC().Format(time.RFC3339)
	case "valid_not_after":
		return leaf.NotAfter.UTC().Format(time.RFC3339)
	case "issuer_dn":
		return base64.StdEncoding.EncodeToString(leaf.RawIssuer)
	case "subject_dn":
		return base64.StdEncoding.EncodeToString(leaf.RawSubject)
	case "leaf":
		return ":" + base64.StdEncoding.EncodeToString(leaf.Raw) + ":"
	case "chain":
		var out []string
		for _, c := range chain[1:] {
			out = append(out, ":"+base64.StdEncoding.EncodeToString(c.Raw)+":")
		}
		return strings.Join(out, ",")
	}
	return ""
}

// applyRequestActions applies header actions to an outbound request.
func applyRequestActions(h http.Header, actions []*computev1.HttpHeaderAction, rs *reqState) {
	for _, a := range actions {
		for _, n := range a.RequestHeadersToRemove {
			h.Del(n)
		}
		for _, o := range a.RequestHeadersToAdd {
			setHeader(h, o.HeaderName, expand(o.HeaderValue, rs), o.Replace)
		}
	}
}

// applyResponseActions applies header actions to a response.
func applyResponseActions(h http.Header, actions []*computev1.HttpHeaderAction, rs *reqState) {
	for _, a := range actions {
		for _, n := range a.ResponseHeadersToRemove {
			h.Del(n)
		}
		for _, o := range a.ResponseHeadersToAdd {
			setHeader(h, o.HeaderName, expand(o.HeaderValue, rs), o.Replace)
		}
	}
}

func setHeader(h http.Header, name, value string, replace bool) {
	if replace {
		h.Set(name, value)
		return
	}
	h.Add(name, value)
}

// applyCustomHeaders applies "Name:Value" custom headers (backend
// services' customRequestHeaders / customResponseHeaders).
func applyCustomHeaders(h http.Header, custom []string, rs *reqState) {
	for _, c := range custom {
		n, v, ok := strings.Cut(c, ":")
		if !ok {
			continue
		}
		h.Add(strings.TrimSpace(n), expand(strings.TrimSpace(v), rs))
	}
}
