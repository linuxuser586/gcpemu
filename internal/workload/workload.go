// Package workload holds what the emulator gives the containers that run
// users' code as on GCP, GKE pods and Build steps, so that unmodified
// clients reach the Instance by real hostname as a service account: the
// metadata and registry proxies their Sidecar agents serve, the DNS
// answers that send Google hostnames to the Sidecar agent's :443 relay to
// the Google front end, and the names of the Instance CA bundle and its
// variables.
package workload

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"

	"github.com/linuxuser586/gcpemu/internal/frontend"
)

// The Instance CA bundle in a workload container, and the system bundle
// it replaces.
const (
	CABundleKey      = "ca-certificates.crt"
	CABundleDir      = "/etc/gcpemu/certs"
	CABundlePath     = CABundleDir + "/" + CABundleKey
	SystemBundlePath = "/etc/ssl/certs/ca-certificates.crt"
)

// CAEnvVars are set to the bundle path in every workload container: Go
// and OpenSSL, Python requests and Node.js read them.
var CAEnvVars = []string{"SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "NODE_EXTRA_CA_CERTS"}

// MetadataHostEnv tells Google client libraries (Go, Python, Java, Node)
// that they run on GCE, so they use the metadata server without probing.
// On GCP the probe is reliable because machines report a Google DMI
// product name; emulated ones cannot, and a probe racing a still-starting
// DNS would make a process decide once and for all that it has no
// credentials.
const MetadataHostEnv = "GCE_METADATA_HOST"

// MetadataProxy forwards metadata requests to the emulator at
// base/md/<caller IP>/<path>, where the caller's address picks its
// identity. It does not add X-Forwarded-For (the metadata server rejects
// requests that carry it, like GCE's) and drops Authorization.
func MetadataProxy(base string) http.Handler {
	u, _ := url.Parse(base)
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			src, _, _ := net.SplitHostPort(pr.In.RemoteAddr)
			pr.Out.URL.Scheme = u.Scheme
			pr.Out.URL.Host = u.Host
			pr.Out.URL.Path = u.Path + "/md/" + src + pr.In.URL.Path
			pr.Out.URL.RawPath = ""
			pr.Out.Host = u.Host
			pr.Out.Header.Del("Authorization")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "metadata server unavailable: "+err.Error(), http.StatusServiceUnavailable)
		},
	}
}

// RegistryProxy forwards registry requests to Artifact Registry at
// registry (host:port, plain HTTP), adding the access token token returns
// for the request when it carries no Authorization of its own.
func RegistryProxy(registry string, token func(*http.Request) string) http.Handler {
	if registry == "" {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "Artifact Registry is not running", http.StatusServiceUnavailable)
		})
	}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = registry
			pr.Out.Host = registry
			if pr.Out.Header.Get("Authorization") == "" {
				if tok := token(pr.In); tok != "" {
					pr.Out.Header.Set("Authorization", "Bearer "+tok)
				}
			}
		},
	}
}

// TokenSource caches the access token served as JSON at URL, as the
// metadata server serves it.
type TokenSource struct {
	URL string

	mu  sync.Mutex
	tok string
	exp time.Time
}

// Token returns the cached token, fetching a new one shortly before it
// expires; "" when none can be had.
func (t *TokenSource) Token() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tok != "" && time.Now().Before(t.exp) {
		return t.tok
	}
	resp, err := http.Get(t.URL)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var v struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&v) != nil {
		return ""
	}
	t.tok = v.AccessToken
	t.exp = time.Now().Add(time.Duration(max(v.ExpiresIn-60, 30)) * time.Second)
	return t.tok
}

// DNSAnswer returns the names a workload's DNS relay answers with addr,
// where its Sidecar agent serves: metadata.google.internal, and, when the
// Sidecar agent relays to the Google front end, the front end's hostnames and every
// LOCATION-docker.pkg.dev. Other names resolve as usual.
func DNSAnswer(frontendHosts []string, relaysFrontend bool, addr net.IP) func(string) net.IP {
	hosts := map[string]bool{}
	for _, h := range frontendHosts {
		hosts[frontend.Normalize(h)] = true
	}
	return func(name string) net.IP {
		switch {
		case name == "metadata.google.internal" || name == "metadata":
			return addr
		case relaysFrontend && (hosts[name] || frontend.IsRegistryHost(name)):
			return addr
		}
		return nil
	}
}
