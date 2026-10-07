package gke

import (
	"sort"
	"strings"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// GKE locations (FR-CORE-021): every compute region and its zones. A
// cluster's location is a zone (zonal cluster) or a region (regional
// cluster, nodes spread over the region's default three zones).
var regionZones = map[string]string{
	"africa-south1":           "abc",
	"asia-east1":              "abc",
	"asia-east2":              "abc",
	"asia-northeast1":         "abc",
	"asia-northeast2":         "abc",
	"asia-northeast3":         "abc",
	"asia-south1":             "abc",
	"asia-south2":             "abc",
	"asia-southeast1":         "abc",
	"asia-southeast2":         "abc",
	"asia-southeast3":         "abc",
	"australia-southeast1":    "abc",
	"australia-southeast2":    "abc",
	"europe-central2":         "abc",
	"europe-north1":           "abc",
	"europe-north2":           "abc",
	"europe-southwest1":       "abc",
	"europe-west1":            "bcd",
	"europe-west2":            "abc",
	"europe-west3":            "abc",
	"europe-west4":            "abc",
	"europe-west6":            "abc",
	"europe-west8":            "abc",
	"europe-west9":            "abc",
	"europe-west10":           "abc",
	"europe-west12":           "abc",
	"me-central1":             "abc",
	"me-central2":             "abc",
	"me-west1":                "abc",
	"northamerica-northeast1": "abc",
	"northamerica-northeast2": "abc",
	"northamerica-south1":     "abc",
	"southamerica-east1":      "abc",
	"southamerica-west1":      "abc",
	"us-central1":             "abcf",
	"us-east1":                "bcd",
	"us-east4":                "abc",
	"us-east5":                "abc",
	"us-south1":               "abc",
	"us-west1":                "abc",
	"us-west2":                "abc",
	"us-west3":                "abc",
	"us-west4":                "abc",
}

// isZone reports whether loc is a known zone.
func isZone(loc string) bool {
	i := strings.LastIndexByte(loc, '-')
	if i < 0 || len(loc)-i != 2 {
		return false
	}
	zs, ok := regionZones[loc[:i]]
	return ok && strings.IndexByte(zs, loc[i+1]) >= 0
}

// isRegion reports whether loc is a known region.
func isRegion(loc string) bool {
	_, ok := regionZones[loc]
	return ok
}

// regionOf returns the region of a zone or region.
func regionOf(loc string) string {
	if isZone(loc) {
		return loc[:strings.LastIndexByte(loc, '-')]
	}
	return loc
}

// zonesOf returns every zone of a region, sorted.
func zonesOf(region string) []string {
	var out []string
	for _, c := range regionZones[region] {
		out = append(out, region+"-"+string(c))
	}
	sort.Strings(out)
	return out
}

// defaultZones returns the zones a regional cluster uses by default (the
// first three of the region).
func defaultZones(region string) []string {
	zs := zonesOf(region)
	if len(zs) > 3 {
		zs = zs[:3]
	}
	return zs
}

// validateLocation returns GKE's error for an unknown location.
func validateLocation(loc string) error {
	if isZone(loc) || isRegion(loc) {
		return nil
	}
	return apierr.NotFound("Location %q does not exist.", loc)
}
