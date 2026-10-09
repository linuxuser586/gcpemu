package gke

import (
	"context"
	"encoding/base64"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"cloud.google.com/go/container/apiv1/containerpb"
	"gopkg.in/yaml.v3"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Kubeconfig convenience (FR-CORE-004): the emulator keeps
// <instance-dir>/kubeconfig with one context per running cluster, named
// like gcloud's (gke_PROJECT_LOCATION_NAME) and authenticating with the
// cluster's admin client certificate, so kubectl works without gcloud.
// GET /container/_emu/kubeconfig?cluster=projects/P/locations/L/clusters/C
// returns the same for one cluster. `gcloud container clusters
// get-credentials` produces the GKE-style kubeconfig (endpoint, CA and
// gke-gcloud-auth-plugin) instead, which authenticates with emulator IAM
// tokens.

func (s *Service) kubeconfigPath() string {
	return filepath.Join(s.env.Config.InstanceDir(), "kubeconfig")
}

type kcNamed struct {
	Name    string         `yaml:"name"`
	Cluster map[string]any `yaml:"cluster,omitempty"`
	User    map[string]any `yaml:"user,omitempty"`
	Context map[string]any `yaml:"context,omitempty"`
}

type kubeconfigFile struct {
	APIVersion     string    `yaml:"apiVersion"`
	Kind           string    `yaml:"kind"`
	Clusters       []kcNamed `yaml:"clusters"`
	Users          []kcNamed `yaml:"users"`
	Contexts       []kcNamed `yaml:"contexts"`
	CurrentContext string    `yaml:"current-context"`
}

// contextName is gcloud's context name for a cluster.
func contextName(projectID, location, name string) string {
	return "gke_" + projectID + "_" + location + "_" + name
}

// buildKubeconfig renders a kubeconfig for the clusters matching keep.
func (s *Service) buildKubeconfig(keep func(*clusterRecord) bool) []byte {
	var recs []*clusterRecord
	_ = s.env.Store.View(func(tx store.Tx) error {
		recs = listClusters(tx, "")
		return nil
	})
	sort.Slice(recs, func(i, j int) bool { return recs[i].key() < recs[j].key() })
	kc := kubeconfigFile{APIVersion: "v1", Kind: "Config", Clusters: []kcNamed{}, Users: []kcNamed{}, Contexts: []kcNamed{}}
	for _, rec := range recs {
		if keep != nil && !keep(rec) {
			continue
		}
		c := rec.cluster()
		if c.Status != containerpb.Cluster_RUNNING && c.Status != containerpb.Cluster_RECONCILING {
			continue
		}
		rt := s.rtFor(rec.key())
		rt.mu.Lock()
		creds := rt.creds
		rt.mu.Unlock()
		if len(creds.Cert) == 0 || c.Endpoint == "" {
			continue
		}
		name := contextName(rec.Int.Project, rec.Int.Location, rec.Int.Name)
		server := "https://" + c.Endpoint
		if rec.Int.APIHostPort != 0 {
			server = "https://127.0.0.1:" + strconv.Itoa(rec.Int.APIHostPort)
		}
		kc.Clusters = append(kc.Clusters, kcNamed{Name: name, Cluster: map[string]any{
			"server":                     server,
			"certificate-authority-data": base64.StdEncoding.EncodeToString(creds.CA),
		}})
		kc.Users = append(kc.Users, kcNamed{Name: name, User: map[string]any{
			"client-certificate-data": base64.StdEncoding.EncodeToString(creds.Cert),
			"client-key-data":         base64.StdEncoding.EncodeToString(creds.Key),
		}})
		kc.Contexts = append(kc.Contexts, kcNamed{Name: name, Context: map[string]any{"cluster": name, "user": name}})
		kc.CurrentContext = name
	}
	b, _ := yaml.Marshal(kc)
	return b
}

// writeKubeconfig rewrites <instance-dir>/kubeconfig.
func (s *Service) writeKubeconfig() {
	p := s.kubeconfigPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, s.buildKubeconfig(nil), 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p)
}

// serveKubeconfig serves GET /_emu/kubeconfig[?cluster=NAME].
func (s *Service) serveKubeconfig(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("cluster")
	var keep func(*clusterRecord) bool
	if name != "" {
		ref, err := parseRef(name, "", "", "", "", 1)
		if err != nil {
			apierr.Write(w, err)
			return
		}
		if err := s.env.Auth.Check(r.Context(), "container.clusters.getCredentials", ref.clusterResource()); err != nil {
			apierr.Write(w, err)
			return
		}
		if _, err := s.api.load(ref); err != nil {
			apierr.Write(w, err)
			return
		}
		keep = func(rec *clusterRecord) bool { return rec.key() == ref.key() }
	} else if err := s.env.Auth.Check(r.Context(), "container.clusters.getCredentials", "//container.googleapis.com/projects/-"); err != nil {
		apierr.Write(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write(s.buildKubeconfig(keep))
}

// clusterJWKS returns the cluster's service account token signing keys
// (the API server's /openid/v1/jwks), for Workload Identity federation.
func (s *Service) clusterJWKS(ctx context.Context, key string) ([]*containerpb.Jwk, error) {
	kc, err := s.kube(key)
	if err != nil {
		return nil, err
	}
	var v struct {
		Keys []struct {
			Kty string `json:"kty"`
			Alg string `json:"alg"`
			Use string `json:"use"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
			X   string `json:"x"`
			Y   string `json:"y"`
			Crv string `json:"crv"`
		} `json:"keys"`
	}
	if err := kc.get(ctx, "/openid/v1/jwks", &v); err != nil {
		return nil, apierr.Internal("reading the cluster's signing keys: %v", err)
	}
	out := make([]*containerpb.Jwk, 0, len(v.Keys))
	for _, k := range v.Keys {
		out = append(out, &containerpb.Jwk{Kty: k.Kty, Alg: k.Alg, Use: k.Use, Kid: k.Kid, N: k.N, E: k.E, X: k.X, Y: k.Y, Crv: k.Crv})
	}
	return out, nil
}
