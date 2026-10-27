package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/pflag"

	"github.com/linuxuser586/gcpemu/internal/config"
)

// loadUnder loads a `gcpemu start` config with CI set to ci, the given
// flags and GCPEMU_* variables, and a config file holding yaml.
func loadUnder(t *testing.T, ci string, args []string, env map[string]string, yaml string) *config.Config {
	t.Helper()
	t.Setenv("CI", ci)
	for _, k := range []string{"GCPEMU_EPHEMERAL", "GCPEMU_LOG_FORMAT", "GCPEMU_CONSOLE"} {
		t.Setenv(k, env[k])
	}
	file := filepath.Join(t.TempDir(), "gcpemu.yaml")
	if err := os.WriteFile(file, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	o := &rootOpts{configFile: file}
	fs := pflag.NewFlagSet("start", pflag.ContinueOnError)
	o.instanceFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	cfg, err := o.load(fs)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// TestCIDefaults is FR-CI-003 (ADR 0039): under CI=true a loaded config is
// Ephemeral, logs JSON and serves no Web console, and an explicit setting
// in the file, the environment or a flag wins over each.
func TestCIDefaults(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ci        string
		args      []string
		env       map[string]string
		yaml      string
		ephemeral bool
		logFormat string
		console   bool
	}{
		{name: "CI=true", ci: "true", ephemeral: true, logFormat: "json"},
		{name: "no CI", logFormat: "text", console: true},
		{name: "CI=1", ci: "1", logFormat: "text", console: true},
		{name: "CI=false", ci: "false", logFormat: "text", console: true},
		{name: "flags", ci: "true", args: []string{"--ephemeral=false", "--log-format", "text", "--console"}, logFormat: "text", console: true},
		{name: "env", ci: "true", env: map[string]string{"GCPEMU_EPHEMERAL": "false", "GCPEMU_LOG_FORMAT": "text", "GCPEMU_CONSOLE": "true"}, logFormat: "text", console: true},
		{name: "file", ci: "true", yaml: "ephemeral: false\nlogFormat: text\nconsole: true\n", logFormat: "text", console: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := loadUnder(t, tc.ci, tc.args, tc.env, tc.yaml)
			if cfg.Ephemeral != tc.ephemeral || cfg.LogFormat != tc.logFormat || cfg.ConsoleEnabled() != tc.console {
				t.Errorf("Ephemeral %v, LogFormat %q, console %v; want %v, %q, %v", cfg.Ephemeral, cfg.LogFormat, cfg.ConsoleEnabled(), tc.ephemeral, tc.logFormat, tc.console)
			}
		})
	}
}
