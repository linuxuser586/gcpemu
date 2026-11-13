//go:build darwin

package trust

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// systemRootsKeychain holds the roots Apple ships.
const systemRootsKeychain = "/System/Library/Keychains/SystemRootCertificates.keychain"

// hostRoots returns the roots macOS trusts for TLS, from the keychain, or
// /etc/ssl/cert.pem when the keychain cannot be read.
func hostRoots() ([]byte, Source) {
	b, err := keychainRoots()
	if err == nil && len(b) == 0 {
		err = errors.New("the keychain lists no trusted roots")
	}
	if err == nil {
		return b, Source{Where: "the macOS keychain"}
	}
	f, where := fileRoots()
	return f, Source{Where: where, FallbackErr: err}
}

// keychainRoots reads the system roots, the keychain search list and the
// admin and user trust settings with the security tool. None needs root.
func keychainRoots() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	system, err := security(ctx, "find-certificate", "-a", "-p", systemRootsKeychain)
	if err != nil {
		return nil, err
	}
	search, err := security(ctx, "find-certificate", "-a", "-p")
	if err != nil && !notFound(err) {
		return nil, err
	}
	admin, err := exportTrustSettings(ctx, "-d")
	if err != nil {
		return nil, err
	}
	user, err := exportTrustSettings(ctx)
	if err != nil {
		return nil, err
	}
	return mergeKeychain(system, search, admin, user)
}

// exportTrustSettings returns a domain's trust settings plist (`-d` for
// admin, none for user), or nil when the domain has none.
func exportTrustSettings(ctx context.Context, domain ...string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "gcpemu-trust-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	f := filepath.Join(dir, "settings.plist")
	if _, err := security(ctx, append(append([]string{"trust-settings-export"}, domain...), f)...); err != nil {
		if notFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return os.ReadFile(f)
}

// security runs /usr/bin/security and returns its standard output.
func security(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "/usr/bin/security", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("security %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// notFound reports whether err is security finding nothing: no
// certificates in the search list, or no trust settings in a domain.
func notFound(err error) bool {
	s := err.Error()
	return strings.Contains(s, "could not be found") || strings.Contains(s, "No Trust Settings")
}
