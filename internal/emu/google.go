package emu

import (
	"net"
	"strings"
)

// googleDomains are the domains of Google's APIs, sign-in and token
// services. The emulator never contacts them (NFR-SEC-002); anonymous
// registry pulls of Managed container images (gcr.io, *.pkg.dev through the
// registry mirror), the user's image references and the emulator's own
// alike, such as its Managed emulators' (ADR 0029), are not API calls. The one exception is
// the Google front end's --gfe-bind listeners, which read buckets the
// Instance doesn't have through from real GCS for callers that carry none
// of its credentials (ADR 0021).
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
