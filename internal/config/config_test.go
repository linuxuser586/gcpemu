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
