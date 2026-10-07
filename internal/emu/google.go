package emu

import (
	"net"
	"strings"
)

// googleDomains are the domains of Google's APIs, sign-in and token
// services. The emulator never contacts them (NFR-SEC-002); anonymous
// pulls of public images (gcr.io, *.pkg.dev through the registry mirror)
// are the user's image references, not API calls.
var googleDomains = []string{"googleapis.com", "google.com", "googleusercontent.com", "gstatic.com"}

// IsGoogleHost reports whether host (optionally with a port) is one of
// Google's API, sign-in or token hosts.
func IsGoogleHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, d := range googleDomains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}
