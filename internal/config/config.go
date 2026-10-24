// Package config loads emulator configuration with the precedence
// flags > env (GCPEMU_*) > gcpemu.yaml > defaults (FR-CORE-010).
package config

import (
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// IAM enforcement modes (FR-IAM-004).
const (
	IAMOff     = "off"
	IAMAudit   = "audit"
	IAMEnforce = "enforce"
)

// Config is the full emulator configuration. Field `env` tags name the
// GCPEMU_* variable; `yaml` tags name the gcpemu.yaml key.
type Config struct {
	Instance         string            `yaml:"instance" env:"INSTANCE"`
	DataDir          string            `yaml:"dataDir" env:"DATA_DIR"`
	Ephemeral        bool              `yaml:"ephemeral" env:"EPHEMERAL"`
	Services         []string          `yaml:"services" env:"SERVICES"`
	Bind             string            `yaml:"bind" env:"BIND"`
	Insecure         bool              `yaml:"iUnderstandThisIsInsecure" env:"I_UNDERSTAND_THIS_IS_INSECURE"`
	Ports            map[string]int    `yaml:"ports" env:"PORTS"`
	PortRange        string            `yaml:"portRange" env:"PORT_RANGE"`
	IAMMode          string            `yaml:"iamMode" env:"IAM_MODE"`
	DefaultPrincipal string            `yaml:"defaultPrincipal" env:"DEFAULT_PRINCIPAL"`
	LogFormat        string            `yaml:"logFormat" env:"LOG_FORMAT"`
	LogLevel         string            `yaml:"logLevel" env:"LOG_LEVEL"`
	LROLatency       map[string]string `yaml:"lroLatency" env:"LRO_LATENCY"`
	Deterministic    bool              `yaml:"deterministic" env:"DETERMINISTIC"`
	StrictProjects   bool              `yaml:"strictProjects" env:"STRICT_PROJECTS"`
	Projects         []string          `yaml:"projects" env:"PROJECTS"`
	Seed             string            `yaml:"seed" env:"SEED"`
	WaitTimeout      time.Duration     `yaml:"waitTimeout" env:"WAIT_TIMEOUT"`
	DNSNoForward     bool              `yaml:"dnsNoForward" env:"DNS_NO_FORWARD"`
	Offline          bool              `yaml:"offline" env:"OFFLINE"`
	// NATReject makes Cloud NAT egress enforcement reject disallowed
	// connections immediately (TCP reset / ICMP unreachable) instead of
	// silently dropping them like GCP (FR-NAT-002), for fast tests.
	NATReject bool `yaml:"natReject" env:"NAT_REJECT"`
	// NATSinkResponse is the response the --offline NAT sink returns to
	// HTTP requests (FR-NAT-005): "STATUS" or "STATUS:BODY"; default
	// "503:gcpemu offline: egress blocked".
	NATSinkResponse string `yaml:"natSinkResponse" env:"NAT_SINK_RESPONSE"`
	// DefaultProject is the project ID the metadata server reports
	// (FR-IAM-008); empty means "gcpemu-project".
	DefaultProject string `yaml:"defaultProject" env:"DEFAULT_PROJECT"`
	// MetadataServiceAccount is the metadata server's default service
	// account email; empty means the project's default compute SA.
	MetadataServiceAccount string `yaml:"metadataServiceAccount" env:"METADATA_SERVICE_ACCOUNT"`
	// HostMode serves real Google hostnames to host processes through a
	// frontend container and its DNS (FR-CORE-043, internal/hostmode).
	HostMode bool `yaml:"hostMode" env:"HOST_MODE"`
	// CDNCacheSize is the Cloud CDN cache size limit (FR-CDN-008), e.g.
	// "1GiB", "512MiB" or a byte count; empty means 1 GiB.
	CDNCacheSize string `yaml:"cdnCacheSize" env:"CDN_CACHE_SIZE"`
	// Console serves the Web console under /console on the gateway
	// (FR-UI-002); nil means the default of ConsoleEnabled.
	Console *bool `yaml:"console" env:"CONSOLE"`
	// LBBind lists extra addresses on which the load balancer's forwarding
	// rule listeners also serve, on their real ports, alongside loopback.
	// Nothing else is exposed there, so it needs no Insecure.
	LBBind []string `yaml:"lbBind" env:"LB_BIND"`
	// GKEBind lists extra addresses on which every GKE cluster's
	// Kubernetes API is also served, and which its serving certificate
	// covers. The API server then accepts only real credentials.
	GKEBind []string `yaml:"gkeBind" env:"GKE_BIND"`
	// ARBind lists extra addresses on which the Artifact Registry registry
	// is also served, over TLS from the instance CA and only to
	// authenticated callers, whatever the IAM enforcement level.
	ARBind []string `yaml:"arBind" env:"AR_BIND"`
	// GFEBind lists extra addresses on which the Google front end also
	// serves real Google hostnames, on port 443 over TLS from the instance
	// CA and only to authenticated callers, whatever the IAM enforcement
	// level (ADR 0021).
	GFEBind []string `yaml:"gfeBind" env:"GFE_BIND"`
	// LinkBind lists addresses on which the Instance serves the link
	// protocol to Linked Instances, over mutual TLS and only to Instances
	// it paired with, except for Pairing requests (ADR 0027).
	LinkBind []string `yaml:"linkBind" env:"LINK_BIND"`
	// External maps the API name of each Delegated API, one no Service
	// serves (e.g. "firestore"), to the host:port of the External emulator
	// the API gateway forwards it to (ADR 0027).
	External map[string]string `yaml:"external" env:"EXTERNAL"`
	// AdminToken is the Admin token used when the API gateway is bound
	// beyond loopback (FR-UI-009); empty means a random one per start.
	// There is no flag, so that it never shows in a process listing.
	AdminToken string `yaml:"adminToken" env:"ADMIN_TOKEN"`
	// VMRuntime says whether a Containerized Instance runs on a VM runtime,
	// whose host reaches LoadBalancer Services only through the Instance's
	// published ports (ADR 0012): "auto" guesses from the runtime's info,
	// "true" and "false" override the guess.
	VMRuntime string `yaml:"vmRuntime" env:"VM_RUNTIME"`
	// LoopbackAliases says whether a Containerized Instance on a VM runtime
	// publishes Cloud SQL instances on 127.0.0.2-127.0.0.254, which it
	// can't see on the host (ADR 0012): "auto" says yes on Docker Desktop
	// only, "true" and "false" override it.
	LoopbackAliases string `yaml:"loopbackAliases" env:"LOOPBACK_ALIASES"`
	// Containerized says whether the Instance is a Containerized Instance
	// when the container runtime can't tell (#502): "auto" goes by the
	// container marker files, "true" and "false" override them. The
	// runtime's answer, when there is one, wins.
	Containerized string `yaml:"containerized" env:"CONTAINERIZED"`
	// NoSQLHostClients stops Cloud SQL serving host clients on a loopback
	// address on a VM runtime (ADR 0005), so that tests which never dial
	// an instance's reported address don't contend for 127.0.0.1 (#316).
	// Only emutest sets it: it has no flag, env var or config key.
	NoSQLHostClients bool `yaml:"-" env:"-"`
	// Build configures Cloud Build: the build: section of gcpemu.yaml and
	// the GCPEMU_BUILD_* variables.
	Build BuildConfig `yaml:"build" env:"BUILD"`
}

// BuildConfig configures Cloud Build.
type BuildConfig struct {
	// Concurrency caps how many Builds are WORKING at once; further
	// Builds wait QUEUED (build.concurrency, default 4).
	Concurrency int `yaml:"concurrency" env:"CONCURRENCY"`
}

// MinAdminTokenLen is the shortest AdminToken accepted.
const MinAdminTokenLen = 32

// ConsoleEnabled reports whether the Web console is served: as configured,
// otherwise on except under CI=true (FR-CI-003).
func (c *Config) ConsoleEnabled() bool {
	if c.Console != nil {
		return *c.Console
	}
	return os.Getenv("CI") != "true"
}

// Default service ports (Section 3.2).
var DefaultPorts = map[string]int{
	"gateway":  4510,
	"gcs":      4443,
	"pubsub":   8085,
	"ar":       5000,
	"dns":      5353,
	"metadata": 8988,
}

// AllServices lists every service name in start order (also the seed
// order: a peer a service depends on at seed time must come first).
var AllServices = []string{"iam", "compute", "dns", "certs", "ar", "pubsub", "secrets", "gcs", "sql", "gke", "lb", "cdn", "nat", "build"}

// Defaults returns the default configuration. CI=true switches on the CI
// defaults of FR-CI-003.
func Defaults() Config {
	c := Config{
		Instance:         "default",
		Bind:             "127.0.0.1",
		IAMMode:          IAMAudit,
		DefaultPrincipal: "user:dev@example.com",
		LogFormat:        "text",
		LogLevel:         "info",
		WaitTimeout:      120 * time.Second,
		Ports:            map[string]int{},
		LROLatency:       map[string]string{},
		VMRuntime:        "auto",
		LoopbackAliases:  "auto",
		Containerized:    "auto",
		Build:            BuildConfig{Concurrency: 4},
	}
	if os.Getenv("CI") == "true" {
		c.Ephemeral = true
		c.LogFormat = "json"
	}
	return c
}

// StateHome returns the base directory for instance data dirs:
// $XDG_STATE_HOME/gcpemu when set, otherwise %LOCALAPPDATA%\gcpemu on
// Windows (ADR 0008) and ~/.local/state/gcpemu elsewhere.
func StateHome() string { return stateHome(runtime.GOOS) }

func stateHome(goos string) string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "gcpemu")
	}
	if goos == "windows" {
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "gcpemu")
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "gcpemu")
	}
	return filepath.Join(home, ".local", "state", "gcpemu")
}

// InstanceDir returns the data dir: the explicit one, or one per instance.
func (c *Config) InstanceDir() string {
	if c.DataDir != "" {
		return c.DataDir
	}
	return filepath.Join(StateHome(), c.Instance)
}

// AllPorts is the Ports key that applies to every listener without its own
// entry: `--port 0` makes them all pick free ports (FR-CORE-042).
const AllPorts = "*"

// Port returns the configured port for name (0 means "pick a free port").
func (c *Config) Port(name string) int {
	if p, ok := c.PortSet(name); ok {
		return p
	}
	return DefaultPorts[name]
}

// PortSet returns the port configured for name, directly or through
// AllPorts, and whether one is.
func (c *Config) PortSet(name string) (int, bool) {
	if p, ok := c.Ports[name]; ok {
		return p, true
	}
	p, ok := c.Ports[AllPorts]
	return p, ok
}

// ParsePortRange parses PortRange ("LO-HI"); ok is false when it is empty.
func (c *Config) ParsePortRange() (lo, hi int, ok bool, err error) {
	if c.PortRange == "" {
		return 0, 0, false, nil
	}
	a, b, found := strings.Cut(c.PortRange, "-")
	lo, err1 := strconv.Atoi(strings.TrimSpace(a))
	hi, err2 := strconv.Atoi(strings.TrimSpace(b))
	if !found || err1 != nil || err2 != nil || lo < 1 || hi > 65535 || lo > hi {
		return 0, 0, false, fmt.Errorf("invalid port range %q (want LO-HI within 1-65535)", c.PortRange)
	}
	return lo, hi, true, nil
}

// lroAliases maps a Service ID to the API name that also keys its LRO
// latency, so that configs written before the Service ID keys still work.
var lroAliases = map[string]string{"gke": "container", "sql": "sqladmin"}

// LRO returns the configured LRO latency for the Service ID service
// (FR-CORE-023): its own key, else its API name alias, else "*".
func (c *Config) LRO(service string) time.Duration {
	v, ok := c.LROLatency[service]
	if !ok {
		v, ok = c.LROLatency[lroAliases[service]]
	}
	if !ok {
		v = c.LROLatency["*"]
	}
	if v == "" || v == "instant" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0
	}
	return d
}

// LoadFile overlays a YAML file onto c. A missing file is not an error.
func (c *Config) LoadFile(path string) error {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var f Config
	if err := yaml.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	c.overlay(&f, nil)
	return nil
}

// overlay copies non-zero fields of src onto c (all of them when set is nil,
// otherwise only names present in set).
func (c *Config) overlay(src *Config, set map[string]bool) {
	overlayStruct(reflect.ValueOf(c).Elem(), reflect.ValueOf(src).Elem(), set, "")
}

// overlayStruct is overlay for any struct value. A nested section
// (BuildConfig) is overlaid field by field, its fields named in set after
// the section and a dot ("Build.Concurrency").
func overlayStruct(dv, sv reflect.Value, set map[string]bool, prefix string) {
	t := dv.Type()
	for i := 0; i < t.NumField(); i++ {
		f := sv.Field(i)
		name := prefix + t.Field(i).Name
		if f.Kind() == reflect.Struct {
			overlayStruct(dv.Field(i), f, set, name+".")
			continue
		}
		if set != nil && !set[name] {
			continue
		}
		if set == nil && f.IsZero() {
			continue
		}
		switch f.Kind() {
		case reflect.Map:
			d := dv.Field(i)
			if d.IsNil() {
				d.Set(reflect.MakeMap(f.Type()))
			}
			for _, k := range f.MapKeys() {
				d.SetMapIndex(k, f.MapIndex(k))
			}
		default:
			dv.Field(i).Set(f)
		}
	}
}

// LoadEnv overlays GCPEMU_* environment variables onto c.
func (c *Config) LoadEnv(getenv func(string) string) error {
	return loadEnv(reflect.ValueOf(c).Elem(), "GCPEMU_", getenv)
}

// loadEnv overlays the variables named prefix plus each field's env tag
// onto the struct dv; a nested section's fields take its tag and "_" as
// their prefix (GCPEMU_BUILD_CONCURRENCY).
func loadEnv(dv reflect.Value, prefix string, getenv func(string) string) error {
	t := dv.Type()
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("env")
		if tag == "-" {
			continue
		}
		name := prefix + tag
		if dv.Field(i).Kind() == reflect.Struct {
			if err := loadEnv(dv.Field(i), name+"_", getenv); err != nil {
				return err
			}
			continue
		}
		v := getenv(name)
		if v == "" {
			continue
		}
		if err := setString(dv.Field(i), v); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

// setString parses v into field f. Slices are comma-separated; maps are
// comma-separated k=v pairs.
func setString(f reflect.Value, v string) error {
	switch f.Kind() {
	case reflect.String:
		f.SetString(v)
	case reflect.Bool:
		b, err := strconv.ParseBool(v)
		if err != nil {
			return err
		}
		f.SetBool(b)
	case reflect.Int:
		n, err := strconv.Atoi(v)
		if err != nil {
			return err
		}
		f.SetInt(int64(n))
	case reflect.Int64:
		if f.Type() == reflect.TypeOf(time.Duration(0)) {
			d, err := time.ParseDuration(v)
			if err != nil {
				return err
			}
			f.SetInt(int64(d))
			return nil
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return err
		}
		f.SetInt(n)
	case reflect.Slice:
		f.Set(reflect.ValueOf(SplitList(v)))
	case reflect.Pointer:
		p := reflect.New(f.Type().Elem())
		if err := setString(p.Elem(), v); err != nil {
			return err
		}
		f.Set(p)
	case reflect.Map:
		if f.IsNil() {
			f.Set(reflect.MakeMap(f.Type()))
		}
		var m map[string]string
		if f.Type().Elem().Kind() != reflect.Int {
			var err error
			if m, err = ParseKV(v); err != nil {
				return err
			}
		}
		if f.Type().Elem().Kind() == reflect.Int {
			ports, err := ParsePorts(v)
			if err != nil {
				return err
			}
			for k, n := range ports {
				f.SetMapIndex(reflect.ValueOf(k), reflect.ValueOf(n))
			}
			return nil
		}
		for k, s := range m {
			f.SetMapIndex(reflect.ValueOf(k), reflect.ValueOf(s))
		}
	default:
		return fmt.Errorf("unsupported kind %s", f.Kind())
	}
	return nil
}

// SplitList splits a comma-separated list, trimming blanks.
func SplitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ParseKV parses "a=1,b=2".
func ParseKV(v string) (map[string]string, error) {
	m := map[string]string{}
	for _, kv := range SplitList(v) {
		k, val, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, fmt.Errorf("expected key=value, got %q", kv)
		}
		m[strings.TrimSpace(k)] = strings.TrimSpace(val)
	}
	return m, nil
}

// ParsePorts parses port settings: comma-separated name=port pairs, and a
// bare port (only 0 is valid) that applies to every listener (AllPorts).
func ParsePorts(v string) (map[string]int, error) {
	out := map[string]int{}
	for _, kv := range SplitList(v) {
		k, val, ok := strings.Cut(kv, "=")
		if !ok {
			k, val = AllPorts, kv
		}
		n, err := strconv.Atoi(strings.TrimSpace(val))
		if err != nil {
			return nil, fmt.Errorf("port %q: %w", kv, err)
		}
		out[strings.TrimSpace(k)] = n
	}
	return out, nil
}

// Validate checks the configuration for consistency (NFR-SEC-001).
func (c *Config) Validate() error {
	switch c.IAMMode {
	case IAMOff, IAMAudit, IAMEnforce:
	default:
		return fmt.Errorf("invalid IAM enforcement level %q (want off, audit or enforce)", c.IAMMode)
	}
	switch c.LogFormat {
	case "json", "text":
	default:
		return fmt.Errorf("invalid log format %q", c.LogFormat)
	}
	if _, _, _, err := c.ParsePortRange(); err != nil {
		return err
	}
	switch strings.ToLower(c.VMRuntime) {
	case "", "auto", "true", "false":
	default:
		return fmt.Errorf("invalid --vm-runtime (GCPEMU_VM_RUNTIME, vmRuntime) %q (want auto, true or false)", c.VMRuntime)
	}
	switch strings.ToLower(c.LoopbackAliases) {
	case "", "auto", "true", "false":
	default:
		return fmt.Errorf("invalid --loopback-aliases (GCPEMU_LOOPBACK_ALIASES, loopbackAliases) %q (want auto, true or false)", c.LoopbackAliases)
	}
	switch strings.ToLower(c.Containerized) {
	case "", "auto", "true", "false":
	default:
		return fmt.Errorf("invalid --containerized (GCPEMU_CONTAINERIZED, containerized) %q (want auto, true or false)", c.Containerized)
	}
	for name, p := range c.Ports {
		if p < 0 || p > 65535 {
			return fmt.Errorf("invalid port %d for %s", p, name)
		}
		if name == AllPorts && p != 0 {
			return fmt.Errorf("--port %d: a port without a name must be 0 (every listener picks a free port)", p)
		}
	}
	for _, b := range c.LANBinds() {
		flag := b.Flag
		for _, a := range b.Addrs {
			ip := net.ParseIP(a)
			if ip == nil {
				return fmt.Errorf("invalid --%s %q (want an IP address of this host)", flag, a)
			}
			if ip.IsUnspecified() {
				return fmt.Errorf("invalid --%s %s: name an address of this host, not the wildcard", flag, a)
			}
		}
	}
	if err := c.validateExternal(); err != nil {
		return err
	}
	if c.Build.Concurrency < 1 {
		return fmt.Errorf("invalid --build-concurrency (GCPEMU_BUILD_CONCURRENCY, build.concurrency) %d (want at least 1)", c.Build.Concurrency)
	}
	if c.AdminToken != "" && len(c.AdminToken) < MinAdminTokenLen {
		return fmt.Errorf("GCPEMU_ADMIN_TOKEN is %d characters; use at least %d", len(c.AdminToken), MinAdminTokenLen)
	}
	if !IsLoopback(c.Bind) && c.IAMMode == IAMOff && !c.Insecure {
		return fmt.Errorf("refusing to bind %s with IAM off; pass --i-understand-this-is-insecure", c.Bind)
	}
	for _, s := range c.Services {
		if !known(s) {
			return fmt.Errorf("unknown service %q (known: %s)", s, strings.Join(AllServices, ","))
		}
	}
	return nil
}

// validateExternal checks that each --external key is an API name and
// each value a host:port.
func (c *Config) validateExternal() error {
	for _, api := range slices.Sorted(maps.Keys(c.External)) {
		addr := c.External[api]
		if !IsAPIName(api) {
			return fmt.Errorf("invalid --external %s=%s: %q is not an API name (want lower-case letters and digits, e.g. firestore)", api, addr, api)
		}
		host, port, err := net.SplitHostPort(addr)
		if _, perr := strconv.Atoi(port); err != nil || perr != nil || host == "" || strings.Contains(host, "/") {
			return fmt.Errorf("invalid --external %s=%s: want the External emulator's host:port, e.g. localhost:8080", api, addr)
		}
	}
	return nil
}

// IsAPIName reports whether s has the form of a GCP API name, the first
// label of its API.googleapis.com host (firestore, bigtableadmin).
func IsAPIName(s string) bool {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func known(s string) bool {
	for _, k := range AllServices {
		if k == s {
			return true
		}
	}
	return false
}

// IsLoopback reports whether bind is a loopback address.
func IsLoopback(bind string) bool {
	return bind == "" || bind == "localhost" || strings.HasPrefix(bind, "127.") || bind == "::1"
}

// RuntimeServices need a container runtime to do anything.
var RuntimeServices = map[string]bool{"sql": true, "gke": true, "nat": true, "build": true}

// Dependencies of each service (FR-CORE-003).
var Dependencies = map[string][]string{
	"gke":     {"iam", "compute", "ar", "dns"},
	"sql":     {"iam", "compute"},
	"lb":      {"iam", "compute", "certs", "dns", "gcs"},
	"certs":   {"iam", "dns"},
	"cdn":     {"lb", "compute"},
	"nat":     {"compute"},
	"build":   {"iam", "gcs", "ar", "pubsub", "secrets"},
	"gcs":     {"iam"},
	"ar":      {"iam"},
	"pubsub":  {"iam"},
	"secrets": {"iam"},
	"dns":     {"iam"},
	"compute": {"iam"},
}

// ResolveServices expands the selection with dependencies and returns it in
// start order. An empty selection means every service.
func ResolveServices(sel []string) []string {
	if len(sel) == 0 {
		return append([]string(nil), AllServices...)
	}
	want := map[string]bool{}
	var add func(string)
	add = func(s string) {
		if want[s] {
			return
		}
		want[s] = true
		for _, d := range Dependencies[s] {
			add(d)
		}
	}
	for _, s := range sel {
		add(s)
	}
	var out []string
	for _, s := range AllServices {
		if want[s] {
			out = append(out, s)
			delete(want, s)
		}
	}
	rest := make([]string, 0, len(want))
	for s := range want {
		rest = append(rest, s)
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// ApplyFlags overlays the fields of flags whose Go field names are in changed.
func (c *Config) ApplyFlags(flags *Config, changed map[string]bool) { c.overlay(flags, changed) }

// LANBind is one of the --lb-bind, --gke-bind, --ar-bind, --gfe-bind and
// --link-bind flags (Flag, without "--") and its addresses.
type LANBind struct {
	Flag  string
	Addrs []string
}

// LANBinds lists the five --*-bind flags, set or not, in that order.
func (c *Config) LANBinds() []LANBind {
	return []LANBind{{"lb-bind", c.LBBind}, {"gke-bind", c.GKEBind}, {"ar-bind", c.ARBind}, {"gfe-bind", c.GFEBind}, {"link-bind", c.LinkBind}}
}
