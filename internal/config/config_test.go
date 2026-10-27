package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPrecedence(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "gcpemu.yaml")
	if err := os.WriteFile(file, []byte("iamMode: enforce\nlogFormat: json\nports:\n  gcs: 9000\nservices: [gcs]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Defaults()
	if err := c.LoadFile(file); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"GCPEMU_LOG_FORMAT": "text", "GCPEMU_PORTS": "pubsub=9001", "GCPEMU_WAIT_TIMEOUT": "5s"}
	if err := c.LoadEnv(func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
	c.ApplyFlags(&Config{IAMMode: IAMOff, LogFormat: "json"}, map[string]bool{"IAMMode": true})

	if c.IAMMode != IAMOff {
		t.Errorf("flag should win: IAMMode = %q", c.IAMMode)
	}
	if c.LogFormat != "text" {
		t.Errorf("env should beat file: LogFormat = %q", c.LogFormat)
	}
	if c.Port("gcs") != 9000 || c.Port("pubsub") != 9001 || c.Port("gateway") != 4562 {
		t.Errorf("ports = %v", c.Ports)
	}
	if c.WaitTimeout != 5*time.Second {
		t.Errorf("WaitTimeout = %v", c.WaitTimeout)
	}
	if !reflect.DeepEqual(c.Services, []string{"gcs"}) {
		t.Errorf("Services = %v", c.Services)
	}
}

// TestDefaultsIgnoreCI is ADR 0039: the CI defaults are the CLI's, so
// Defaults and ConsoleEnabled give the same answer whatever CI holds.
func TestDefaultsIgnoreCI(t *testing.T) {
	t.Setenv("CI", "true")
	c := Defaults()
	if c.Ephemeral || c.LogFormat != "text" {
		t.Errorf("Defaults under CI=true: Ephemeral %v, LogFormat %q; want false, text", c.Ephemeral, c.LogFormat)
	}
	if !c.ConsoleEnabled() {
		t.Error("a nil Console should mean on, under CI=true too")
	}
}

// TestLoadFileExplicitZero: a key the file names wins even when its value
// is the zero one, so "ephemeral: false" beats a default of true.
func TestLoadFileExplicitZero(t *testing.T) {
	file := filepath.Join(t.TempDir(), "gcpemu.yaml")
	if err := os.WriteFile(file, []byte("ephemeral: false\nstrictProjects: false\nlogLevel:\nbuild:\n  concurrency: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Defaults()
	c.Ephemeral, c.StrictProjects, c.LogLevel = true, true, "warn"
	if err := c.LoadFile(file); err != nil {
		t.Fatal(err)
	}
	if c.Ephemeral || c.StrictProjects || c.Build.Concurrency != 0 {
		t.Errorf("explicit zeros not applied: Ephemeral %v, StrictProjects %v, Build.Concurrency %d", c.Ephemeral, c.StrictProjects, c.Build.Concurrency)
	}
	if c.LogLevel != "warn" || c.IAMMode != IAMAudit {
		t.Errorf("keys absent or empty in the file changed: LogLevel %q, IAMMode %q", c.LogLevel, c.IAMMode)
	}
}

// TestConsoleSetting: the console is on by default, and "false" in the
// file or the environment turns it off.
func TestConsoleSetting(t *testing.T) {
	c := Defaults()
	if !c.ConsoleEnabled() {
		t.Error("console should be on by default")
	}
	file := filepath.Join(t.TempDir(), "gcpemu.yaml")
	if err := os.WriteFile(file, []byte("console: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.LoadFile(file); err != nil {
		t.Fatal(err)
	}
	if c.ConsoleEnabled() {
		t.Error("console: false in the file should turn it off")
	}
	c = Defaults()
	if err := c.LoadEnv(func(k string) string { return map[string]string{"GCPEMU_CONSOLE": "false"}[k] }); err != nil {
		t.Fatal(err)
	}
	if c.ConsoleEnabled() {
		t.Error("GCPEMU_CONSOLE=false should turn it off")
	}
}

func TestResolveServices(t *testing.T) {
	got := ResolveServices([]string{"gke"})
	want := []string{"iam", "compute", "dns", "ar", "gke"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ResolveServices(gke) = %v, want %v", got, want)
	}
	if len(ResolveServices(nil)) != len(AllServices) {
		t.Error("empty selection should mean all services")
	}
}

func TestValidate(t *testing.T) {
	c := Defaults()
	c.Bind = "0.0.0.0"
	c.IAMMode = IAMOff
	if err := c.Validate(); err == nil {
		t.Error("expected refusal to bind 0.0.0.0 with IAM off")
	}
	c.Insecure = true
	if err := c.Validate(); err != nil {
		t.Error(err)
	}
	c.Services = []string{"bogus"}
	if err := c.Validate(); err == nil {
		t.Error("expected unknown service error")
	}

	// --lb-bind exposes only the load balancer: no insecure flag needed.
	c = Defaults()
	c.IAMMode = IAMOff
	c.LBBind = []string{"192.168.1.20", "fd00::1"}
	if err := c.Validate(); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{"0.0.0.0", "::", "lan.example"} {
		c.LBBind = []string{bad}
		if err := c.Validate(); err == nil {
			t.Errorf("--lb-bind %s: expected an error", bad)
		}
	}

	// --gke-bind and --ar-bind require authentication there, so they
	// need no insecure flag either, and take the same addresses.
	c = Defaults()
	c.IAMMode = IAMOff
	c.GKEBind = []string{"192.168.1.20"}
	c.ARBind = []string{"192.168.1.20", "fd00::1"}
	if err := c.Validate(); err != nil {
		t.Error(err)
	}
	c.ARBind = nil
	for _, bad := range []string{"0.0.0.0", "lan.example"} {
		c.GKEBind = []string{bad}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "--gke-bind") {
			t.Errorf("--gke-bind %s: err = %v", bad, err)
		}
	}
	c.GKEBind = nil
	c.ARBind = []string{"::"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "--ar-bind") {
		t.Errorf("--ar-bind ::: err = %v", err)
	}

	// --gfe-bind too (#462).
	c.ARBind = nil
	c.GFEBind = []string{"192.168.1.20"}
	if err := c.Validate(); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{"0.0.0.0", "lan.example"} {
		c.GFEBind = []string{bad}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "--gfe-bind") {
			t.Errorf("--gfe-bind %s: err = %v", bad, err)
		}
	}

	// GCPEMU_VM_RUNTIME is auto, true or false (ADR 0012).
	c = Defaults()
	if c.VMRuntime != "auto" {
		t.Errorf("default VMRuntime = %q, want auto", c.VMRuntime)
	}
	for _, ok := range []string{"auto", "true", "false", "TRUE", ""} {
		c.VMRuntime = ok
		if err := c.Validate(); err != nil {
			t.Errorf("VMRuntime %q: %v", ok, err)
		}
	}
	c.VMRuntime = "yes"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "GCPEMU_VM_RUNTIME") {
		t.Errorf("VMRuntime yes: err = %v", err)
	}

	// GCPEMU_LOOPBACK_ALIASES is auto, true or false too (ADR 0012).
	c = Defaults()
	if c.LoopbackAliases != "auto" {
		t.Errorf("default LoopbackAliases = %q, want auto", c.LoopbackAliases)
	}
	for _, ok := range []string{"auto", "true", "false", "False", ""} {
		c.LoopbackAliases = ok
		if err := c.Validate(); err != nil {
			t.Errorf("LoopbackAliases %q: %v", ok, err)
		}
	}
	c.LoopbackAliases = "yes"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "GCPEMU_LOOPBACK_ALIASES") {
		t.Errorf("LoopbackAliases yes: err = %v", err)
	}

	// GCPEMU_CONTAINERIZED is auto, true or false too (#502).
	c = Defaults()
	if c.Containerized != "auto" {
		t.Errorf("default Containerized = %q, want auto", c.Containerized)
	}
	for _, ok := range []string{"auto", "true", "false", "FALSE", ""} {
		c.Containerized = ok
		if err := c.Validate(); err != nil {
			t.Errorf("Containerized %q: %v", ok, err)
		}
	}
	c.Containerized = "bogus"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "--containerized") ||
		!strings.Contains(err.Error(), "GCPEMU_CONTAINERIZED") || !strings.Contains(err.Error(), "containerized)") {
		t.Errorf("Containerized bogus: err = %v", err)
	}
}

func TestLRO(t *testing.T) {
	c := Defaults()
	c.LROLatency = map[string]string{"gke": "20s", "*": "1s"}
	if c.LRO("gke") != 20*time.Second || c.LRO("sql") != time.Second {
		t.Errorf("LRO = %v %v", c.LRO("gke"), c.LRO("sql"))
	}
	// API names alias their Service ID; the Service ID wins over its alias.
	c.LROLatency = map[string]string{"container": "20s", "sqladmin": "2s", "sql": "3s"}
	if c.LRO("gke") != 20*time.Second || c.LRO("sql") != 3*time.Second || c.LRO("lb") != 0 {
		t.Errorf("aliases: LRO = %v %v %v", c.LRO("gke"), c.LRO("sql"), c.LRO("lb"))
	}
}

// TestPorts is FR-CORE-042: a bare 0 makes every listener without its own
// entry pick a free port; named entries still win; ranges are validated.
func TestPorts(t *testing.T) {
	p, err := ParsePorts("0, gcs=4443")
	if err != nil || !reflect.DeepEqual(p, map[string]int{AllPorts: 0, "gcs": 4443}) {
		t.Fatalf("ParsePorts = %v, %v", p, err)
	}
	if _, err := ParsePorts("gcs=x"); err == nil {
		t.Error("ParsePorts accepted a non-numeric port")
	}
	c := Defaults()
	// Not 4510, inside LocalStack's external-service range (#645).
	if c.Port("gateway") != 4562 {
		t.Fatalf("default gateway port = %d, want 4562", c.Port("gateway"))
	}
	// Not 5000, which macOS's AirPlay Receiver holds on all interfaces (#641).
	if c.Port("ar") != 4561 {
		t.Errorf("default registry port = %d, want 4561", c.Port("ar"))
	}
	if _, ok := c.PortSet("lb"); ok {
		t.Error("lb port set by default")
	}
	if err := c.LoadEnv(func(k string) string { return map[string]string{"GCPEMU_PORTS": "0,gcs=4443"}[k] }); err != nil {
		t.Fatal(err)
	}
	if c.Port("gateway") != 0 || c.Port("gcs") != 4443 || c.Port("frontend") != 0 {
		t.Errorf("ports with AllPorts = %v", c.Ports)
	}
	if v, ok := c.PortSet("lb"); !ok || v != 0 {
		t.Errorf("PortSet(lb) = %d, %v", v, ok)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		rng    string
		lo, hi int
		bad    bool
	}{
		{"20000-20999", 20000, 20999, false},
		{" 5 - 5 ", 5, 5, false},
		{"20999-20000", 0, 0, true},
		{"0-10", 0, 0, true},
		{"1-70000", 0, 0, true},
		{"20000", 0, 0, true},
	} {
		c := Defaults()
		c.PortRange = tc.rng
		lo, hi, ok, err := c.ParsePortRange()
		if tc.bad {
			if err == nil || c.Validate() == nil {
				t.Errorf("range %q accepted", tc.rng)
			}
			continue
		}
		if err != nil || !ok || lo != tc.lo || hi != tc.hi {
			t.Errorf("range %q = %d-%d %v %v", tc.rng, lo, hi, ok, err)
		}
	}
	c = Defaults()
	c.Ports[AllPorts] = 8000
	if err := c.Validate(); err == nil {
		t.Error("a bare non-zero port was accepted")
	}
}

// TestStateHome is ADR 0008: $XDG_STATE_HOME wins on every OS; otherwise
// Windows uses %LOCALAPPDATA%\gcpemu and other OSes ~/.local/state/gcpemu.
func TestStateHome(t *testing.T) {
	local := filepath.Join(t.TempDir(), "AppData", "Local")
	t.Setenv("LOCALAPPDATA", local)
	t.Setenv("XDG_STATE_HOME", "")
	if got, want := stateHome("windows"), filepath.Join(local, "gcpemu"); got != want {
		t.Errorf("windows: stateHome = %q, want %q", got, want)
	}
	if home, err := os.UserHomeDir(); err == nil {
		if got, want := stateHome("linux"), filepath.Join(home, ".local", "state", "gcpemu"); got != want {
			t.Errorf("linux: stateHome = %q, want %q", got, want)
		}
	}
	xdg := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdg)
	for _, goos := range []string{"windows", "linux", "darwin"} {
		if got, want := stateHome(goos), filepath.Join(xdg, "gcpemu"); got != want {
			t.Errorf("%s with XDG_STATE_HOME: stateHome = %q, want %q", goos, got, want)
		}
	}
	c := Defaults()
	c.Instance = "dev"
	if got, want := c.InstanceDir(), filepath.Join(xdg, "gcpemu", "dev"); got != want {
		t.Errorf("InstanceDir = %q, want %q", got, want)
	}
}

// NoSQLHostClients is for emutest only (#316): no env var reaches it.
func TestNoSQLHostClientsNotFromEnv(t *testing.T) {
	c := Defaults()
	set := map[string]string{"GCPEMU_": "true", "GCPEMU_-": "true", "GCPEMU_NO_SQL_HOST_CLIENTS": "true"}
	if err := c.LoadEnv(func(k string) string { return set[k] }); err != nil {
		t.Fatal(err)
	}
	if c.NoSQLHostClients {
		t.Fatal("an env var set NoSQLHostClients")
	}
}

// Cloud Build is a Runtime Service over Cloud Storage, Artifact Registry,
// IAM, Pub/Sub and Secret Manager (#392).
func TestBuildService(t *testing.T) {
	got := ResolveServices([]string{"build"})
	want := []string{"iam", "ar", "pubsub", "secrets", "gcs", "build"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ResolveServices(build) = %v, want %v", got, want)
	}
	if !RuntimeServices["build"] {
		t.Error("build is not a Runtime Service")
	}
}

// build.concurrency caps the WORKING Builds: 4 unless set, from the
// config file or GCPEMU_BUILD_CONCURRENCY, and at least 1.
func TestBuildConcurrency(t *testing.T) {
	c := Defaults()
	if c.Build.Concurrency != 4 {
		t.Errorf("default = %d, want 4", c.Build.Concurrency)
	}
	f := filepath.Join(t.TempDir(), "gcpemu.yaml")
	if err := os.WriteFile(f, []byte("build:\n  concurrency: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.LoadFile(f); err != nil || c.Build.Concurrency != 2 {
		t.Errorf("from file = %d, %v; want 2", c.Build.Concurrency, err)
	}
	env := map[string]string{"GCPEMU_BUILD_CONCURRENCY": "1"}
	if err := c.LoadEnv(func(k string) string { return env[k] }); err != nil || c.Build.Concurrency != 1 {
		t.Errorf("from env = %d, %v; want 1", c.Build.Concurrency, err)
	}
	flags := Defaults()
	flags.Build.Concurrency = 3
	c.ApplyFlags(&flags, map[string]bool{"Build.Concurrency": true})
	if c.Build.Concurrency != 3 {
		t.Errorf("from --build-concurrency = %d, want 3", c.Build.Concurrency)
	}
	c.Build.Concurrency = 0
	if err := c.Validate(); err == nil {
		t.Error("concurrency 0 accepted")
	}
}

// External emulators are keyed by API name and configured by address
// (#623, ADR 0027).
func TestExternal(t *testing.T) {
	c := Defaults()
	if err := c.LoadEnv(func(k string) string {
		if k == "GCPEMU_EXTERNAL" {
			return "firestore=localhost:8080, spanner=127.0.0.1:9010"
		}
		return ""
	}); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"firestore": "localhost:8080", "spanner": "127.0.0.1:9010"}
	if !reflect.DeepEqual(c.External, want) {
		t.Fatalf("External = %v, want %v", c.External, want)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []map[string]string{
		{"firestore": "localhost"},          // no port
		{"firestore": "localhost:http"},     // not a port number
		{"firestore": ""},                   // no address
		{"Firestore": "localhost:8080"},     // not an API name
		{"firestore.v1": "localhost:8080"},  // not an API name
		{"": "localhost:8080"},              // no API name
		{"firestore": "http://localhost:1"}, // a URL, not an address
	} {
		c.External = bad
		if err := c.Validate(); err == nil {
			t.Errorf("--external %v: expected an error", bad)
		}
	}
}

// "managed" has the Instance run the External emulator itself, for the
// four APIs Google ships an emulator for and no other (#624, ADR 0029).
func TestExternalManaged(t *testing.T) {
	c := Defaults()
	c.External = map[string]string{"firestore": "managed", "datastore": "managed", "spanner": "managed", "bigtable": "managed"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, api := range []string{"pubsub", "foo"} {
		c.External = map[string]string{api: "managed"}
		err := c.Validate()
		if err == nil {
			t.Fatalf("--external %s=managed accepted", api)
		}
		for _, want := range []string{api, "bigtable", "datastore", "firestore", "spanner"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("--external %s=managed: %q does not name %s", api, err, want)
			}
		}
	}
}
