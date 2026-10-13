//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/linuxuser586/gcpemu/internal/escapeguard"
)

// resourceTypes lists, per module in tofu/, the provider resource types
// the emulated services support (IF-001). TestOpenTofu checks that each
// module declares exactly these, so the list is the supported surface.
var resourceTypes = map[string][]string{
	"iam": {
		"google_service_account", "google_service_account_key",
		"google_service_account_iam_member", "google_service_account_iam_binding", "google_service_account_iam_policy",
		"google_project_iam_custom_role", "google_project_iam_member", "google_project_iam_binding",
		"google_iam_workload_identity_pool", "google_iam_workload_identity_pool_provider",
		"google_project_service",
	},
	"storage": {
		"google_storage_bucket", "google_storage_bucket_object",
		"google_storage_bucket_iam_member", "google_storage_bucket_iam_binding", "google_storage_bucket_iam_policy",
		"google_storage_hmac_key", "google_storage_notification",
		"google_storage_bucket_acl", "google_storage_default_object_acl", "google_storage_object_acl",
		"google_storage_bucket_access_control", "google_storage_default_object_access_control",
		"google_storage_object_access_control",
		"google_storage_managed_folder", "google_storage_managed_folder_iam_member",
		"google_storage_managed_folder_iam_binding", "google_storage_managed_folder_iam_policy",
		"google_service_account", "google_pubsub_topic", "google_pubsub_topic_iam_member",
	},
	"pubsub": {
		"google_pubsub_schema", "google_pubsub_topic", "google_pubsub_subscription",
		"google_pubsub_topic_iam_member", "google_pubsub_topic_iam_binding", "google_pubsub_topic_iam_policy",
		"google_pubsub_subscription_iam_member", "google_pubsub_subscription_iam_binding", "google_pubsub_subscription_iam_policy",
		"google_service_account",
	},
	"dns": {
		"google_dns_managed_zone", "google_dns_record_set", "google_compute_network",
	},
	"network": {
		"google_compute_network", "google_compute_subnetwork", "google_compute_firewall", "google_compute_route",
		"google_compute_address", "google_compute_global_address", "google_service_networking_connection",
		"google_compute_router", "google_compute_router_nat",
		"google_compute_network_endpoint_group", "google_compute_network_endpoint", "google_compute_network_endpoints",
		"google_compute_network_peering",
		"google_compute_region_network_endpoint_group", "google_compute_region_network_endpoint",
		"google_compute_subnetwork_iam_member", "google_compute_subnetwork_iam_binding", "google_compute_subnetwork_iam_policy",
	},
	"lb": {
		"google_compute_health_check", "google_compute_region_health_check",
		"google_compute_backend_bucket", "google_compute_backend_bucket_signed_url_key",
		"google_compute_backend_service", "google_compute_backend_service_signed_url_key", "google_compute_region_backend_service",
		"google_compute_url_map", "google_compute_region_url_map",
		"google_compute_ssl_certificate", "google_compute_region_ssl_certificate", "google_compute_managed_ssl_certificate",
		"google_compute_ssl_policy", "google_compute_region_ssl_policy",
		"google_compute_target_http_proxy", "google_compute_target_https_proxy",
		"google_compute_region_target_http_proxy", "google_compute_region_target_https_proxy",
		"google_compute_global_forwarding_rule", "google_compute_forwarding_rule",
		"google_compute_security_policy",
		"google_compute_http_health_check", "google_compute_https_health_check",
		"google_compute_target_ssl_proxy", "google_compute_target_tcp_proxy", "google_compute_region_target_tcp_proxy",
		"google_compute_target_grpc_proxy",
		"google_compute_global_network_endpoint_group", "google_compute_global_network_endpoint",
		"google_compute_network", "google_compute_subnetwork", "google_compute_network_endpoint_group",
		"google_compute_global_address", "google_storage_bucket",
	},
	"certs": {
		"google_certificate_manager_certificate", "google_certificate_manager_dns_authorization",
		"google_certificate_manager_certificate_map", "google_certificate_manager_certificate_map_entry",
		"google_certificate_manager_trust_config", "google_network_security_backend_authentication_config",
		"google_network_security_server_tls_policy", "google_network_security_client_tls_policy",
	},
	"sql": {
		"google_sql_database_instance", "google_sql_database", "google_sql_user", "google_sql_ssl_cert",
		"google_service_account",
	},
	"gke": {
		"google_container_cluster", "google_container_node_pool", "google_compute_instance_group_named_port",
		"google_compute_network", "google_compute_subnetwork", "google_service_account",
	},
	"secrets": {
		"google_secret_manager_secret", "google_secret_manager_secret_version",
		"google_secret_manager_secret_iam_member", "google_secret_manager_secret_iam_binding", "google_secret_manager_secret_iam_policy",
		"google_secret_manager_regional_secret", "google_secret_manager_regional_secret_version",
		"google_pubsub_topic", "google_service_account",
	},
	"ar": {
		"google_artifact_registry_repository",
		"google_artifact_registry_repository_iam_member", "google_artifact_registry_repository_iam_binding",
		"google_artifact_registry_repository_iam_policy",
		"google_service_account",
	},
}

// needsRuntime are the modules that start containers.
var needsRuntime = map[string]bool{"sql": true, "gke": true}

var resourceRE = regexp.MustCompile(`(?m)^resource\s+"(google_[a-z0-9_]+)"`)

// TestOpenTofu is the OpenTofu acceptance (SRS 11.1, IF-001): every
// module in tofu/ is applied, planned again (no changes allowed) and
// destroyed with the google and google-beta providers at the current and
// previous minor version. The provider block is what `gcpemu tofu-provider`
// prints. plan, apply and destroy run behind an Escape guard, so an
// Escaped request fails the module; the escape-guard subtest proves that
// the guard catches one.
//
// GCPEMU_TOFU_PROVIDERS narrows the matrix: "google-beta" (both minors),
// "google@8.6.0" or "google@8.6.0,google-beta@8.5.0". GCPEMU_TOFU_MODULES
// picks modules ("lb,dns").
func TestOpenTofu(t *testing.T) {
	checkModules(t)
	tofuBin := onPath(t, "tofu")
	providers := providerMatrix(t)
	e := start(t)
	plugins := os.Getenv("TF_PLUGIN_CACHE_DIR")
	if plugins == "" {
		plugins = filepath.Join(cacheDir(t), "tofu-plugins")
	}
	if err := os.MkdirAll(plugins, 0o755); err != nil {
		t.Fatal(err)
	}
	modules := sortedKeys(resourceTypes)
	if v := os.Getenv("GCPEMU_TOFU_MODULES"); v != "" {
		modules = strings.Split(v, ",")
	}
	var in initer // tofu init shares the plugin cache
	for _, p := range providers {
		t.Run(p.String(), func(t *testing.T) {
			// A project per provider version keeps leftovers of one run from
			// colliding with the next. Bucket names derive from it and may
			// not contain "google".
			project := "tf-" + strings.ReplaceAll(strings.ReplaceAll(p.name, "google", "g"), "-beta", "b") +
				"-" + strings.ReplaceAll(p.minor(), ".", "-")
			block := e.run(t, e.bin, "tofu-provider", "--data-dir", e.dir, "--project", project)
			t.Run("escape-guard", func(t *testing.T) {
				t.Parallel()
				canary(t, tofuBin, plugins, p, project, block, &in)
			})
			for _, m := range modules {
				t.Run(m, func(t *testing.T) {
					t.Parallel()
					if needsRuntime[m] && !e.runtime {
						t.Skip("needs a container runtime")
					}
					tf := newModule(t, tofuBin, os.DirFS(filepath.Join("tofu", m)), m, plugins, p, project, block)
					in.init(tf, plugins, p)
					tf.run(20*time.Minute, "apply", "-auto-approve", "-no-color")
					tf.planClean()
					tf.run(20*time.Minute, "destroy", "-auto-approve", "-no-color")
					if left := strings.TrimSpace(tf.run(time.Minute, "state", "list")); left != "" {
						t.Errorf("state after destroy:\n%s", left)
					}
				})
			}
		})
	}
	resp, err := http.Get("http://" + e.vars["GCPEMU_GATEWAY"] + "/_emu/v1/ready")
	if err != nil || resp.StatusCode != 200 {
		t.Errorf("ready after the modules: %v %v", resp, err)
	}
}

// checkModules asserts that each module declares exactly the resource
// types listed for it.
func checkModules(t *testing.T) {
	t.Helper()
	dirs, err := os.ReadDir("tofu")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		if _, ok := resourceTypes[d.Name()]; !ok {
			t.Errorf("tofu/%s is not in resourceTypes", d.Name())
		}
	}
	for m, want := range resourceTypes {
		files, _ := filepath.Glob(filepath.Join("tofu", m, "*.tf"))
		seen := map[string]bool{}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for _, sm := range resourceRE.FindAllStringSubmatch(string(b), -1) {
				seen[sm[1]] = true
			}
		}
		got := sortedKeys(seen)
		want = slices.Sorted(slices.Values(want))
		if !slices.Equal(got, want) {
			t.Errorf("tofu/%s declares %v, resourceTypes lists %v", m, got, want)
		}
	}
}

// provider is one provider of the matrix.
type provider struct{ name, version string }

func (p provider) String() string { return p.name + "@" + p.version }

func (p provider) minor() string {
	parts := strings.SplitN(p.version, ".", 3)
	return parts[0] + "." + parts[1]
}

// providerMatrix returns the providers of GCPEMU_TOFU_PROVIDERS (default
// "google,google-beta"): a bare name stands for the latest release of its
// current minor and of the previous one, name@X.Y.Z for that release.
func providerMatrix(t *testing.T) []provider {
	t.Helper()
	spec := os.Getenv("GCPEMU_TOFU_PROVIDERS")
	if spec == "" {
		spec = "google,google-beta"
	}
	var out []provider
	for _, s := range strings.Split(spec, ",") {
		name, version, pinned := strings.Cut(strings.TrimSpace(s), "@")
		if name != "google" && name != "google-beta" {
			t.Fatalf("GCPEMU_TOFU_PROVIDERS: %q is not google or google-beta, optionally @X.Y.Z", s)
		}
		if pinned {
			out = append(out, provider{name, version})
			continue
		}
		vs := registryVersions(t, name)
		cur := vs[len(vs)-1]
		out = append(out, provider{name, versionString(cur)})
		for i := len(vs) - 2; i >= 0; i-- {
			if vs[i][0] != cur[0] || vs[i][1] != cur[1] {
				out = append(out, provider{name, versionString(vs[i])})
				break
			}
		}
	}
	return out
}

// registryVersions returns the stable releases of hashicorp/<name> in the
// OpenTofu registry, oldest first. Each successful lookup is saved in the
// cache, and that list stands in when the registry is unreachable.
func registryVersions(t *testing.T, name string) [][3]int {
	t.Helper()
	saved := filepath.Join(cacheDir(t), "tofu-versions-"+name+".json")
	list, err := fetchVersions(name)
	if err == nil {
		b, _ := json.Marshal(list)
		if err := os.WriteFile(saved, b, 0o644); err != nil {
			t.Logf("saving the provider versions of %s: %v", name, err)
		}
	} else {
		b, rerr := os.ReadFile(saved)
		if rerr != nil || json.Unmarshal(b, &list) != nil {
			t.Fatalf("provider versions of %s (pin versions in GCPEMU_TOFU_PROVIDERS to skip the lookup): %v", name, err)
		}
		t.Logf("provider versions of %s: %v; using the cached list in %s", name, err, saved)
	}
	var vs [][3]int
	for _, v := range list {
		var n [3]int
		parts := strings.Split(v, ".")
		if len(parts) != 3 {
			continue
		}
		ok := true
		for i, p := range parts {
			if n[i], err = strconv.Atoi(p); err != nil {
				ok = false // pre-release
			}
		}
		if ok {
			vs = append(vs, n)
		}
	}
	if len(vs) == 0 {
		t.Fatalf("no releases of hashicorp/%s", name)
	}
	slices.SortFunc(vs, func(a, b [3]int) int { return slices.Compare(a[:], b[:]) })
	return vs
}

// fetchVersions asks the OpenTofu registry for the versions of
// hashicorp/<name>, retrying for about a minute.
func fetchVersions(name string) ([]string, error) {
	cl := &http.Client{Timeout: 30 * time.Second}
	var body struct{ Versions []struct{ Version string } }
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 5 * time.Second)
		}
		var resp *http.Response
		resp, err = cl.Get("https://registry.opentofu.org/v1/providers/hashicorp/" + name + "/versions")
		if err != nil {
			continue
		}
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if err == nil && resp.StatusCode != http.StatusOK {
			err = fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, v := range body.Versions {
		out = append(out, v.Version)
	}
	return out, nil
}

func versionString(v [3]int) string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }

// canaryEndpoint is the setting the escape-guard subtest leaves out of
// the provider block, and canaryHost where the provider then sends it.
const (
	canaryEndpoint = "dns_custom_endpoint"
	canaryHost     = "dns.gcpemu.invalid"
)

// canary plans a module that reads a DNS zone through a provider block
// without its dns endpoint, and fails unless the Escape guard refuses the
// read. Without it, a provider that ignored HTTPS_PROXY would pass every
// module with the guard never seeing a request.
func canary(t *testing.T, bin, plugins string, p provider, project, block string, in *initer) {
	var kept []string
	for _, l := range strings.Split(block, "\n") {
		if !strings.Contains(l, canaryEndpoint) {
			kept = append(kept, l)
		}
	}
	if len(kept) == len(strings.Split(block, "\n")) {
		t.Fatalf("the provider block sets no %s:\n%s", canaryEndpoint, block)
	}
	src := fstest.MapFS{"main.tf": {Data: []byte(`variable "project" {}

data "google_dns_managed_zone" "canary" {
  name = "escape-guard-canary"
}
`)}}
	tf := newModule(t, bin, src, "escape-guard", plugins, p, project, strings.Join(kept, "\n"))
	in.init(tf, plugins, p)
	out, escaped, err := tf.exec(5*time.Minute, "plan", "-no-color")
	if err == nil {
		t.Errorf("plan without %s succeeded:\n%s", canaryEndpoint, tail(out, 40))
	}
	if !slices.Equal(escaped, []string{canaryHost}) {
		t.Errorf("the Escape guard refused %v, want [%s]; tofu plan said:\n%s", escaped, canaryHost, tail(out, 40))
	}
}

// module is a copy of one tofu/ module bound to a provider version.
type module struct {
	t                 *testing.T
	bin, dir, project string
	env               []string
	guard             *escapeguard.Guard
}

// newModule copies the module in src and adds the provider requirements (the
// local name "google" maps to google-beta when that is under test, so the
// modules are the same for both) and the provider block.
func newModule(t *testing.T, bin string, src fs.FS, name, plugins string, p provider, project, block string) *module {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.CopyFS(dir, src); err != nil {
		t.Fatal(err)
	}
	versions := fmt.Sprintf(`terraform {
  required_providers {
    google = {
      source  = "hashicorp/%s"
      version = "= %s"
    }
    tls = {
      source  = "hashicorp/tls"
      version = "= %s"
    }
  }
}
`, p.name, p.version, tlsVersion)
	for f, s := range map[string]string{"versions.tf": versions, "provider.tf": block} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	env := append(os.Environ(), "TF_IN_AUTOMATION=1", "TF_INPUT=0", "CHECKPOINT_DISABLE=1")
	if os.Getenv("TF_PLUGIN_CACHE_DIR") == "" {
		env = append(env, "TF_PLUGIN_CACHE_DIR="+plugins)
	}
	return &module{t: t, bin: bin, dir: dir, env: env, project: project, guard: escapeguard.New(t)}
}

// initer runs tofu init for the modules one at a time, since they share
// the plugin cache.
type initer struct {
	mu           sync.Mutex
	registryDown bool // an init has used up its retries on a registry error
}

// tlsVersion is the hashicorp/tls version every module requires.
const tlsVersion = "4.4.1"

// registryErrRE matches tofu init output (with its whitespace collapsed)
// for a registry or network failure, which a later attempt may not hit.
var registryErrRE = regexp.MustCompile(`could not query provider|could not connect to registry|failed to request discovery document|` +
	`connection refused|connection reset|i/o timeout|TLS handshake timeout|no such host|Client\.Timeout exceeded`)

// init initializes tf. When the plugin cache holds every provider tf
// requires, a lock file is written from the cache first: with it (and
// only with its h1: hash for this platform) init does not contact the
// registry at all. Otherwise init is retried while it fails on the
// registry or the network.
func (in *initer) init(tf *module, plugins string, p provider) {
	tf.t.Helper()
	// A failed init ends the subtest (t.Fatalf), so the lock is released
	// by defer: otherwise every other module waits on it until the job
	// times out.
	in.mu.Lock()
	defer in.mu.Unlock()
	platform := runtime.GOOS + "_" + runtime.GOARCH
	cached := true
	for _, pv := range []provider{p, {"tls", tlsVersion}} {
		if _, err := os.Stat(filepath.Join(plugins, "registry.opentofu.org", "hashicorp", pv.name, pv.version, platform)); err != nil {
			cached = false
		}
	}
	if cached {
		tf.run(time.Minute, "providers", "lock", "-fs-mirror="+plugins, "-platform="+platform, "-no-color")
		tf.run(10*time.Minute, "init", "-no-color")
		return
	}
	backoff := []time.Duration{10 * time.Second, 30 * time.Second}
	for attempt := 0; ; attempt++ {
		out, err := tf.try(10*time.Minute, "init", "-no-color")
		if err == nil {
			return
		}
		// tofu wraps its diagnostics, so the match is on the words alone.
		registry := registryErrRE.MatchString(strings.Join(strings.Fields(out), " "))
		if !registry || in.registryDown || attempt == len(backoff) {
			in.registryDown = in.registryDown || registry
			tf.t.Fatalf("tofu init (attempt %d): %v\n%s", attempt+1, err, tail(out, 80))
		}
		tf.t.Logf("tofu init failed on the registry, retrying in %v:\n%s", backoff[attempt], tail(out, 10))
		time.Sleep(backoff[attempt])
	}
}

func (m *module) run(timeout time.Duration, args ...string) string {
	m.t.Helper()
	out, err := m.try(timeout, args...)
	if err != nil {
		m.t.Fatalf("tofu %s: %v\n%s", args[0], err, tail(out, 80))
	}
	return out
}

// try runs tofu and fails the test on each request the Escape guard
// refused.
func (m *module) try(timeout time.Duration, args ...string) (string, error) {
	m.t.Helper()
	out, escaped, err := m.exec(timeout, args...)
	for _, h := range escaped {
		m.t.Errorf("tofu %s: %s", args[0], escapeguard.Describe(h))
	}
	return out, err
}

// exec runs tofu and returns the hosts the Escape guard refused. plan,
// apply and destroy run behind the guard; init and the commands that
// stay local do not, since init downloads providers from the registry.
func (m *module) exec(timeout time.Duration, args ...string) (string, []string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	full := append([]string{"-chdir=" + m.dir}, args...)
	env := m.env
	if args[0] == "apply" || args[0] == "plan" || args[0] == "destroy" {
		full = append(full, "-var", "project="+m.project)
		env = append(slices.Clip(env), m.guard.Env()...)
	}
	cmd := exec.CommandContext(ctx, m.bin, full...)
	cmd.Env = env
	start := time.Now()
	out, err := cmd.CombinedOutput()
	m.t.Logf("tofu %s (%v)", args[0], time.Since(start).Round(100*time.Millisecond))
	return string(out), m.guard.Take(), err
}

// planClean fails the test unless a plan right after apply is empty, and
// shows the planned changes when it is not.
func (m *module) planClean() {
	m.t.Helper()
	out, err := m.try(5*time.Minute, "plan", "-detailed-exitcode", "-no-color")
	if err == nil {
		return
	}
	if i := strings.Index(out, "OpenTofu will perform"); i >= 0 {
		out = out[i:]
	}
	m.t.Errorf("plan after apply is not empty: %v\n%s", err, tail(out, 200))
}

func sortedKeys[V any](m map[string]V) []string { return slices.Sorted(maps.Keys(m)) }

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
