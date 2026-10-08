package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestPrecedence(t *testing.T) {
	t.Setenv("CI", "")
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
	if c.Port("gcs") != 9000 || c.Port("pubsub") != 9001 || c.Port("gateway") != 4510 {
		t.Errorf("ports = %v", c.Ports)
	}
	if c.WaitTimeout != 5*time.Second {
		t.Errorf("WaitTimeout = %v", c.WaitTimeout)
	}
	if !reflect.DeepEqual(c.Services, []string{"gcs"}) {
		t.Errorf("Services = %v", c.Services)
	}
}

func TestCIDefaults(t *testing.T) {
	t.Setenv("CI", "true")
	c := Defaults()
	if !c.Ephemeral || c.LogFormat != "json" {
		t.Errorf("CI defaults not applied: %+v", c)
	}
	if c.ConsoleEnabled() {
		t.Error("console should be off under CI=true unless asked for")
	}
	on := true
	c.ApplyFlags(&Config{Console: &on}, map[string]bool{"Console": true})
	if !c.ConsoleEnabled() {
		t.Error("--console should turn the console on under CI=true")
	}
}

// TestConsoleSetting: the console is on by default, and "false" in the
// file or the environment turns it off.
func TestConsoleSetting(t *testing.T) {
	t.Setenv("CI", "")
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
}

func TestLRO(t *testing.T) {
	c := Defaults()
	c.LROLatency = map[string]string{"container": "20s", "*": "1s"}
	if c.LRO("container") != 20*time.Second || c.LRO("sqladmin") != time.Second {
		t.Errorf("LRO = %v %v", c.LRO("container"), c.LRO("sqladmin"))
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
	if c.Port("gateway") != 4510 {
		t.Fatalf("default gateway port = %d", c.Port("gateway"))
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
