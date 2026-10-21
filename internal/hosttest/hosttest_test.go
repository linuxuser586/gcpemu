package hosttest

import (
	"maps"
	"testing"
)

func TestParseEnv(t *testing.T) {
	want := map[string]string{"GCPEMU_GATEWAY": "127.0.0.1:4510", "QUOTED": "it's", "EMPTY": ""}
	for name, out := range map[string]string{
		"bash":       "export EMPTY=''\nexport GCPEMU_GATEWAY='127.0.0.1:4510'\nexport QUOTED='it'\\''s'\n",
		"powershell": "$env:EMPTY = ''\r\n$env:GCPEMU_GATEWAY = '127.0.0.1:4510'\r\n$env:QUOTED = 'it''s'\r\n",
	} {
		if got := ParseEnv(out + "# gcpemu: a hint\n"); !maps.Equal(got, want) {
			t.Errorf("%s: ParseEnv = %v, want %v", name, got, want)
		}
	}
}
