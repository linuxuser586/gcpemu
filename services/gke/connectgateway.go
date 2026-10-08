package gke

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/project"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Connect gateway (connectgateway.googleapis.com, ADR 0002): a subset of
// GKE's fleet gateway that proxies the Kubernetes API of a cluster, so
// that the Web console (FR-UI-003) and any other HTTP client reach a
// cluster through the API gateway with emulator IAM:
//
//	* /v1/projects/{project}/locations/{location}/gkeMemberships/{cluster}/{kubernetes path}
//
// Every cluster is an implicit fleet membership named like the cluster.
// {project} is the Project ID or number; {location} is the cluster's
// location, its region, or "global". The caller needs
// gkehub.gateway.{get,post,put,patch,delete} for the request's method on
// the membership, and the request runs on the API server as the caller's
// principal (Impersonate-User), so the cluster's IAM authorization webhook
// and RBAC decide what it may do, as for kubectl.

const connectGatewayHost = "connectgateway.googleapis.com"

// gatewayVerbs maps HTTP methods to gkehub.gateway.* permission verbs.
var gatewayVerbs = map[string]string{
	http.MethodGet: "get", http.MethodHead: "get", http.MethodOptions: "get",
	http.MethodPost: "post", http.MethodPut: "put", http.MethodPatch: "patch", http.MethodDelete: "delete",
}

// serveConnectGateway proxies one request to a cluster's API server.
func (s *Service) serveConnectGateway(w http.ResponseWriter, r *http.Request) {
	segs := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 8)
	if len(segs) < 7 || segs[0] != "v1" || segs[1] != "projects" || segs[3] != "locations" || segs[5] != "gkeMemberships" {
		apierr.Write(w, apierr.NotFound("Unknown Connect gateway path %q; use /v1/projects/PROJECT/locations/LOCATION/gkeMemberships/CLUSTER/KUBERNETES_PATH.", r.URL.Path))
		return
	}
	proj, loc, name := segs[2], segs[4], segs[6]
	rest := "/"
	if len(segs) == 8 {
		rest += segs[7]
	}
	verb, ok := gatewayVerbs[r.Method]
	if !ok {
		apierr.Write(w, apierr.InvalidArgument("Method %s is not supported by the Connect gateway.", r.Method))
		return
	}
	rec, err := s.membership(proj, loc, name)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	membership := "//gkehub.googleapis.com/projects/" + rec.Int.Project + "/locations/" + loc + "/memberships/" + name
	if err := s.env.Auth.Check(r.Context(), "gkehub.gateway."+verb, membership); err != nil {
		apierr.Write(w, err)
		return
	}
	c := rec.cluster()
	if c.Status != containerpb.Cluster_RUNNING && c.Status != containerpb.Cluster_RECONCILING {
		apierr.Write(w, apierr.FailedPrecondition("Cluster %s is not running (status %s).", name, c.Status))
		return
	}
	kc, err := s.kube(rec.key())
	if err != nil {
		apierr.Write(w, err)
		return
	}
	target, err := url.Parse(kc.base)
	if err != nil {
		apierr.Write(w, apierr.Internal("cluster endpoint %q: %v", kc.base, err))
		return
	}
	// Without a Principal the request would run as the admin certificate.
	user := emu.PrincipalFrom(r.Context()).Email()
	if user == "" {
		apierr.Write(w, apierr.Unauthenticated("The Connect gateway needs a caller to impersonate; the request has no Principal."))
		return
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = target.Scheme, target.Host
			pr.Out.URL.Path, pr.Out.URL.RawPath = rest, ""
			pr.Out.Host = target.Host
			// The admin client certificate authenticates the gateway; the
			// caller's own credentials and impersonation are not passed on.
			pr.Out.Header.Del("Authorization")
			for k := range pr.Out.Header {
				if strings.HasPrefix(http.CanonicalHeaderKey(k), "Impersonate-") {
					pr.Out.Header.Del(k)
				}
			}
			pr.Out.Header.Set("Impersonate-User", user)
		},
		Transport: kc.hc.Transport,
		// Watches and followed logs stream.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			apierr.Write(w, apierr.New(codes.Unavailable, "Cannot reach the API server of cluster %s: %v", name, err))
		},
	}
	proxy.ServeHTTP(w, r)
}

// membership returns the cluster a Connect gateway membership names.
func (s *Service) membership(proj, loc, name string) (*clusterRecord, error) {
	var matches []*clusterRecord
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, rec := range listClusters(tx, "") {
			if rec.Int.Name != name || (rec.Int.Project != proj && project.NumberString(rec.Int.Project) != proj) {
				continue
			}
			if loc == "global" || loc == rec.Int.Location || loc == regionOf(rec.Int.Location) {
				matches = append(matches, rec)
			}
		}
		return nil
	})
	switch len(matches) {
	case 0:
		return nil, apierr.NotFound("Membership projects/%s/locations/%s/memberships/%s not found: no GKE cluster %q in that project and location.", proj, loc, name, name)
	case 1:
		return matches[0], nil
	default:
		return nil, apierr.FailedPrecondition("Membership name %q matches clusters in several locations; use the cluster's location instead of %q.", name, loc)
	}
}
