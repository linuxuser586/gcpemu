package gke

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/services/iam"
)

// Workload Identity Federation for GKE (FR-GKE-005, FR-INT-007).
//
// Each node's metadata proxy forwards pod requests here tagged with the
// pod IP. The emulator finds the pod, its Kubernetes service account and
// the account's iam.gke.io/gcp-service-account annotation, and serves the
// GCE metadata API for the matching identity:
//
//   - Workload Identity off for the node pool (or a host-network caller):
//     the node's service account.
//   - Annotated KSA: the Google service account, whose tokens are issued
//     only if serviceAccount:PROJECT.svc.id.goog[NS/KSA] may impersonate it
//     (roles/iam.workloadIdentityUser, i.e. iam.serviceAccounts.getAccessToken).
//   - Unannotated KSA: the federated identity; the token belongs to the
//     principal serviceAccount:PROJECT.svc.id.goog[NS/KSA], so IAM bindings
//     for that member apply directly.

const annoGSA = "iam.gke.io/gcp-service-account"

// wiEntry caches a pod IP's identity.
type wiEntry struct {
	ns, ksa, gsa string
	err          error
	exp          time.Time
}

// metadataHandlers is provided by the iam service.
type metadataHandlers interface {
	MetadataHandler(id iam.MetadataIdentity) http.Handler
}

func (s *Service) serveMetadata(w http.ResponseWriter, r *http.Request, key, node, ip, path string) {
	// Every answer, errors included, carries the flavor header: client
	// libraries probe GET / and treat a missing header as "not on GCE",
	// caching the verdict for the life of the process.
	w.Header().Set("Metadata-Flavor", "Google")
	rec, err := s.loadKey(key)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	c := rec.cluster()
	var nr *nodeRecord
	for i := range rec.Int.Nodes {
		if rec.Int.Nodes[i].Name == node {
			nr = &rec.Int.Nodes[i]
		}
	}
	if nr == nil {
		http.NotFound(w, r)
		return
	}
	np := findPool(c, nr.Pool)
	if np == nil {
		http.NotFound(w, r)
		return
	}
	mh, ok := s.peerMetadata()
	if !ok {
		http.Error(w, "the iam service is not running", http.StatusServiceUnavailable)
		return
	}
	id := iam.MetadataIdentity{
		Project: rec.Int.Project,
		Zone:    nr.Zone,
		Email:   nodeServiceAccount(rec.Int.Project, np.Config.ServiceAccount),
		Attributes: map[string]string{
			"cluster-name":     c.Name,
			"cluster-location": c.Location,
			"cluster-uid":      c.Id,
		},
	}
	// Rewrite the request onto the metadata handler's path space.
	r2 := r.Clone(r.Context())
	r2.URL = &url.URL{Path: path, RawQuery: r.URL.RawQuery}
	if !wiEnabled(c, np) || !s.isPodIP(rec, ip) || path == "/" {
		mh.MetadataHandler(id).ServeHTTP(w, r2)
		return
	}
	e := s.resolvePod(r.Context(), key, ip)
	if e.err != nil {
		http.Error(w, "Workload Identity: "+e.err.Error(), http.StatusNotFound)
		return
	}
	pool := c.WorkloadIdentityConfig.WorkloadPool
	member := "serviceAccount:" + pool + "[" + e.ns + "/" + e.ksa + "]"
	acct, leaf := saLeaf(path)
	if e.gsa != "" {
		id.Email = e.gsa
		if leaf == "token" || leaf == "identity" {
			if acct != "default" && !strings.EqualFold(acct, e.gsa) {
				http.NotFound(w, r)
				return
			}
			if !s.wiAllowed(r.Context(), member, e.gsa) {
				http.Error(w, "Unable to generate access token; IAM returned 403 Forbidden: Permission 'iam.serviceAccounts.getAccessToken' denied on resource (or it may not exist). "+
					"Grant roles/iam.workloadIdentityUser on "+e.gsa+" to "+member+".", http.StatusForbidden)
				return
			}
		}
		mh.MetadataHandler(id).ServeHTTP(w, r2)
		return
	}
	// Unannotated KSA: federated identity.
	id.Email = pool
	if leaf == "token" && (acct == "default" || acct == pool) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			mh.MetadataHandler(id).ServeHTTP(w, r2) // renders the standard 403
			return
		}
		keys := s.saKeys()
		if keys == nil {
			http.Error(w, "the iam service is not running", http.StatusServiceUnavailable)
			return
		}
		tok, exp, err := keys.AccessToken(r.Context(), emu.Principal(member))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Metadata-Flavor", "Google")
		writeJSON(w, map[string]any{"access_token": tok, "expires_in": exp, "token_type": "Bearer"})
		return
	}
	if leaf == "identity" {
		http.Error(w, "Workload Identity: identity tokens require the Kubernetes service account to be annotated with "+annoGSA, http.StatusNotFound)
		return
	}
	mh.MetadataHandler(id).ServeHTTP(w, r2)
}

// saLeaf returns the account and leaf of
// /computeMetadata/v1/instance/service-accounts/{acct}/{leaf}.
func saLeaf(path string) (string, string) {
	rest, ok := strings.CutPrefix(path, "/computeMetadata/v1/instance/service-accounts/")
	if !ok {
		return "", ""
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

func (s *Service) peerMetadata() (metadataHandlers, bool) {
	p, ok := s.env.Lookup("iam")
	if !ok {
		return nil, false
	}
	mh, ok := p.(metadataHandlers)
	return mh, ok
}

// isPodIP reports whether ip lies in the cluster's pod range (host-network
// callers and the node itself get the node identity).
func (s *Service) isPodIP(rec *clusterRecord, ip string) bool {
	return rec.Int.PodCIDR != "" && containsIP(rec.Int.PodCIDR, ip)
}

// resolvePod finds the pod with ip and its service account annotation.
func (s *Service) resolvePod(ctx context.Context, key, ip string) wiEntry {
	rt := s.rtFor(key)
	if v, ok := rt.wi.Load(ip); ok {
		if e := v.(wiEntry); time.Now().Before(e.exp) {
			return e
		}
	}
	e := s.lookupPod(ctx, key, ip)
	ttl := 5 * time.Second
	if e.err != nil {
		ttl = time.Second
	}
	e.exp = time.Now().Add(ttl)
	rt.wi.Store(ip, e)
	return e
}

func (s *Service) lookupPod(ctx context.Context, key, ip string) wiEntry {
	kc, err := s.kube(key)
	if err != nil {
		return wiEntry{err: err}
	}
	var pods kubeList[kubePod]
	if err := kc.get(ctx, "/api/v1/pods?fieldSelector=status.podIP%3D"+url.QueryEscape(ip), &pods); err != nil {
		return wiEntry{err: err}
	}
	for _, p := range pods.Items {
		if p.Spec.HostNetwork || p.Status.Phase == "Succeeded" || p.Status.Phase == "Failed" {
			continue
		}
		e := wiEntry{ns: p.Metadata.Namespace, ksa: p.Spec.ServiceAccountName}
		if e.ksa == "" {
			e.ksa = "default"
		}
		var sa struct {
			Metadata objectMeta `json:"metadata"`
		}
		if err := kc.get(ctx, "/api/v1/namespaces/"+e.ns+"/serviceaccounts/"+e.ksa, &sa); err != nil {
			return wiEntry{err: err}
		}
		e.gsa = strings.TrimSpace(sa.Metadata.Annotations[annoGSA])
		return e
	}
	return wiEntry{err: errf("no pod has IP %s", ip)}
}

// wiAllowed reports whether member may impersonate gsa. IAM mode off
// allows everything.
func (s *Service) wiAllowed(ctx context.Context, member, gsa string) bool {
	if s.env.Auth.Mode() == config.IAMOff {
		return true
	}
	p, ok := s.env.Lookup("iam")
	if !ok {
		return true
	}
	t, ok := p.(emu.IAMPermissionTester)
	if !ok {
		return true
	}
	proj := "-"
	if _, dom, ok := strings.Cut(gsa, "@"); ok {
		if pid, ok := strings.CutSuffix(dom, ".iam.gserviceaccount.com"); ok {
			proj = pid
		}
	}
	res := "//iam.googleapis.com/projects/" + proj + "/serviceAccounts/" + gsa
	got := t.TestPermissions(emu.WithPrincipal(ctx, emu.Principal(member)), res, []string{"iam.serviceAccounts.getAccessToken"})
	return len(got) == 1
}
