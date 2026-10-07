package sql

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/reqlog"
	"github.com/linuxuser586/gcpemu/internal/store"
	"github.com/linuxuser586/gcpemu/services/sql/proxy"
)

// The agent endpoint: the in-container sql-proxy asks the emulator, for
// every client startup message, whether and how to admit it. All policy
// lives here so that changes to authorized networks, SSL mode, users and
// IAM bindings apply to the next connection.

func (s *Service) peer(name string) emu.Service {
	p, _ := s.env.Lookup(name)
	return p
}

// vpc returns compute's VPC contract when the compute service runs.
func (s *Service) vpc() (emu.VPC, bool) {
	v, ok := s.peer("compute").(emu.VPC)
	return v, ok
}

// ipOwner identifies the instance's private IP reservation.
func ipOwner(project, name string) string { return "sql:" + resourceLabel(project, name) }

// normalizeNetwork returns "projects/P/global/networks/N" for a network
// given as a short name, partial or full URL.
func normalizeNetwork(project, n string) string {
	if i := strings.Index(n, "projects/"); i >= 0 {
		return n[i:]
	}
	if strings.Contains(n, "/") {
		return n
	}
	return "projects/" + project + "/global/networks/" + n
}

func (s *Service) agentLogin(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("instance")
	rec, err := s.loadRecord(project, name)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get(proxy.KeyHeader)), []byte(rec.AgentKey)) != 1 {
		apierr.Write(w, apierr.PermissionDenied("invalid agent key"))
		return
	}
	var req proxy.LoginRequest
	b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err := json.Unmarshal(b, &req); err != nil {
		apierr.Write(w, apierr.InvalidArgument("%v", err))
		return
	}
	start := time.Now()
	resp := s.decideLogin(r.Context(), rec, &req)
	e := reqlog.Entry{Time: start, Service: "sql", Protocol: "postgres", Method: "CONNECT " + req.Listener + " " + req.Remote,
		Resource: "projects/" + project + "/instances/" + name, Principal: req.User, Code: "OK",
		LatencyMS: float64(time.Since(start).Microseconds()) / 1000}
	if resp.Action == proxy.ActionDeny {
		s.env.Log.Info("sql: connection refused", "instance", project+":"+name, "user", req.User, "remote", req.Remote, "listener", req.Listener, "reason", resp.Message)
		e.Status, e.Code = 1, resp.Code
	}
	s.env.RequestLog.Add(e)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func deny(code, format string, args ...any) *proxy.LoginResponse {
	return &proxy.LoginResponse{Action: proxy.ActionDeny, Code: code, Message: fmt.Sprintf(format, args...)}
}

// decideLogin applies FR-SQL-004 (authorized networks on the public path),
// FR-SQL-007 (SSL modes, client certificates), connector enforcement and
// FR-SQL-006 (IAM database authentication).
func (s *Service) decideLogin(ctx context.Context, rec *instanceRecord, req *proxy.LoginRequest) *proxy.LoginResponse {
	in := rec.Instance
	st := in.Settings
	if st == nil {
		st = &sqladmin.Settings{}
	}
	ipc := st.IpConfiguration
	if ipc == nil {
		ipc = &sqladmin.IpConfiguration{}
	}
	var cert *clientCertInfo
	if len(req.ClientCert) > 0 {
		var err error
		if cert, err = verifyClientCert(rec, req.ClientCert); err != nil {
			return deny("28000", "%v", err)
		}
		if !cert.Ephemeral && !s.sslCertExists(in.Project, in.Name, cert.SHA1) {
			return deny("28000", "client certificate has been revoked")
		}
	}

	if req.Listener == proxy.ListenerDirect {
		if st.ConnectorEnforcement == "REQUIRED" {
			return deny("28000", "connections to instance %s must use the Cloud SQL connectors (connectorEnforcement is REQUIRED)", in.ConnectionName)
		}
		if msg := s.networkPolicy(ctx, rec, req.Local, req.Remote); msg != "" {
			return deny("28000", "%s", msg)
		}
		switch sslMode(ipc) {
		case "ENCRYPTED_ONLY":
			if !req.TLS {
				return deny("28000", "pg_hba.conf rejects connection for host %q, user %q, database %q, no encryption", req.Remote, req.User, req.Database)
			}
		case "TRUSTED_CLIENT_CERTIFICATE_REQUIRED":
			if !req.TLS || cert == nil {
				return deny("28000", "connection requires a valid client certificate")
			}
		}
	}

	u, ok := s.findUser(in.Project, in.Name, req.User)
	if !ok || u.User.Type == "" || u.User.Type == "BUILT_IN" {
		return &proxy.LoginResponse{Action: proxy.ActionPassthrough}
	}
	// IAM database authentication.
	if flagMap(st)["cloudsql.iam_authentication"] != "on" {
		return deny("28000", "Cloud SQL IAM user authentication failed for user %q: the cloudsql.iam_authentication flag is not enabled on the instance", req.User)
	}
	var principal emu.Principal
	switch {
	case cert != nil && cert.Principal != "":
		principal = emu.Principal(cert.Principal)
	case req.Password != nil:
		p, ok := s.env.Auth.Authenticate(ctx, strings.TrimSpace(*req.Password))
		if !ok {
			return deny("28P01", "Cloud SQL IAM user authentication failed for user %q", req.User)
		}
		principal = p
	default:
		return &proxy.LoginResponse{Action: proxy.ActionPassword}
	}
	if !iamUserMatches(u.User, principal) {
		return deny("28P01", "Cloud SQL IAM user authentication failed for user %q", req.User)
	}
	if err := s.env.Auth.Check(emu.WithPrincipal(ctx, principal), "cloudsql.instances.login", instanceResource(in.Project, in.Name)); err != nil {
		return deny("28P01", "Cloud SQL IAM user authentication failed for user %q: %s lacks cloudsql.instances.login (roles/cloudsql.instanceUser)", req.User, principal)
	}
	return &proxy.LoginResponse{Action: proxy.ActionTrust}
}

// iamUserMatches compares an IAM database user with the authenticated
// principal: CLOUD_IAM_USER users are the full email, service accounts the
// email without ".gserviceaccount.com".
func iamUserMatches(u *sqladmin.User, p emu.Principal) bool {
	kind, email, _ := strings.Cut(string(p), ":")
	switch u.Type {
	case "CLOUD_IAM_USER":
		return kind == "user" && strings.EqualFold(email, u.Name)
	case "CLOUD_IAM_SERVICE_ACCOUNT":
		return kind == "serviceAccount" && strings.EqualFold(strings.TrimSuffix(email, ".gserviceaccount.com"), u.Name)
	}
	return false
}

// sslMode returns the effective SSL mode (requireSsl is the legacy form).
func sslMode(ipc *sqladmin.IpConfiguration) string {
	if ipc.SslMode != "" && ipc.SslMode != "SSL_MODE_UNSPECIFIED" {
		return ipc.SslMode
	}
	if ipc.RequireSsl {
		return "TRUSTED_CLIENT_CERTIFICATE_REQUIRED"
	}
	return "ALLOW_UNENCRYPTED_AND_ENCRYPTED"
}

// networkPolicy returns a refusal message for a direct (5432) connection,
// or "" to admit it. Connections to the private IP and from other
// emulator containers on the services network are not subject to
// authorized networks. Connections arriving at the public IP, or from the
// host (the source is a container network's gateway: the published host
// port, or the host dialling the public IP), are checked against
// authorizedNetworks; host connections match 127.0.0.1/32 as well as the
// gateway address. With ipv4Enabled=false only the host may use the
// published port.
func (s *Service) networkPolicy(ctx context.Context, rec *instanceRecord, local, remote string) string {
	if rec.PrivateIP != "" && local == rec.PrivateIP {
		return ""
	}
	fromHost := false
	var gateways []string
	if _, plane, err := s.plane(ctx); err == nil {
		if n, err := plane.Services(ctx); err == nil {
			gateways = append(gateways, n.Gateway)
			if remote != n.Gateway && cidrContains(n.Subnet, remote) && local != rec.PublicIP {
				return "" // another emulator container on the services network
			}
		}
		if n, err := plane.External(ctx); err == nil {
			gateways = append(gateways, n.Gateway)
		}
	}
	if rec.PrivateNet != nil {
		gateways = append(gateways, rec.PrivateNet.Gateway)
	}
	for _, g := range gateways {
		if g != "" && remote == g {
			fromHost = true
		}
	}
	ipc := rec.Instance.Settings.IpConfiguration
	public := ipc != nil && ipc.Ipv4Enabled
	if !public {
		if fromHost {
			return ""
		}
		return fmt.Sprintf("instance %s has no public IP", rec.Instance.ConnectionName)
	}
	sources := []string{remote}
	if fromHost {
		sources = append(sources, "127.0.0.1")
	}
	now := s.env.Clock.Now()
	for _, acl := range ipc.AuthorizedNetworks {
		if acl == nil {
			continue
		}
		if acl.ExpirationTime != "" {
			if t, err := time.Parse(time.RFC3339Nano, acl.ExpirationTime); err == nil && now.After(t) {
				continue
			}
		}
		for _, src := range sources {
			if aclMatches(acl.Value, src) {
				return ""
			}
		}
	}
	return fmt.Sprintf("connection from %s is not authorized: the address is not in the authorized networks of instance %s", remote, rec.Instance.ConnectionName)
}

// aclMatches reports whether ip is covered by an authorized network value
// (a CIDR or a single address).
func aclMatches(value, ip string) bool {
	if !strings.Contains(value, "/") {
		return net.ParseIP(value).Equal(net.ParseIP(ip))
	}
	return cidrContains(value, ip)
}

func cidrContains(cidr, ip string) bool {
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		return false
	}
	p := net.ParseIP(ip)
	return p != nil && n.Contains(p)
}

func (s *Service) sslCertExists(project, inst, sha string) bool {
	var ok bool
	_ = s.env.Store.View(func(tx store.Tx) error {
		ok = store.Exists(tx, nsSSLCerts, childKey(project, inst, sha))
		return nil
	})
	return ok
}
