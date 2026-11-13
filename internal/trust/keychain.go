package trust

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// The roots macOS trusts for TLS are its system roots plus the
// certificates admin and user trust settings make roots, minus those they
// deny (#1138). /etc/ssl/cert.pem is a static bundle from LibreSSL that
// misses newer roots. Apple's date-limited distrusts (such as Entrust's
// for leaves issued after 2024-11-15), valid.apple.com revocation and CT
// checks happen only inside trustd and do not carry over to a bundle.

// decision is what a domain's trust settings say of a certificate.
type decision int

const (
	undecided decision = iota
	trusted
	denied
)

// Trust settings keys and values (SecTrustSettings.h).
const (
	resultTrustRoot   = 1
	resultTrustAsRoot = 2
	resultDeny        = 3
)

// The policies a TLS server root may be trusted for: Apple's SSL policy
// and its basic X.509 policy, as their DER OIDs.
var (
	policySSL   = []byte{0x2a, 0x86, 0x48, 0x86, 0xf7, 0x63, 0x64, 0x01, 0x03} // 1.2.840.113635.100.1.3
	policyBasic = []byte{0x2a, 0x86, 0x48, 0x86, 0xf7, 0x63, 0x64, 0x01, 0x02} // 1.2.840.113635.100.1.2
)

// mergeKeychain returns the roots macOS trusts for TLS, as PEM, in order:
// system first, then the rest. systemPEM is the system roots keychain's
// certificates, searchPEM the certificates of the user's keychain search
// list (login and System keychains), and adminPlist and userPlist the
// admin and user trust settings, as `security trust-settings-export`
// writes them. A domain's decision overrides the one below it: user over
// admin over system.
func mergeKeychain(systemPEM, searchPEM, adminPlist, userPlist []byte) ([]byte, error) {
	admin, err := trustSettings(adminPlist)
	if err != nil {
		return nil, fmt.Errorf("admin trust settings: %w", err)
	}
	user, err := trustSettings(userPlist)
	if err != nil {
		return nil, fmt.Errorf("user trust settings: %w", err)
	}
	effective := func(h string, system bool) bool {
		for _, d := range []decision{user[h], admin[h]} {
			if d != undecided {
				return d == trusted
			}
		}
		return system
	}
	var out bytes.Buffer
	seen := map[string]bool{}
	add := func(certs []byte, system bool) {
		for rest := certs; ; {
			var b *pem.Block
			if b, rest = pem.Decode(rest); b == nil {
				return
			}
			if b.Type != "CERTIFICATE" {
				continue
			}
			sum := sha1.Sum(b.Bytes)
			h := strings.ToUpper(hex.EncodeToString(sum[:]))
			if seen[h] || !effective(h, system) {
				continue
			}
			seen[h] = true
			_ = pem.Encode(&out, &pem.Block{Type: "CERTIFICATE", Bytes: b.Bytes})
		}
	}
	add(systemPEM, true)
	add(searchPEM, false)
	return out.Bytes(), nil
}

// trustSettings returns the decision a domain's exported trust settings
// make for each certificate, by upper-case hex SHA-1. Empty input is a
// domain with no trust settings.
func trustSettings(plist []byte) (map[string]decision, error) {
	if len(bytes.TrimSpace(plist)) == 0 {
		return nil, nil
	}
	v, err := parsePlist(plist)
	if err != nil {
		return nil, err
	}
	top, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("not a dictionary")
	}
	list, _ := top["trustList"].(map[string]any)
	out := map[string]decision{}
	for h, e := range list {
		entry, _ := e.(map[string]any)
		out[strings.ToUpper(h)] = decide(entry)
	}
	return out, nil
}

// decide returns what one trustList entry decides for a TLS server root.
// No usage constraints means always trust; otherwise the first constraint
// that applies to TLS for every application decides, and those limited to
// an application, a hostname or another policy are skipped.
func decide(entry map[string]any) decision {
	raw, ok := entry["trustSettings"]
	if !ok {
		return trusted
	}
	constraints, _ := raw.([]any)
	if len(constraints) == 0 {
		return trusted
	}
	for _, c := range constraints {
		d, _ := c.(map[string]any)
		if d == nil {
			continue
		}
		if _, ok := d["kSecTrustSettingsApplication"]; ok {
			continue
		}
		if _, ok := d["kSecTrustSettingsPolicyString"]; ok {
			continue
		}
		if p, ok := d["kSecTrustSettingsPolicy"]; ok {
			oid, _ := p.([]byte)
			if !bytes.Equal(oid, policySSL) && !bytes.Equal(oid, policyBasic) {
				continue
			}
		}
		result := int64(resultTrustRoot)
		if r, ok := d["kSecTrustSettingsResult"].(int64); ok {
			result = r
		}
		switch result {
		case resultTrustRoot, resultTrustAsRoot:
			return trusted
		case resultDeny:
			return denied
		}
	}
	return undecided
}

// errEnd ends a dict or array in parsePlist.
var errEnd = errors.New("end of element")

// parsePlist parses an XML property list into map[string]any, []any,
// []byte (data), string (string and date), int64, float64 and bool.
func parsePlist(b []byte) (any, error) {
	d := xml.NewDecoder(bytes.NewReader(b))
	for {
		tok, err := d.Token()
		if err != nil {
			if err == io.EOF {
				err = errors.New("no plist element")
			}
			return nil, err
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "plist" {
			v, err := plistValue(d)
			if err == errEnd {
				return nil, errors.New("empty plist")
			}
			return v, err
		}
	}
}

// plistValue reads the next value, or returns errEnd at its container's
// end.
func plistValue(d *xml.Decoder) (any, error) {
	for {
		tok, err := d.Token()
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.EndElement:
			return nil, errEnd
		case xml.StartElement:
			return plistElement(d, t)
		}
	}
}

func plistElement(d *xml.Decoder, se xml.StartElement) (any, error) {
	text := func() (string, error) {
		var s string
		err := d.DecodeElement(&s, &se)
		return s, err
	}
	switch se.Name.Local {
	case "dict":
		m := map[string]any{}
		for {
			k, err := plistValue(d)
			if err == errEnd {
				return m, nil
			}
			if err != nil {
				return nil, err
			}
			key, ok := k.(plistKey)
			if !ok {
				return nil, errors.New("dict entry without a key")
			}
			v, err := plistValue(d)
			if err == errEnd {
				return nil, fmt.Errorf("key %q without a value", key)
			}
			if err != nil {
				return nil, err
			}
			m[string(key)] = v
		}
	case "array":
		a := []any{}
		for {
			v, err := plistValue(d)
			if err == errEnd {
				return a, nil
			}
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
	case "key":
		s, err := text()
		return plistKey(s), err
	case "string", "date":
		return text()
	case "data":
		s, err := text()
		if err != nil {
			return nil, err
		}
		return base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), ""))
	case "integer":
		s, err := text()
		if err != nil {
			return nil, err
		}
		return strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	case "real":
		s, err := text()
		if err != nil {
			return nil, err
		}
		return strconv.ParseFloat(strings.TrimSpace(s), 64)
	case "true", "false":
		if err := d.Skip(); err != nil {
			return nil, err
		}
		return se.Name.Local == "true", nil
	}
	return nil, fmt.Errorf("unknown plist element <%s>", se.Name.Local)
}

// plistKey is a dict key, told apart from a string value.
type plistKey string
