package sql

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Connector support (FR-SQL-005) and SSL certificates (FR-SQL-007).

// connectSettings returns what the Cloud SQL connectors need: addresses,
// the server CA and the database version. No DNS name is reported, so the
// connectors verify the server certificate's CN ("project:instance").
func (s *Service) connectSettings(w http.ResponseWriter, r *http.Request) {
	rec, err := s.instanceFor(r, "cloudsql.instances.get")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	in := rec.Instance
	writeJSON(w, r, &sqladmin.ConnectSettings{
		Kind:            "sql#connectSettings",
		BackendType:     in.BackendType,
		DatabaseVersion: in.DatabaseVersion,
		IpAddresses:     in.IpAddresses,
		Region:          in.Region,
		ServerCaCert:    in.ServerCaCert,
		ServerCaMode:    in.Settings.IpConfiguration.ServerCaMode,
	})
}

// principalForToken resolves an access token for an IAM-authenticated
// ephemeral certificate.
func (s *Service) principalForToken(ctx context.Context, tok string) (string, error) {
	if tok == "" {
		return "", nil
	}
	p, ok := s.env.Auth.Authenticate(ctx, tok)
	if !ok {
		return "", apierr.Unauthenticated("Invalid request: the access token in the request is invalid or expired.").
			WithReason(errDomain, "INVALID_ACCESS_TOKEN")
	}
	return string(p), nil
}

// generateEphemeralCert signs the connector's public key with the
// instance's client CA. With access_token (IAM database authentication)
// the caller's principal is embedded in the certificate and the
// certificate expires no later than an hour.
func (s *Service) generateEphemeralCert(w http.ResponseWriter, r *http.Request) {
	rec, err := s.instanceFor(r, "cloudsql.instances.connect")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var req sqladmin.GenerateEphemeralCertRequest
	if err := decode(r, &req, false); err != nil {
		apierr.Write(w, err)
		return
	}
	ttl := ephemeralTTL
	if req.ValidDuration != "" {
		if d, err := time.ParseDuration(req.ValidDuration); err == nil && d > 0 {
			ttl = d
		}
	}
	certP, err := s.ephemeral(r.Context(), rec, req.PublicKey, req.AccessToken, ttl)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, r, &sqladmin.GenerateEphemeralCertResponse{EphemeralCert: sslCertOf(certP, rec.Instance.Project, rec.Instance.Name)})
}

// createEphemeral is the older sslCerts.createEphemeral form.
func (s *Service) createEphemeral(w http.ResponseWriter, r *http.Request) {
	rec, err := s.instanceFor(r, "cloudsql.sslCerts.createEphemeral")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var req sqladmin.SslCertsCreateEphemeralRequest
	if err := decode(r, &req, false); err != nil {
		apierr.Write(w, err)
		return
	}
	certP, err := s.ephemeral(r.Context(), rec, req.PublicKey, req.AccessToken, ephemeralTTL)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, r, sslCertOf(certP, rec.Instance.Project, rec.Instance.Name))
}

func (s *Service) ephemeral(ctx context.Context, rec *instanceRecord, pubPEM, token string, ttl time.Duration) (string, error) {
	pub, err := parsePublicKey(pubPEM)
	if err != nil {
		return "", errInvalid("Invalid public key: %v.", err)
	}
	principal, err := s.principalForToken(ctx, token)
	if err != nil {
		return "", err
	}
	certP, err := newEphemeralCert(rec, pub, principal, ttl)
	if err != nil {
		return "", apierr.Internal("sign certificate: %v", err)
	}
	return certP, nil
}

func (s *Service) listServerCas(w http.ResponseWriter, r *http.Request) {
	rec, err := s.instanceFor(r, "cloudsql.instances.listServerCas")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	ca := rec.Instance.ServerCaCert
	writeJSON(w, r, &sqladmin.InstancesListServerCasResponse{
		Kind: "sql#instancesListServerCas", Certs: []*sqladmin.SslCert{ca}, ActiveVersion: ca.Sha1Fingerprint,
	})
}

func (s *Service) listServerCertificates(w http.ResponseWriter, r *http.Request) {
	rec, err := s.instanceFor(r, "cloudsql.instances.listServerCertificates")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	ca := rec.Instance.ServerCaCert
	writeJSON(w, r, &sqladmin.InstancesListServerCertificatesResponse{
		Kind:          "sql#instancesListServerCertificates",
		CaCerts:       []*sqladmin.SslCert{ca},
		ServerCerts:   []*sqladmin.SslCert{sslCertOf(rec.ServerCertPEM, rec.Instance.Project, rec.Instance.Name)},
		ActiveVersion: ca.Sha1Fingerprint,
	})
}

// insertSslCert creates a client certificate signed by the instance's
// client CA; it is accepted by TRUSTED_CLIENT_CERTIFICATE_REQUIRED.
func (s *Service) insertSslCert(w http.ResponseWriter, r *http.Request) {
	rec, err := s.instanceFor(r, "cloudsql.sslCerts.create")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var req sqladmin.SslCertsInsertRequest
	if err := decode(r, &req, false); err != nil {
		apierr.Write(w, err)
		return
	}
	if strings.TrimSpace(req.CommonName) == "" {
		apierr.Write(w, errInvalid("Missing common name."))
		return
	}
	project, inst := rec.Instance.Project, rec.Instance.Name
	var exists bool
	_ = s.env.Store.View(func(tx store.Tx) error {
		scanJSON(tx, nsSSLCerts, project+"/"+inst+"/", func(_ string, c *sqladmin.SslCert) {
			exists = exists || c.CommonName == req.CommonName
		})
		return nil
	})
	if exists {
		apierr.Write(w, apierr.AlreadyExists("Invalid request: a client certificate with common name %q already exists.", req.CommonName).WithLegacy("alreadyExists"))
		return
	}
	certP, keyP, err := newClientCert(rec, req.CommonName)
	if err != nil {
		apierr.Write(w, apierr.Internal("%v", err))
		return
	}
	c := sslCertOf(certP, project, inst)
	op := s.newOp(r.Context(), project, inst, "CREATE_CLIENT_CERTIFICATE")
	op = s.runOp(op, false, func(context.Context) error {
		return s.env.Store.Update(func(tx store.Tx) error {
			return store.PutJSON(tx, nsSSLCerts, childKey(project, inst, c.Sha1Fingerprint), c)
		})
	})
	writeJSON(w, r, &sqladmin.SslCertsInsertResponse{
		Kind:         "sql#sslCertsInsert",
		Operation:    op,
		ServerCaCert: rec.Instance.ServerCaCert,
		ClientCert:   &sqladmin.SslCertDetail{CertInfo: c, CertPrivateKey: keyP},
	})
}

func (s *Service) listSslCerts(w http.ResponseWriter, r *http.Request) {
	rec, err := s.instanceFor(r, "cloudsql.sslCerts.list")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	resp := &sqladmin.SslCertsListResponse{Kind: "sql#sslCertsList"}
	_ = s.env.Store.View(func(tx store.Tx) error {
		scanJSON(tx, nsSSLCerts, rec.Instance.Project+"/"+rec.Instance.Name+"/", func(_ string, c *sqladmin.SslCert) {
			resp.Items = append(resp.Items, c)
		})
		return nil
	})
	sort.Slice(resp.Items, func(i, j int) bool { return resp.Items[i].CommonName < resp.Items[j].CommonName })
	writeJSON(w, r, resp)
}

func (s *Service) getSslCert(w http.ResponseWriter, r *http.Request) {
	rec, err := s.instanceFor(r, "cloudsql.sslCerts.get")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var c sqladmin.SslCert
	var ok bool
	_ = s.env.Store.View(func(tx store.Tx) error {
		ok = store.GetJSON(tx, nsSSLCerts, childKey(rec.Instance.Project, rec.Instance.Name, r.PathValue("fp")), &c) == nil
		return nil
	})
	if !ok {
		apierr.Write(w, apierr.NotFound("The SSL certificate does not exist.").WithLegacy("sslCertificateDoesNotExist"))
		return
	}
	writeJSON(w, r, &c)
}

func (s *Service) deleteSslCert(w http.ResponseWriter, r *http.Request) {
	rec, err := s.instanceFor(r, "cloudsql.sslCerts.delete")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	project, inst, fp := rec.Instance.Project, rec.Instance.Name, r.PathValue("fp")
	if !s.sslCertExists(project, inst, fp) {
		apierr.Write(w, apierr.NotFound("The SSL certificate does not exist.").WithLegacy("sslCertificateDoesNotExist"))
		return
	}
	op := s.newOp(r.Context(), project, inst, "DELETE_CLIENT_CERTIFICATE")
	writeJSON(w, r, s.runOp(op, false, func(context.Context) error {
		return s.env.Store.Update(func(tx store.Tx) error { return tx.Delete(nsSSLCerts, childKey(project, inst, fp)) })
	}))
}

// resetSslConfig deletes every client certificate of the instance.
func (s *Service) resetSslConfig(w http.ResponseWriter, r *http.Request) {
	rec, err := s.instanceFor(r, "cloudsql.instances.resetSslConfig")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	project, inst := rec.Instance.Project, rec.Instance.Name
	op := s.newOp(r.Context(), project, inst, "RESET_SSL_CONFIG")
	writeJSON(w, r, s.runOp(op, false, func(context.Context) error {
		return s.env.Store.Update(func(tx store.Tx) error {
			var keys []string
			tx.Scan(nsSSLCerts, project+"/"+inst+"/", func(k string, _ []byte) bool { keys = append(keys, k); return true })
			for _, k := range keys {
				if err := tx.Delete(nsSSLCerts, k); err != nil {
					return err
				}
			}
			return nil
		})
	}))
}

// Tiers (Recorded): the machine types Cloud SQL lists.
var tiers = []struct {
	Name string
	RAM  int64
}{
	{"db-f1-micro", 644245094}, {"db-g1-small", 1825361100},
	{"db-custom-1-3840", 3840 << 20}, {"db-custom-2-7680", 7680 << 20}, {"db-custom-4-15360", 15360 << 20},
	{"db-custom-8-30720", 30720 << 20}, {"db-custom-16-61440", 61440 << 20},
	{"db-perf-optimized-N-2", 16 << 30}, {"db-perf-optimized-N-4", 32 << 30}, {"db-perf-optimized-N-8", 64 << 30},
}

func (s *Service) listTiers(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if err := s.env.EnsureProject(project); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := s.check(r.Context(), "cloudsql.tiers.list", projectResource(project)); err != nil {
		apierr.Write(w, err)
		return
	}
	regions := sortedKeys(sqlRegions)
	resp := &sqladmin.TiersListResponse{Kind: "sql#tiersList"}
	for _, t := range tiers {
		resp.Items = append(resp.Items, &sqladmin.Tier{Kind: "sql#tier", Tier: t.Name, RAM: t.RAM, DiskQuota: 65536 << 30, Region: regions})
	}
	writeJSON(w, r, resp)
}
