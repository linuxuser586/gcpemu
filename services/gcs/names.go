package gcs

import (
	"net"
	"strings"
	"unicode/utf8"
)

// validBucketName implements the GCS bucket naming rules
// (https://cloud.google.com/storage/docs/buckets#naming): lowercase
// letters, digits, '-', '_' and '.'; starts and ends with a letter or
// digit; 3–63 characters (up to 222 with dots, each component ≤ 63); not an
// IP address; no "goog" prefix and no "google" or close misspellings.
func validBucketName(name string) bool {
	n := len(name)
	if n < 3 || n > 222 {
		return false
	}
	if !strings.Contains(name, ".") && n > 63 {
		return false
	}
	for i := 0; i < n; i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.':
			if i == 0 || i == n-1 {
				return false
			}
		default:
			return false
		}
	}
	for _, comp := range strings.Split(name, ".") {
		if comp == "" || len(comp) > 63 {
			return false
		}
	}
	if net.ParseIP(name) != nil {
		return false
	}
	if strings.HasPrefix(name, "goog") {
		return false
	}
	for _, bad := range []string{"google", "g00gle", "go0gle", "g0ogle"} {
		if strings.Contains(name, bad) {
			return false
		}
	}
	return true
}

// validObjectName implements the object naming rules: 1–1024 bytes of
// UTF-8, no CR/LF/NUL, not "." or "..", and not under the ACME challenge path.
func validObjectName(name string) bool {
	if name == "" || len(name) > 1024 || !utf8.ValidString(name) {
		return false
	}
	if name == "." || name == ".." || strings.HasPrefix(name, ".well-known/acme-challenge/") {
		return false
	}
	return !strings.ContainsAny(name, "\r\n\x00")
}

// Locations accepted for buckets (FR-CORE-021), with their location type.
var multiRegions = map[string]bool{"US": true, "EU": true, "ASIA": true}

var dualRegions = map[string]bool{
	"NAM4": true, "EUR4": true, "ASIA1": true, "EUR5": true, "EUR7": true, "EUR8": true,
}

var regions = map[string]bool{}

func init() {
	for _, r := range []string{
		"africa-south1",
		"asia-east1", "asia-east2", "asia-northeast1", "asia-northeast2", "asia-northeast3",
		"asia-south1", "asia-south2", "asia-southeast1", "asia-southeast2",
		"australia-southeast1", "australia-southeast2",
		"europe-central2", "europe-north1", "europe-north2", "europe-southwest1",
		"europe-west1", "europe-west2", "europe-west3", "europe-west4", "europe-west6",
		"europe-west8", "europe-west9", "europe-west10", "europe-west12",
		"me-central1", "me-central2", "me-west1",
		"northamerica-northeast1", "northamerica-northeast2", "northamerica-south1",
		"southamerica-east1", "southamerica-west1",
		"us-central1", "us-east1", "us-east4", "us-east5", "us-south1",
		"us-west1", "us-west2", "us-west3", "us-west4",
	} {
		regions[strings.ToUpper(r)] = true
	}
}

// locationType returns the GCS locationType for an upper-cased location,
// or "" when the location is not valid.
func locationType(loc string, customPlacement bool) string {
	switch {
	case multiRegions[loc]:
		if customPlacement {
			return "dual-region"
		}
		return "multi-region"
	case dualRegions[loc]:
		return "dual-region"
	case regions[loc]:
		return "region"
	}
	return ""
}

// validStorageClass reports whether c is a GCS storage class.
func validStorageClass(c string) bool {
	switch c {
	case "STANDARD", "NEARLINE", "COLDLINE", "ARCHIVE", "MULTI_REGIONAL", "REGIONAL", "DURABLE_REDUCED_AVAILABILITY":
		return true
	}
	return false
}
