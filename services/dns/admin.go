package dns

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	mdns "github.com/miekg/dns"

	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/frontend"
)

// The Web console's Cloud DNS view (SRS 4.8.3) has a query box, and a
// browser cannot speak DNS, so the admin API asks the Instance's DNS server
// on its behalf:
//
//	GET /_emu/v1/dns/query?name=N[&type=T][&network=projects/P/global/networks/V]
//
// The query goes over the wire to the data plane, as dig's would, so it is
// answered (and logged) exactly as a client's is. With network it carries
// the VPC network in the EDNS0 option a VPC's DNS relay adds, and private
// zones bound to that network are visible.

// queryRR is one resource record of a query's response.
type queryRR struct {
	Name string `json:"name"`
	Type string `json:"type"`
	TTL  uint32 `json:"ttl"`
	Data string `json:"data"`
}

// queryResult is the response to a console query.
type queryResult struct {
	Server             string    `json:"server"`
	Name               string    `json:"name"`
	Type               string    `json:"type"`
	Network            string    `json:"network,omitempty"`
	Rcode              string    `json:"rcode"`
	Authoritative      bool      `json:"authoritative"`
	RecursionAvailable bool      `json:"recursionAvailable"`
	Zone               string    `json:"zone,omitempty"`
	Answer             []queryRR `json:"answer"`
	Authority          []queryRR `json:"authority"`
	Additional         []queryRR `json:"additional"`
	LatencyMS          float64   `json:"latencyMs"`
}

func adminError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// serveQuery is GET /_emu/v1/dns/query.
func (s *Service) serveQuery(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := strings.TrimSpace(q.Get("name"))
	if name == "" {
		adminError(w, http.StatusBadRequest, "name is required")
		return
	}
	name = mdns.Fqdn(name)
	if _, ok := mdns.IsDomainName(name); !ok {
		adminError(w, http.StatusBadRequest, "name "+name+" is not a domain name")
		return
	}
	typ := strings.ToUpper(strings.TrimSpace(q.Get("type")))
	if typ == "" {
		typ = "A"
	}
	qtype, ok := mdns.StringToType[typ]
	if !ok {
		adminError(w, http.StatusBadRequest, "type "+typ+" is not a DNS record type")
		return
	}
	network := ""
	if v := strings.TrimSpace(q.Get("network")); v != "" {
		if network = normalizeNetwork(v); network == "" {
			adminError(w, http.StatusBadRequest, "network must be a VPC network, e.g. projects/P/global/networks/N")
			return
		}
	}
	server := s.Addr()
	if server == "" {
		adminError(w, http.StatusServiceUnavailable, "the DNS server is not running")
		return
	}
	if host, port, err := net.SplitHostPort(server); err == nil {
		if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
			server = net.JoinHostPort("127.0.0.1", port)
		}
	}

	m := new(mdns.Msg)
	m.SetQuestion(name, qtype)
	m.RecursionDesired = true
	m.SetEdns0(4096, false)
	if network != "" {
		m = frontend.SetNetwork(m, network)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	timer := clock.StartStopwatch()
	resp, err := exchange(ctx, m, server)
	if err != nil {
		adminError(w, http.StatusBadGateway, "query "+server+": "+err.Error())
		return
	}
	out := queryResult{
		Server: server, Name: name, Type: typ, Network: network,
		Rcode:              mdns.RcodeToString[resp.Rcode],
		Authoritative:      resp.Authoritative,
		RecursionAvailable: resp.RecursionAvailable,
		Answer:             queryRRs(resp.Answer),
		Authority:          queryRRs(resp.Ns),
		Additional:         queryRRs(resp.Extra),
		LatencyMS:          float64(timer.Elapsed().Microseconds()) / 1000,
	}
	if z := s.currentIndex().findZone(strings.ToLower(name), network); z != nil {
		out.Zone = "projects/" + z.project + "/managedZones/" + z.name
	}
	writeJSON(w, http.StatusOK, out)
}

// queryRRs lists a response section's records, leaving out the OPT
// pseudo-record.
func queryRRs(in []mdns.RR) []queryRR {
	out := []queryRR{}
	for _, rr := range in {
		h := rr.Header()
		if h.Rrtype == mdns.TypeOPT {
			continue
		}
		out = append(out, queryRR{Name: h.Name, Type: mdns.TypeToString[h.Rrtype], TTL: h.Ttl,
			Data: strings.TrimSpace(rdataString(rr))})
	}
	return out
}
