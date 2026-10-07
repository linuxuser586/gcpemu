package lb

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"regexp"
	"sort"
	"strings"
	"time"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// SSL certificates (global and regional; FR-LB-004): SELF_MANAGED with PEM
// validation, and Google-managed (MANAGED) certificates that stay
// PROVISIONING until each domain resolves to a forwarding rule using the
// certificate, then become ACTIVE with a certificate issued by the
// emulator's CA (managedcerts.go). Private keys are stored apart from the
// resource and never returned, as on GCP.

const nsSSLKeys = "lb/sslKeys"

var kindSSLCertificate = &kind{
	coll: "sslCertificates", typ: "compute#sslCertificate", snake: "ssl_certificate",
	gperm: "compute.sslCertificates", rperm: "compute.regionSslCertificates",
	aggKind: "compute#sslCertificateAggregatedList",
	noPatch: true, noUpdate: true,
	newObj: func() any { return &computev1.SslCertificate{} },
	inTx: func(tx store.Tx, path string, obj any) error {
		if k := hidePrivateKey(obj); k != "" {
			return store.PutJSON(tx, nsSSLKeys, path, k)
		}
		return nil
	},
	afterDelete: func(ctx context.Context, s *Service, path string, obj any) {
		_ = s.env.Store.Update(func(tx store.Tx) error { return tx.Delete(nsSSLKeys, path) })
	},
}

// pacific renders compute timestamps like GCP.
var pacific = time.FixedZone("", -7*60*60)

func stamp(t time.Time) string { return t.In(pacific).Format("2006-01-02T15:04:05.000-07:00") }

var domainRE = regexp.MustCompile(`^(?:[a-zA-Z0-9](?:[-a-zA-Z0-9]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z][-a-zA-Z0-9]{0,62}\.?$`)

func prepareSSLCertificate(ctx context.Context, s *Service, sc scope, path string, obj, old any) error {
	c := obj.(*computev1.SslCertificate)
	if c.Type == "" {
		c.Type = "SELF_MANAGED"
		if c.Managed != nil {
			c.Type = "MANAGED"
		}
	}
	switch c.Type {
	case "SELF_MANAGED":
		if c.SelfManaged != nil {
			if c.Certificate == "" {
				c.Certificate = c.SelfManaged.Certificate
			}
			if c.PrivateKey == "" {
				c.PrivateKey = c.SelfManaged.PrivateKey
			}
			c.SelfManaged = nil
		}
		if c.Certificate == "" {
			return errRequired("resource.certificate")
		}
		if c.PrivateKey == "" {
			return errRequired("resource.privateKey")
		}
		pair, err := tls.X509KeyPair([]byte(c.Certificate), []byte(c.PrivateKey))
		if err != nil {
			if !strings.Contains(c.Certificate, "BEGIN CERTIFICATE") {
				return errInvalid("resource.certificate", "<certificate>", "The SSL certificate could not be parsed.")
			}
			return errInvalid("resource.privateKey", "<private key>", "The SSL key could not be parsed or does not match the certificate: "+err.Error())
		}
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return errInvalid("resource.certificate", "<certificate>", "The SSL certificate could not be parsed.")
		}
		c.SubjectAlternativeNames = certSANs(leaf)
		c.ExpireTime = stamp(leaf.NotAfter)
		c.Managed = nil
	case "MANAGED":
		if sc.region != "" {
			return errInvalid("resource.type", c.Type, "Google-managed certificates are only supported as global resources.")
		}
		if c.Managed == nil || len(c.Managed.Domains) == 0 {
			return errRequired("resource.managed.domains")
		}
		if len(c.Managed.Domains) > 100 {
			return errInvalid("resource.managed.domains", len(c.Managed.Domains), "At most 100 domains are allowed.")
		}
		seen := map[string]bool{}
		for i, d := range c.Managed.Domains {
			d = strings.ToLower(strings.TrimSuffix(d, "."))
			if !domainRE.MatchString(d) || strings.Contains(d, "*") {
				return errInvalid("resource.managed.domains["+itoa(i)+"]", d, "Domain must be a fully qualified domain name without wildcards.")
			}
			if seen[d] {
				return errInvalid("resource.managed.domains", d, "Duplicate domain.")
			}
			seen[d] = true
			c.Managed.Domains[i] = d
		}
		if old == nil {
			c.Managed.Status = "PROVISIONING"
			c.Managed.DomainStatus = map[string]string{}
			for _, d := range c.Managed.Domains {
				c.Managed.DomainStatus[d] = "PROVISIONING"
			}
			c.Certificate, c.PrivateKey, c.ExpireTime = "", "", ""
			c.SubjectAlternativeNames = append([]string{}, c.Managed.Domains...)
		}
	default:
		return errInvalid("resource.type", c.Type, "Must be SELF_MANAGED or MANAGED.")
	}
	return nil
}

// certSANs lists a certificate's DNS and IP subject alternative names.
func certSANs(leaf *x509.Certificate) []string {
	out := append([]string{}, leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		out = append(out, ip.String())
	}
	if len(out) == 0 && leaf.Subject.CommonName != "" {
		out = []string{leaf.Subject.CommonName}
	}
	return out
}

// hidePrivateKey removes the private key from the resource before it is
// stored (the key goes to nsSSLKeys in afterSave).
func hidePrivateKey(obj any) string {
	c, ok := obj.(*computev1.SslCertificate)
	if !ok {
		return ""
	}
	k := c.PrivateKey
	c.PrivateKey = ""
	return k
}

// tlsCertificate loads an SSL certificate resource as a tls.Certificate
// (nil for a managed certificate that is not ACTIVE).
func (s *Service) tlsCertificate(path string) (*tls.Certificate, *computev1.SslCertificate, error) {
	obj, ok := s.load(kindSSLCertificate, path)
	if !ok {
		return nil, nil, errNotFound(path)
	}
	c := obj.(*computev1.SslCertificate)
	if c.Certificate == "" {
		return nil, c, nil
	}
	var key string
	_ = s.env.Store.View(func(tx store.Tx) error { return store.GetJSON(tx, nsSSLKeys, path, &key) })
	if key == "" {
		return nil, c, apierr.Internal("private key of %s is missing", path)
	}
	pair, err := tls.X509KeyPair([]byte(c.Certificate), []byte(key))
	if err != nil {
		return nil, c, err
	}
	pair.Leaf, _ = x509.ParseCertificate(pair.Certificate[0])
	return &pair, c, nil
}

func pemChain(der [][]byte) string {
	var b strings.Builder
	for _, d := range der {
		b.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: d}))
	}
	return b.String()
}

func sortStrings(s []string) { sort.Strings(s) }
