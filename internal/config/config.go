// Package config loads emulator configuration with the precedence
// flags > env (GCPEMU_*) > gcpemu.yaml > defaults (FR-CORE-010).
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
var AllServices = []string{"iam", "compute", "dns", "certs", "ar", "pubsub", "gcs", "sql", "gke", "lb", "cdn", "nat"}

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
	}
	for k, v := range DefaultPorts {
		c.Ports[k] = v
	}
	if os.Getenv("CI") == "true" {
		c.Ephemeral = true
		c.LogFormat = "json"
	}
	return c
}

// StateHome returns the base directory for instance data dirs.
func StateHome() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "gcpemu")
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

// Port returns the configured port for name (0 means "pick a free port").
func (c *Config) Port(name string) int {
	if p, ok := c.Ports[name]; ok {
		return p
	}
	return DefaultPorts[name]
}

// LRO returns the configured LRO latency for service (FR-CORE-023).
func (c *Config) LRO(service string) time.Duration {
	v, ok := c.LROLatency[service]
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
	dv := reflect.ValueOf(c).Elem()
	sv := reflect.ValueOf(src).Elem()
	t := dv.Type()
	for i := 0; i < t.NumField(); i++ {
		f := sv.Field(i)
		if set != nil && !set[t.Field(i).Name] {
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
	dv := reflect.ValueOf(c).Elem()
	t := dv.Type()
	for i := 0; i < t.NumField(); i++ {
		name := "GCPEMU_" + t.Field(i).Tag.Get("env")
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
	case reflect.Map:
		m, err := ParseKV(v)
		if err != nil {
			return err
		}
		if f.IsNil() {
			f.Set(reflect.MakeMap(f.Type()))
		}
		for k, s := range m {
			if f.Type().Elem().Kind() == reflect.Int {
				n, err := strconv.Atoi(s)
				if err != nil {
					return fmt.Errorf("%s: %w", k, err)
				}
				f.SetMapIndex(reflect.ValueOf(k), reflect.ValueOf(n))
			} else {
				f.SetMapIndex(reflect.ValueOf(k), reflect.ValueOf(s))
			}
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

// Validate checks the configuration for consistency (NFR-SEC-001).
func (c *Config) Validate() error {
	switch c.IAMMode {
	case IAMOff, IAMAudit, IAMEnforce:
	default:
		return fmt.Errorf("invalid IAM mode %q (want off, audit or enforce)", c.IAMMode)
	}
	switch c.LogFormat {
	case "json", "text":
	default:
		return fmt.Errorf("invalid log format %q", c.LogFormat)
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

// Dependencies of each service (FR-CORE-003).
var Dependencies = map[string][]string{
	"gke":     {"iam", "compute", "ar", "dns"},
	"sql":     {"iam", "compute"},
	"lb":      {"iam", "compute", "certs", "dns", "gcs"},
	"certs":   {"iam", "dns"},
	"cdn":     {"lb", "compute"},
	"nat":     {"compute"},
	"gcs":     {"iam"},
	"ar":      {"iam"},
	"pubsub":  {"iam"},
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
