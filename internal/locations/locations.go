// Package locations embeds the real Google Cloud regions and zones
// (FR-CORE-021) so every service validates locations the same way. It also
// records each region's auto-mode VPC subnet range (SRS 1.2, compute).
package locations

import (
	"hash/fnv"
	"sort"
	"strings"
)

// Region is one GCP region.
type Region struct {
	Name string
	// Description is the human-readable location ("Iowa").
	Description string
	// Zones are the zone suffixes ("a", "b", ...); see ZoneNames.
	zones string
	// AutoCIDR is the subnet range an auto-mode VPC network creates in the
	// region (e.g. 10.128.0.0/20 in us-central1).
	AutoCIDR string
}

// ZoneNames returns the region's zones ("us-central1-a", ...).
func (r Region) ZoneNames() []string {
	out := make([]string, 0, len(r.zones))
	for _, z := range r.zones {
		out = append(out, r.Name+"-"+string(z))
	}
	return out
}

// ID returns a stable numeric ID for the region (the compute API exposes one).
func (r Region) ID() uint64 { return stableID(r.Name, 1000) }

// ZoneID returns a stable numeric ID for a zone.
func ZoneID(zone string) uint64 { return stableID(zone, 2000) }

func stableID(s string, base uint64) uint64 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return base + uint64(h.Sum32()%8000)
}

// regions is the embedded catalogue: name, description, zone suffixes and
// auto-mode range, as published in the GCP documentation.
var regions = []Region{
	{"africa-south1", "Johannesburg", "abc", "10.218.0.0/20"},
	{"asia-east1", "Changhua County, Taiwan", "abc", "10.140.0.0/20"},
	{"asia-east2", "Hong Kong", "abc", "10.170.0.0/20"},
	{"asia-northeast1", "Tokyo", "abc", "10.146.0.0/20"},
	{"asia-northeast2", "Osaka", "abc", "10.174.0.0/20"},
	{"asia-northeast3", "Seoul", "abc", "10.178.0.0/20"},
	{"asia-south1", "Mumbai", "abc", "10.160.0.0/20"},
	{"asia-south2", "Delhi", "abc", "10.190.0.0/20"},
	{"asia-southeast1", "Jurong West, Singapore", "abc", "10.148.0.0/20"},
	{"asia-southeast2", "Jakarta", "abc", "10.184.0.0/20"},
	{"asia-southeast3", "Bangkok", "abc", "10.230.0.0/20"},
	{"australia-southeast1", "Sydney", "abc", "10.152.0.0/20"},
	{"australia-southeast2", "Melbourne", "abc", "10.192.0.0/20"},
	{"europe-central2", "Warsaw", "abc", "10.186.0.0/20"},
	{"europe-north1", "Hamina, Finland", "abc", "10.166.0.0/20"},
	{"europe-north2", "Stockholm", "abc", "10.226.0.0/20"},
	{"europe-southwest1", "Madrid", "abc", "10.204.0.0/20"},
	{"europe-west1", "St. Ghislain, Belgium", "bcd", "10.132.0.0/20"},
	{"europe-west10", "Berlin", "abc", "10.214.0.0/20"},
	{"europe-west12", "Turin", "abc", "10.210.0.0/20"},
	{"europe-west2", "London", "abc", "10.154.0.0/20"},
	{"europe-west3", "Frankfurt", "abc", "10.156.0.0/20"},
	{"europe-west4", "Eemshaven, Netherlands", "abc", "10.164.0.0/20"},
	{"europe-west6", "Zurich", "abc", "10.172.0.0/20"},
	{"europe-west8", "Milan", "abc", "10.198.0.0/20"},
	{"europe-west9", "Paris", "abc", "10.200.0.0/20"},
	{"me-central1", "Doha", "abc", "10.212.0.0/20"},
	{"me-central2", "Dammam", "abc", "10.216.0.0/20"},
	{"me-west1", "Tel Aviv", "abc", "10.208.0.0/20"},
	{"northamerica-northeast1", "Montréal", "abc", "10.162.0.0/20"},
	{"northamerica-northeast2", "Toronto", "abc", "10.188.0.0/20"},
	{"northamerica-south1", "Querétaro, Mexico", "abc", "10.224.0.0/20"},
	{"southamerica-east1", "Osasco, São Paulo", "abc", "10.158.0.0/20"},
	{"southamerica-west1", "Santiago", "abc", "10.194.0.0/20"},
	{"us-central1", "Council Bluffs, Iowa", "abcf", "10.128.0.0/20"},
	{"us-east1", "Moncks Corner, South Carolina", "bcd", "10.142.0.0/20"},
	{"us-east4", "Ashburn, Virginia", "abc", "10.150.0.0/20"},
	{"us-east5", "Columbus, Ohio", "abc", "10.202.0.0/20"},
	{"us-south1", "Dallas", "abc", "10.206.0.0/20"},
	{"us-west1", "The Dalles, Oregon", "abc", "10.138.0.0/20"},
	{"us-west2", "Los Angeles", "abc", "10.168.0.0/20"},
	{"us-west3", "Salt Lake City", "abc", "10.180.0.0/20"},
	{"us-west4", "Las Vegas", "abc", "10.182.0.0/20"},
}

var (
	byName = map[string]Region{}
	zoneOf = map[string]string{}
)

func init() {
	sort.Slice(regions, func(i, j int) bool { return regions[i].Name < regions[j].Name })
	for _, r := range regions {
		byName[r.Name] = r
		for _, z := range r.ZoneNames() {
			zoneOf[z] = r.Name
		}
	}
}

// Regions returns every region sorted by name.
func Regions() []Region { return append([]Region(nil), regions...) }

// Lookup returns a region by name.
func Lookup(name string) (Region, bool) {
	r, ok := byName[name]
	return r, ok
}

// IsRegion reports whether name is a real region.
func IsRegion(name string) bool { _, ok := byName[name]; return ok }

// IsZone reports whether name is a real zone.
func IsZone(name string) bool { _, ok := zoneOf[name]; return ok }

// ZoneRegion returns the region of a zone.
func ZoneRegion(zone string) (string, bool) {
	r, ok := zoneOf[zone]
	return r, ok
}

// Zones returns every zone sorted by name.
func Zones() []string {
	out := make([]string, 0, len(zoneOf))
	for z := range zoneOf {
		out = append(out, z)
	}
	sort.Strings(out)
	return out
}

// IsLocation reports whether loc is a region or a zone.
func IsLocation(loc string) bool { return IsRegion(loc) || IsZone(loc) }

// RegionOf returns the region of a region or zone name.
func RegionOf(loc string) (string, bool) {
	if IsRegion(loc) {
		return loc, true
	}
	if r, ok := zoneOf[loc]; ok {
		return r, true
	}
	// Tolerate unknown zone suffixes of known regions for error messages.
	if i := strings.LastIndex(loc, "-"); i > 0 && IsRegion(loc[:i]) {
		return loc[:i], false
	}
	return "", false
}
