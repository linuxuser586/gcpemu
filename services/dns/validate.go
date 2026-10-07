package dns

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	mdns "github.com/miekg/dns"
	dnsv1 "google.golang.org/api/dns/v1"
	"google.golang.org/grpc/codes"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// Errors in the shapes Cloud DNS returns (legacy errors[].reason values).

func errInvalid(field string, value any) *apierr.Error {
	return apierr.InvalidArgument("Invalid value for '%s': '%v'", field, value)
}

func errRequired(field string) *apierr.Error {
	return apierr.InvalidArgument("Required field '%s' not specified", field).WithLegacy("required")
}

func errNotFound(field, name string) *apierr.Error {
	return apierr.NotFound("The '%s' resource named '%s' does not exist.", field, name)
}

func errExists(field, name string) *apierr.Error {
	return apierr.AlreadyExists("The resource '%s' named '%s' already exists", field, name).WithLegacy("alreadyExists")
}

func errNotEmpty(name string) *apierr.Error {
	return apierr.New(codes.FailedPrecondition, "The resource named '%s' cannot be deleted because it is not empty", name).
		WithLegacy("containerNotEmpty").WithHTTP(http.StatusBadRequest)
}

func errConditionNotMet(field string) *apierr.Error {
	return apierr.New(codes.FailedPrecondition, "Precondition not met for '%s'", field).
		WithLegacy("conditionNotMet").WithHTTP(http.StatusPreconditionFailed)
}

var zoneNameRE = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)

// validZoneName reports whether s is a valid managed zone name: 1-63
// characters, lower-case letters, digits and dashes, starting with a letter.
func validZoneName(s string) bool { return zoneNameRE.MatchString(s) }

// validDNSName reports whether s is an absolute domain name (ending in '.').
func validDNSName(s string) bool {
	if s == "." || !strings.HasSuffix(s, ".") || strings.ContainsAny(s, " \t\r\n;\"()") {
		return false
	}
	_, ok := mdns.IsDomainName(s)
	return ok
}

// inZone reports whether name is the apex of or below origin.
func inZone(name, origin string) bool {
	name, origin = strings.ToLower(name), strings.ToLower(origin)
	return name == origin || strings.HasSuffix(name, "."+origin)
}

// supportedTypes are the record types the emulator accepts (FR-DNS-002).
var supportedTypes = map[string]bool{
	"A": true, "AAAA": true, "CNAME": true, "MX": true, "TXT": true, "SRV": true,
	"NS": true, "SOA": true, "CAA": true, "PTR": true, "SPF": true, "DS": true,
	"HTTPS": true, "SVCB": true, "TLSA": true, "NAPTR": true, "SSHFP": true,
}

// parseRR parses one rrdata of an rrset into a wire record.
func parseRR(name string, ttl int64, typ, rdata string) (mdns.RR, error) {
	if strings.ContainsAny(rdata, "\r\n") || strings.TrimSpace(rdata) == "" {
		return nil, fmt.Errorf("bad rdata")
	}
	rr, err := mdns.NewRR(fmt.Sprintf("%s %d IN %s %s", name, ttl, typ, rdata))
	if err != nil {
		return nil, err
	}
	if rr == nil || rr.Header().Rrtype != mdns.StringToType[typ] {
		return nil, fmt.Errorf("bad rdata")
	}
	return rr, nil
}

// validateRRSet checks an rrset against the zone origin; field names the
// rrset in errors (e.g. "entity.change.additions[0]").
func validateRRSet(rs *dnsv1.ResourceRecordSet, field, origin string) error {
	if rs == nil {
		return errRequired(field)
	}
	if rs.Name == "" {
		return errRequired(field + ".name")
	}
	if !validDNSName(rs.Name) || !inZone(rs.Name, origin) {
		return errInvalid(field+".name", rs.Name)
	}
	if rs.Type == "" {
		return errRequired(field + ".type")
	}
	if !supportedTypes[rs.Type] {
		return errInvalid(field+".type", rs.Type)
	}
	if rs.Ttl < 0 || rs.Ttl > 1<<31-1 {
		return errInvalid(field+".ttl", rs.Ttl)
	}
	apex := strings.EqualFold(rs.Name, origin)
	switch rs.Type {
	case "SOA":
		if !apex {
			return errInvalid(field+".name", rs.Name)
		}
	case "CNAME":
		if apex {
			return apierr.InvalidArgument("The resource '%s' named '%s' cannot be created because a CNAME record set is not permitted at the zone apex.", field, rrsetLabel(rs)).
				WithLegacy("cnameAtApex")
		}
	}
	if rs.RoutingPolicy != nil {
		if len(rs.Rrdatas) > 0 {
			return apierr.InvalidArgument("The resource '%s' cannot specify both rrdatas and routingPolicy.", field)
		}
		return validateRoutingPolicy(rs, field)
	}
	if len(rs.Rrdatas) == 0 {
		return errRequired(field + ".rrdata")
	}
	if (rs.Type == "CNAME" || rs.Type == "SOA") && len(rs.Rrdatas) != 1 {
		return errInvalid(field+".rrdata", strings.Join(rs.Rrdatas, ","))
	}
	return validateRrdatas(rs.Name, rs.Ttl, rs.Type, rs.Rrdatas, field+".rrdata")
}

func validateRrdatas(name string, ttl int64, typ string, rrdatas []string, field string) error {
	seen := map[string]bool{}
	for i, d := range rrdatas {
		f := fmt.Sprintf("%s[%d]", field, i)
		rr, err := parseRR(name, ttl, typ, d)
		if err != nil {
			return errInvalid(f, d)
		}
		k := strings.ToLower(rdataString(rr))
		if seen[k] {
			return apierr.InvalidArgument("The resource record set '%s' contains duplicate rrdata '%s'.", field, d).WithLegacy("duplicateResourceRecordData")
		}
		seen[k] = true
	}
	return nil
}

// rdataString returns the presentation form of an RR's rdata.
func rdataString(rr mdns.RR) string {
	s := rr.String()
	h := rr.Header().String()
	return strings.TrimPrefix(s, h)
}

// sameRRSet reports whether two rrsets are identical for deletion matching
// (Cloud DNS requires deletions to match the existing rrset exactly).
func sameRRSet(a, b *dnsv1.ResourceRecordSet) bool {
	if !strings.EqualFold(a.Name, b.Name) || a.Type != b.Type || a.Ttl != b.Ttl {
		return false
	}
	if (a.RoutingPolicy == nil) != (b.RoutingPolicy == nil) {
		return false
	}
	if a.RoutingPolicy != nil {
		ja, _ := a.RoutingPolicy.MarshalJSON()
		jb, _ := b.RoutingPolicy.MarshalJSON()
		return string(ja) == string(jb)
	}
	return sameRrdatas(a.Name, a.Type, a.Rrdatas, b.Rrdatas)
}

// sameRrdatas compares rrdata sets order-insensitively after normalising
// their presentation form.
func sameRrdatas(name, typ string, x, y []string) bool {
	if len(x) != len(y) {
		return false
	}
	norm := func(in []string) map[string]int {
		m := map[string]int{}
		for _, d := range in {
			if rr, err := parseRR(name, 0, typ, d); err == nil {
				m[strings.ToLower(rdataString(rr))]++
			} else {
				m[d]++
			}
		}
		return m
	}
	mx, my := norm(x), norm(y)
	if len(mx) != len(my) {
		return false
	}
	for k, v := range mx {
		if my[k] != v {
			return false
		}
	}
	return true
}
