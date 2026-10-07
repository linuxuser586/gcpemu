package ar

import (
	"bytes"
	"sort"

	"gopkg.in/yaml.v3"
)

// Node registry configuration for GKE (FR-AR-005, FR-GKE-006).
//
// RegistriesYAML renders a k3s /etc/rancher/k3s/registries.yaml that
// sends every image pull of a node through the emulator registry at
// registryAddr (the netplane address of "ar"):
//
//	mirrors:
//	  "*":                          # any other registry (k3s >= 1.26.15)
//	    endpoint: ["http://ADDR"]
//	  ADDR:                         # images named ADDR/P/R/IMG (plain HTTP)
//	    endpoint: ["http://ADDR"]
//	  docker.io:                    # DefaultMirrorHosts
//	    endpoint: ["http://ADDR"]
//	  us-central1-docker.pkg.dev:   # one entry per Artifact Registry location
//	    endpoint: ["http://ADDR"]
//	configs:                        # only with credentials
//	  ADDR:
//	    auth: {username: oauth2accesstoken, password: TOKEN}
//
// containerd passes the original registry in the "ns" query parameter of
// mirror requests, which the registry uses to serve Artifact Registry
// repositories (by location) or pull public images through its cache.
// Mirror keys must be exact hosts or "*": k3s/containerd do not accept
// partial wildcards such as "*-docker.pkg.dev", so every location host is
// listed. Public-registry mirroring is anonymous; credentials are only
// needed for Artifact Registry repositories when IAM is enforcing.
//
// Verified with rancher/k3s v1.36.5: k3s writes a containerd hosts.toml
// per key (and _default for "*"), and node system images (pause, coredns,
// local-path-provisioner) are pulled through the mirror.
//
// k3s falls back to the original registry when the mirror fails; nodes
// without egress (private clusters) can disable that with
// --disable-default-registry-endpoint so failures surface immediately.

// RegistryAuth is the credential a node presents to the registry, e.g.
// Username "oauth2accesstoken" with an emulator access token, or
// "_json_key" with a service account key (FR-AR-003).
type RegistryAuth struct {
	Username string
	Password string
}

type k3sRegistries struct {
	Mirrors map[string]k3sMirror `yaml:"mirrors"`
	Configs map[string]k3sConfig `yaml:"configs,omitempty"`
}

type k3sMirror struct {
	Endpoint []string `yaml:"endpoint"`
}

type k3sConfig struct {
	Auth *k3sAuth `yaml:"auth,omitempty"`
}

type k3sAuth struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// RegistriesYAML returns a k3s registries.yaml routing pulls for every
// Artifact Registry location in locations (all locations when empty), the
// DefaultMirrorHosts and any other registry ("*") to the registry at
// registryAddr (host:port, plain HTTP). The first auth, if any, is
// configured for registryAddr.
func RegistriesYAML(registryAddr string, locations []string, auth ...RegistryAuth) []byte {
	if len(locations) == 0 {
		for l := range arLocations {
			locations = append(locations, l)
		}
		sort.Strings(locations)
	}
	ep := k3sMirror{Endpoint: []string{"http://" + registryAddr}}
	out := k3sRegistries{Mirrors: map[string]k3sMirror{"*": ep, registryAddr: ep}}
	for _, h := range DefaultMirrorHosts {
		out.Mirrors[h] = ep
	}
	for _, l := range locations {
		out.Mirrors[l+"-docker.pkg.dev"] = ep
	}
	if len(auth) > 0 && auth[0].Username != "" {
		out.Configs = map[string]k3sConfig{registryAddr: {Auth: &k3sAuth{Username: auth[0].Username, Password: auth[0].Password}}}
	}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(out); err != nil {
		panic(err) // plain data; cannot fail
	}
	_ = enc.Close()
	return b.Bytes()
}
