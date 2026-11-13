package trust

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"
)

// testCert returns a self-signed CA certificate (DER) named cn.
func testCert(t *testing.T, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func pemOf(ders ...[]byte) []byte {
	var b bytes.Buffer
	for _, d := range ders {
		_ = pem.Encode(&b, &pem.Block{Type: "CERTIFICATE", Bytes: d})
	}
	return b.Bytes()
}

func sha1Hex(der []byte) string {
	s := sha1.Sum(der)
	return strings.ToUpper(hex.EncodeToString(s[:]))
}

// trustPlist is a trust-settings-export plist: entries maps each SHA-1 to
// its trustSettings array's XML, or "" for an entry without one.
func trustPlist(entries map[string]string) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>trustList</key>
	<dict>
`)
	for h, settings := range entries {
		fmt.Fprintf(&b, "\t\t<key>%s</key>\n\t\t<dict>\n\t\t\t<key>issuerName</key>\n\t\t\t<data>\n\t\t\tMDcxDzANBgNVBAoTBmdjcGVtdTEk\n\t\t\t</data>\n\t\t\t<key>modDate</key>\n\t\t\t<date>2026-10-28T14:47:28Z</date>\n", h)
		if settings != "" {
			fmt.Fprintf(&b, "\t\t\t<key>trustSettings</key>\n%s\n", settings)
		}
		b.WriteString("\t\t</dict>\n")
	}
	b.WriteString("\t</dict>\n\t<key>trustVersion</key>\n\t<integer>1</integer>\n</dict>\n</plist>\n")
	return []byte(b.String())
}

// constraint is one usage constraint dict of a trustSettings array.
func constraint(policy []byte, result int, extra string) string {
	var b strings.Builder
	b.WriteString("<dict>")
	if policy != nil {
		fmt.Fprintf(&b, "<key>kSecTrustSettingsPolicy</key><data>%s</data>", base64.StdEncoding.EncodeToString(policy))
	}
	if result != 0 {
		fmt.Fprintf(&b, "<key>kSecTrustSettingsResult</key><integer>%d</integer>", result)
	}
	b.WriteString(extra)
	b.WriteString("</dict>")
	return b.String()
}

func array(items ...string) string { return "<array>" + strings.Join(items, "") + "</array>" }

// TestMergeKeychain: the roots macOS trusts are the system roots and the
// certificates admin or user trust settings make roots, minus those they
// deny, with user over admin over system (#1138).
func TestMergeKeychain(t *testing.T) {
	sysKept, sysDenied := testCert(t, "system kept"), testCert(t, "system denied by admin")
	adminRoot, userRoot := testCert(t, "admin root"), testCert(t, "user root")
	untrusted, userOverrides := testCert(t, "in a keychain, no trust settings"), testCert(t, "admin trusts, user denies")
	admin := trustPlist(map[string]string{
		sha1Hex(sysDenied):     array(constraint(policySSL, resultDeny, "")),
		sha1Hex(adminRoot):     "",
		sha1Hex(userOverrides): "",
	})
	user := trustPlist(map[string]string{
		sha1Hex(userRoot):      array(constraint(policySSL, 0, "")),
		sha1Hex(userOverrides): array(constraint(nil, resultDeny, "")),
	})
	got, err := mergeKeychain(pemOf(sysKept, sysDenied), pemOf(adminRoot, untrusted, sysKept, userRoot, userOverrides), admin, user)
	if err != nil {
		t.Fatal(err)
	}
	if want := pemOf(sysKept, adminRoot, userRoot); !bytes.Equal(got, want) {
		t.Errorf("mergeKeychain kept %d certificates, want system kept, admin root, user root", bytes.Count(got, []byte("BEGIN")))
	}
	// No trust settings in either domain: the system roots.
	if got, err := mergeKeychain(pemOf(sysKept), pemOf(untrusted), nil, nil); err != nil || !bytes.Equal(got, pemOf(sysKept)) {
		t.Errorf("mergeKeychain without trust settings = %d certificates, %v", bytes.Count(got, []byte("BEGIN")), err)
	}
	if _, err := mergeKeychain(pemOf(sysKept), nil, []byte("<plist><dict>"), nil); err == nil {
		t.Error("a truncated admin plist is not an error")
	}
}

// TestDecide: which usage constraints decide a TLS server root's trust.
func TestDecide(t *testing.T) {
	smime := []byte{0x2a, 0x86, 0x48, 0x86, 0xf7, 0x63, 0x64, 0x01, 0x08}
	for _, tc := range []struct {
		name     string
		settings string // "" for no trustSettings key
		want     decision
	}{
		{"no constraints", "", trusted},
		{"empty constraints", array(), trusted},
		{"ssl, default result", array(constraint(policySSL, 0, "")), trusted},
		{"basic, trust as root", array(constraint(policyBasic, resultTrustAsRoot, "")), trusted},
		{"ssl deny", array(constraint(policySSL, resultDeny, "")), denied},
		{"any policy deny", array(constraint(nil, resultDeny, "")), denied},
		{"other policy only", array(constraint(smime, resultTrustRoot, "")), undecided},
		{"one application only", array(constraint(policySSL, resultTrustRoot, "<key>kSecTrustSettingsApplication</key><data>AAAA</data>")), undecided},
		{"one hostname only", array(constraint(policySSL, resultTrustRoot, "<key>kSecTrustSettingsPolicyString</key><string>a.example</string>")), undecided},
		{"unspecified, then deny", array(constraint(policySSL, 4, ""), constraint(policySSL, resultDeny, "")), denied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := trustSettings(trustPlist(map[string]string{"ab": tc.settings}))
			if err != nil {
				t.Fatal(err)
			}
			if got["AB"] != tc.want {
				t.Errorf("decision = %d, want %d", got["AB"], tc.want)
			}
		})
	}
}

// TestParsePlist: the value types trust-settings-export writes.
func TestParsePlist(t *testing.T) {
	v, err := parsePlist([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>d</key>
	<data>
	QUJD
	REVG
	</data>
	<key>i</key><integer>-3</integer>
	<key>r</key><real>1.5</real>
	<key>s</key><string>x &amp; y</string>
	<key>t</key><true/>
	<key>f</key><false/>
	<key>a</key><array><string>1</string><dict/></array>
	<key>e</key><dict/>
</dict>
</plist>`))
	if err != nil {
		t.Fatal(err)
	}
	m := v.(map[string]any)
	if string(m["d"].([]byte)) != "ABCDEF" || m["i"] != int64(-3) || m["r"] != 1.5 || m["s"] != "x & y" ||
		m["t"] != true || m["f"] != false || len(m["a"].([]any)) != 2 || len(m["e"].(map[string]any)) != 0 {
		t.Errorf("parsePlist = %#v", m)
	}
	for _, bad := range []string{"", "<plist></plist>", "<plist><dict><string>x</string></dict></plist>", "<plist><bogus/></plist>"} {
		if _, err := parsePlist([]byte(bad)); err == nil {
			t.Errorf("parsePlist(%q) is not an error", bad)
		}
	}
}
