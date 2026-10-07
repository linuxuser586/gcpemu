package compute

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/reqlog"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// NAT gateway → emulator event channel. The in-container agent posts
// connection events (NAT translations seen by conntrack, and --offline
// sink attempts) to POST /compute/v1/_gcpemu/natEvents through the
// services network; requests carry a per-process token. Translations are
// logged in Cloud Logging JSON shape when the NAT's logConfig asks for
// them (FR-NAT-004); sink attempts are always logged and recorded in the
// store namespace "compute/natsink", visible through the admin API
// (/_emu/v1/resources?namespace=compute/natsink) (FR-NAT-005).

const (
	nsNatSink     = "compute/natsink"
	natEventsPath = "/compute/v1/_gcpemu/natEvents"
	natTokenHdr   = "X-Gcpemu-Nat-Token"
)

// natEvent is one connection event from the gateway agent.
type natEvent struct {
	Type    string `json:"type"` // "translation" or "sink"
	Time    string `json:"time"`
	Proto   string `json:"proto"`
	Src     string `json:"src"`
	SrcPort int    `json:"sport"`
	Dst     string `json:"dst"`
	DstPort int    `json:"dport"`
	NatIP   string `json:"natIP,omitempty"`
	NatPort int    `json:"natPort,omitempty"`
	// Bytes is the request prefix the sink received (sink events).
	Request string `json:"request,omitempty"`
}

type natReport struct {
	Network string     `json:"network"`
	Events  []natEvent `json:"events"`
}

// sinkServer holds the shared secret of the event channel.
type sinkServer struct {
	token string
	mu    sync.Mutex
	seq   uint64
}

func (s *sinkServer) close() {}

func (s *Service) sinkState() *sinkServer { return s.sink }

func (s *Service) sinkToken() string { return s.sinkState().token }

// sinkReportURL is the event URL as reachable from containers.
func (s *Service) sinkReportURL(ctx context.Context) (string, error) {
	plane, err := s.env.Containers.Netplane(ctx)
	if err != nil {
		return "", err
	}
	addr, err := plane.Addr(ctx, "gateway")
	if err != nil {
		return "", err
	}
	return "http://" + addr + natEventsPath, nil
}

// natEvents receives agent reports.
func (s *Service) natEvents(w http.ResponseWriter, r *http.Request) {
	tok := r.Header.Get(natTokenHdr)
	if subtle.ConstantTimeCompare([]byte(tok), []byte(s.sinkToken())) != 1 {
		apierr.Write(w, apierr.PermissionDenied("invalid NAT gateway token"))
		return
	}
	var rep natReport
	if _, err := decode(r, &rep); err != nil {
		apierr.Write(w, err)
		return
	}
	s.handleNatEvents(rep)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleNatEvents(rep natReport) {
	nets := s.netsOf(rep.Network)
	cov := s.natCoverage(rep.Network, nets)
	for _, ev := range rep.Events {
		var c *coverage
		for i := range cov {
			if inCIDR(cov[i].cidr, ev.Src) {
				c = &cov[i]
				break
			}
		}
		vm, subnet := s.ownerOfIP(nets, ev.Src)
		if ev.Type != "translation" || !inNets(nets, ev.Dst) {
			s.logEgress(rep.Network, ev, c)
		}
		switch ev.Type {
		case "sink":
			s.recordSink(rep.Network, ev, c, vm)
		case "translation":
			if c == nil || !c.logTrans || inNets(nets, ev.Dst) {
				continue
			}
			s.logNatFlow(rep.Network, ev, c, vm, subnet, "OK")
		case "dropped":
			if c == nil || !c.logErrors {
				continue
			}
			s.logNatFlow(rep.Network, ev, c, vm, subnet, "DROPPED")
		}
	}
}

// logEgress adds an internet egress attempt to the request log
// (FR-CORE-061): the NAT gateway that served it, or the network when no
// NAT covers the source.
func (s *Service) logEgress(network string, ev natEvent, c *coverage) {
	if ev.Type == "translation" && c == nil {
		return
	}
	e := reqlog.Entry{Service: "nat", Protocol: ev.Proto, Resource: network, Code: "OK",
		Method: fmt.Sprintf("EGRESS %s:%d -> %s:%d", ev.Src, ev.SrcPort, ev.Dst, ev.DstPort)}
	if t, err := time.Parse(time.RFC3339Nano, ev.Time); err == nil {
		e.Time = t
	}
	if c != nil {
		e.Resource = c.router + "/nats/" + c.nat
	}
	switch ev.Type {
	case "sink":
		e.Code = "OFFLINE_SINK"
	case "dropped":
		e.Status, e.Code = 1, "DROPPED"
	}
	s.env.RequestLog.Add(e)
}

// inNets reports whether ip is inside one of the VPC's container networks.
func inNets(nets []*vpcNet, ip string) bool {
	for _, st := range nets {
		if inCIDR(st.Subnet, ip) {
			return true
		}
	}
	return false
}

func inCIDR(c, ip string) bool {
	cc, err := parseCIDR(c, false)
	if err != nil {
		return false
	}
	v, ok := parseIP(ip)
	return ok && cc.contains(v)
}

// ownerOfIP finds the workload holding ip on one of the VPC's networks.
func (s *Service) ownerOfIP(nets []*vpcNet, ip string) (owner, subnet string) {
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, st := range nets {
			if !inCIDR(st.Subnet, ip) {
				continue
			}
			subnet = st.Subnetwork
			if b, ok := tx.Get(nsIPAlloc, st.Name+"/ip/"+ip); ok {
				owner = string(b)
			}
			return nil
		}
		return nil
	})
	return owner, subnet
}

// logNatFlow writes a Cloud NAT log entry (compute.googleapis.com/nat_flows).
func (s *Service) logNatFlow(np string, ev natEvent, c *coverage, vm, subnet, status string) {
	p, _, _ := pathParts(np)
	natIP := ev.NatIP
	_ = s.env.Store.View(func(tx store.Tx) error {
		if rt, ok := get[computev1.Router](tx, nsRouters, c.router); ok {
			for _, nat := range rt.Nats {
				if nat.Name == c.nat {
					if ips := natIPs(tx, c.router, nat); len(ips) > 0 {
						natIP = ips[0]
					}
				}
			}
		}
		return nil
	})
	payload := map[string]any{
		"connection": map[string]any{
			"src_ip": ev.Src, "src_port": ev.SrcPort, "dest_ip": ev.Dst, "dest_port": ev.DstPort,
			"protocol": protoNumber(ev.Proto), "nat_ip": natIP, "nat_port": ev.NatPort,
		},
		"allocation_status": status,
		"gateway_identifiers": map[string]any{
			"gateway_name": c.nat, "router_name": lastSeg(c.router), "region": c.region,
		},
		"endpoint": map[string]any{
			"project_id": p, "vm_name": lastSeg(vm), "region": c.region,
		},
		"vpc": map[string]any{
			"project_id": p, "vpc_name": lastSeg(np), "subnetwork_name": lastSeg(subnet),
		},
		"destination": map[string]any{},
	}
	s.env.Log.Info("nat flow",
		"logName", "projects/"+p+"/logs/compute.googleapis.com%2Fnat_flows",
		"resource", map[string]any{"type": "nat_gateway", "labels": map[string]any{
			"region": c.region, "router_id": lastSeg(c.router), "gateway_name": c.nat, "project_id": p,
		}},
		"severity", "INFO",
		"timestamp", ev.Time,
		"jsonPayload", payload,
	)
}

func protoNumber(p string) int {
	switch strings.ToLower(p) {
	case "tcp":
		return 6
	case "udp":
		return 17
	case "icmp":
		return 1
	}
	return 0
}

// recordSink stores and logs an --offline egress attempt (FR-NAT-005).
func (s *Service) recordSink(np string, ev natEvent, c *coverage, vm string) {
	st := s.sinkState()
	st.mu.Lock()
	st.seq++
	seq := st.seq
	st.mu.Unlock()
	rec := map[string]any{
		"time": ev.Time, "network": np, "proto": ev.Proto,
		"source": fmt.Sprintf("%s:%d", ev.Src, ev.SrcPort), "destination": fmt.Sprintf("%s:%d", ev.Dst, ev.DstPort),
		"vm": vm, "request": ev.Request,
	}
	if c != nil {
		rec["router"], rec["nat"] = c.router, c.nat
	}
	key := fmt.Sprintf("%s-%06d", stamp(s.env.Clock.Now()), seq)
	_ = s.env.Store.Update(func(tx store.Tx) error { return store.PutJSON(tx, nsNatSink, key, rec) })
	b, _ := json.Marshal(rec)
	s.env.Log.Info("nat sink: offline egress attempt recorded", "attempt", json.RawMessage(b))
}
