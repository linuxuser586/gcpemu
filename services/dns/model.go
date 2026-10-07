package dns

import (
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	dnsv1 "google.golang.org/api/dns/v1"
)

// Store namespaces.
const (
	nsZones   = "dns/zones"   // project/zone → zoneRec
	nsRRSets  = "dns/rrsets"  // project/zone/name/TYPE → dnsv1.ResourceRecordSet
	nsChanges = "dns/changes" // project/zone/%012d → changeRec
	nsOps     = "dns/ops"     // project/zone/%012d → dnsv1.Operation
	nsMeta    = "dns/meta"    // "gen" → generation token of the last commit
)

// Resource kinds as returned by Cloud DNS.
const (
	kindZone       = "dns#managedZone"
	kindZoneList   = "dns#managedZonesListResponse"
	kindRRSet      = "dns#resourceRecordSet"
	kindRRSetList  = "dns#resourceRecordSetsListResponse"
	kindChange     = "dns#change"
	kindChangeList = "dns#changesListResponse"
	kindOp         = "dns#operation"
	kindOpList     = "dns#managedZoneOperationsListResponse"
	kindPVC        = "dns#managedZonePrivateVisibilityConfig"
	kindPVCNet     = "dns#managedZonePrivateVisibilityConfigNetwork"
	kindLogging    = "dns#managedZoneCloudLoggingConfig"
	kindProject    = "dns#project"
	kindQuota      = "dns#quota"
)

// Default SOA/NS parameters of a new zone (as Cloud DNS creates them).
const (
	defaultApexTTL   = 21600
	hostmaster       = "cloud-dns-hostmaster.google.com."
	privateNS        = "ns-gcp-private.googledomains.com."
	soaTimers        = "21600 3600 259200 300"
	visibilityPublic = "public"
	visibilityPriv   = "private"
)

// zoneRec is the stored form of a managed zone.
type zoneRec struct {
	Project    string             `json:"project"`
	Zone       *dnsv1.ManagedZone `json:"zone"`
	NextChange int64              `json:"nextChange"`
	NextOp     int64              `json:"nextOp"`
}

// changeRec is a stored change; Status is derived from DoneAt at read time
// so a configured LRO latency (FR-CORE-023) shows "pending" until elapsed.
type changeRec struct {
	Change *dnsv1.Change `json:"change"`
	DoneAt time.Time     `json:"doneAt"`
}

func zoneKey(project, zone string) string { return project + "/" + zone }

func rrsetKey(zk, name, typ string) string {
	return zk + "/" + strings.ToLower(name) + "/" + strings.ToUpper(typ)
}

func seqKey(zk string, n int64) string { return fmt.Sprintf("%s/%012d", zk, n) }

func zoneResource(project, zone string) string {
	return "//dns.googleapis.com/projects/" + project + "/managedZones/" + zone
}

func projectResource(project string) string {
	return "//cloudresourcemanager.googleapis.com/projects/" + project
}

// rfc3339 formats t the way Cloud DNS does (millisecond precision, UTC).
func rfc3339(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z07:00") }

// nameServers returns the delegation set for a public zone. Cloud DNS uses
// one of the ns-cloud-{a..e}{1..4}.googledomains.com. sets.
func nameServers(project, zone string) []string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(project + "/" + zone))
	letter := string(rune('a' + h.Sum32()%5))
	out := make([]string, 4)
	for i := range out {
		out[i] = fmt.Sprintf("ns-cloud-%s%d.googledomains.com.", letter, i+1)
	}
	return out
}

// apexRecords returns the automatic SOA and NS record sets of a new zone.
func apexRecords(z *dnsv1.ManagedZone) []*dnsv1.ResourceRecordSet {
	ns := z.NameServers
	return []*dnsv1.ResourceRecordSet{
		{Kind: kindRRSet, Name: z.DnsName, Type: "NS", Ttl: defaultApexTTL, Rrdatas: append([]string(nil), ns...)},
		{Kind: kindRRSet, Name: z.DnsName, Type: "SOA", Ttl: defaultApexTTL,
			Rrdatas: []string{fmt.Sprintf("%s %s 1 %s", ns[0], hostmaster, soaTimers)}},
	}
}

// rrsetLabel names an rrset in error messages: "www.example.com. (A)".
func rrsetLabel(rs *dnsv1.ResourceRecordSet) string {
	return fmt.Sprintf("%s (%s)", rs.Name, rs.Type)
}
