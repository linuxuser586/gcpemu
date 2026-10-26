package instance

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/loopback"
)

// TestWarnLoopbackNetmask: a macOS Instance with an alias other than a /32
// logs one warning naming the fix (#677); /32 aliases, none, or another OS
// log nothing.
func TestWarnLoopbackNetmask(t *testing.T) {
	for _, tc := range []struct {
		goos    string
		aliases loopback.AliasState
		warn    bool
	}{
		{"darwin", loopback.AliasesWide, true},
		{"darwin", loopback.AliasesNarrow, false},
		{"darwin", loopback.AliasesMissing, false},
		{"linux", loopback.AliasesWide, false},
		{"windows", loopback.AliasesWide, false},
	} {
		var b bytes.Buffer
		warnLoopbackNetmask(slog.New(slog.NewTextHandler(&b, nil)), tc.goos, func() loopback.AliasState { return tc.aliases })
		got := b.String()
		if !tc.warn {
			if got != "" {
				t.Errorf("%s %v: logged %q, want nothing", tc.goos, tc.aliases, got)
			}
			continue
		}
		if strings.Count(got, "\n") != 1 || !strings.Contains(got, "level=WARN") ||
			!strings.Contains(got, "sudo gcpemu loopback install") || !strings.Contains(got, "mDNSResponder") {
			t.Errorf("%s %v: logged %q, want one warning naming the fix", tc.goos, tc.aliases, got)
		}
	}
}
