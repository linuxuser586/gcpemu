package cli

import (
	"bytes"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

// psValues are values that PowerShell must load unchanged (#267).
var psValues = map[string]string{
	"GCPEMU_PS_QUOTE":    "it's",
	"GCPEMU_PS_CURLY":    "C:\\Users\\O’Brien\\‘a‚b‛\\ca.pem",
	"GCPEMU_PS_PATH":     `C:\Users\me\AppData\Local\gcpemu\default\ca.pem`,
	"GCPEMU_PS_UNC":      `\\server\share\`,
	"GCPEMU_PS_DOLLAR":   "$HOME $(whoami) ${x}",
	"GCPEMU_PS_BACKTICK": "a`nb`",
	"GCPEMU_PS_SPACES":   "  two  spaces ; # \"dq\" ",
}

func TestWriteEnvPowerShell(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`plain`, `'plain'`},
		{`it's`, `'it''s'`},
		{"O’Brien", "'O’’Brien'"},
		{"‘‚‛", "'‘‘‚‚‛‛'"},
		{`C:\a\`, `'C:\a\'`},
		{"$x `y", "'$x `y'"},
	} {
		if got := psQuote(c.in); got != c.want {
			t.Errorf("psQuote(%q) = %s, want %s", c.in, got, c.want)
		}
	}
	for _, shell := range []string{"powershell", "pwsh"} {
		var b bytes.Buffer
		if err := writeEnv(&b, shell, map[string]string{"B": "2", "A": "it's"}); err != nil {
			t.Fatal(err)
		}
		if want := "$env:A = 'it''s'\n$env:B = '2'\n"; b.String() != want {
			t.Errorf("%s: got %q, want %q", shell, b.String(), want)
		}
	}
	err := writeEnv(&bytes.Buffer{}, "cmd", map[string]string{"A": "1"})
	if err == nil || !strings.Contains(err.Error(), "powershell") {
		t.Errorf("unknown shell error = %v, want it to list powershell", err)
	}
}

// TestPowerShellLoadsEnv loads writeEnv's output the way the README says,
// with Out-String | Invoke-Expression, in every PowerShell on PATH, and
// checks each variable comes back unchanged.
func TestPowerShellLoadsEnv(t *testing.T) {
	var shells []string
	for _, s := range []string{"powershell", "pwsh"} {
		if p, err := exec.LookPath(s); err == nil {
			shells = append(shells, p)
		}
	}
	if len(shells) == 0 {
		t.Skip("no PowerShell on PATH")
	}
	var out bytes.Buffer
	if err := writeEnv(&out, "powershell", psValues); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "env.ps1")
	if err := os.WriteFile(file, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	// Read the file as UTF-8, since 5.1 decodes a native program's piped
	// output in the OEM code page (a README note covers that), and print
	// each value as base64 so the console encoding can't change it.
	var script strings.Builder
	script.WriteString("$ErrorActionPreference = 'Stop'; $ProgressPreference = 'SilentlyContinue'\n")
	script.WriteString("Get-Content -Encoding UTF8 -LiteralPath '" + strings.ReplaceAll(file, "'", "''") + "' | Out-String | Invoke-Expression\n")
	keys := sortedKeys(psValues)
	for _, k := range keys {
		script.WriteString("[Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($env:" + k + "))\n")
	}
	for _, sh := range shells {
		t.Run(filepath.Base(sh), func(t *testing.T) {
			cmd := exec.Command(sh, "-NoProfile", "-NonInteractive", "-EncodedCommand", encodePS(script.String()))
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			b, err := cmd.Output()
			if err != nil {
				t.Fatalf("%v\n%s%s", err, b, stderr.Bytes())
			}
			lines := strings.Fields(string(b))
			if len(lines) != len(keys) {
				t.Fatalf("got %d lines, want %d:\n%s", len(lines), len(keys), b)
			}
			for i, k := range keys {
				v, err := base64.StdEncoding.DecodeString(lines[i])
				if err != nil {
					t.Fatalf("%s: %v", k, err)
				}
				if string(v) != psValues[k] {
					t.Errorf("%s = %q, want %q", k, v, psValues[k])
				}
			}
		})
	}
}

// encodePS encodes a script for -EncodedCommand: base64 of UTF-16LE.
func encodePS(s string) string {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		b[2*i], b[2*i+1] = byte(c), byte(c>>8)
	}
	return base64.StdEncoding.EncodeToString(b)
}
