package compute

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/reqlog"
	"github.com/linuxuser586/gcpemu/internal/store"
)

const (
	tNet    = "projects/p/global/networks/vpc"
	tRouter = "projects/p/regions/us-central1/routers/router"
)

// natTestService returns a service whose VPC has a us-central1 subnetwork
// (10.0.0.0/24, covered by a NAT with logConfig) and a europe-west1 one
// (10.0.1.0/24, not covered), and the buffer its JSON log goes to.
func natTestService(t *testing.T, logCfg *computev1.RouterNatLogConfig) (*Service, *bytes.Buffer) {
	t.Helper()
	st := store.NewMemory()
	buf := &bytes.Buffer{}
	s := &Service{
		env: &emu.Env{
			Config:     &config.Config{},
			Store:      st,
			Clock:      clock.Real{},
			Log:        slog.New(slog.NewJSONHandler(buf, nil)),
			RequestLog: reqlog.New(100, slog.New(slog.DiscardHandler)),
		},
		sink: &sinkServer{},
	}
	err := st.Update(func(tx store.Tx) error {
		for _, sn := range []struct{ name, region, cidr string }{
			{"nodes", "us-central1", "10.0.0.0/24"},
			{"eu", "europe-west1", "10.0.1.0/24"},
		} {
			path := "projects/p/regions/" + sn.region + "/subnetworks/" + sn.name
			if err := store.PutJSON(tx, nsSubnets, path, &computev1.Subnetwork{
				Name: sn.name, IpCidrRange: sn.cidr, Network: link(tNet), SelfLink: link(path),
			}); err != nil {
				return err
			}
			if err := store.PutJSON(tx, nsVPCNets, sn.name, &vpcNet{
				Name: "gcpemu-" + sn.name, Subnet: sn.cidr, Range: sn.cidr, Network: tNet, Region: sn.region, Subnetwork: path,
			}); err != nil {
				return err
			}
		}
		if err := store.PutJSON(tx, nsRouters, tRouter, &computev1.Router{
			Name: "router", Network: link(tNet), SelfLink: link(tRouter),
			Nats: []*computev1.RouterNat{{
				Name: "nat", NatIpAllocateOption: "AUTO_ONLY", SourceSubnetworkIpRangesToNat: "ALL_SUBNETWORKS_ALL_IP_RANGES",
				LogConfig: logCfg,
			}},
		}); err != nil {
			return err
		}
		if err := store.PutJSON(tx, nsNatIPs, tRouter+"/nat", []string{"203.0.113.5"}); err != nil {
			return err
		}
		return tx.Put(nsIPAlloc, "gcpemu-nodes/ip/10.0.0.2", []byte("projects/p/zones/us-central1-a/instances/vm-1"))
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, buf
}

// natFlows returns the Cloud NAT log entries written to buf.
func natFlows(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
	for sc.Scan() {
		var e map[string]any
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("%v: %s", err, sc.Text())
		}
		if e["logName"] == "projects/p/logs/compute.googleapis.com%2Fnat_flows" {
			out = append(out, e)
		}
	}
	return out
}

// natTestEvents: a covered translation and drop, an uncovered drop and
// an intra-VPC flow.
var natTestEvents = []natEvent{
	{Type: "translation", Time: "2026-10-08T00:00:00Z", Proto: "tcp", Src: "10.0.0.2", SrcPort: 40000, Dst: "1.1.1.1", DstPort: 443, NatIP: "172.22.0.2", NatPort: 40001},
	{Type: "dropped", Time: "2026-10-08T00:00:01Z", Proto: "tcp", Src: "10.0.0.2", SrcPort: 40002, Dst: "1.1.1.1", DstPort: 443},
	{Type: "dropped", Time: "2026-10-08T00:00:02Z", Proto: "udp", Src: "10.0.1.2", SrcPort: 5353, Dst: "8.8.8.8", DstPort: 53},
	{Type: "translation", Time: "2026-10-08T00:00:03Z", Proto: "tcp", Src: "10.0.0.2", SrcPort: 40004, Dst: "10.0.1.2", DstPort: 80, NatIP: "10.0.1.254", NatPort: 40004},
}

// TestNatFlowLogFilters is FR-NAT-004: logConfig.filter selects
// translations, drops or both, written in Cloud Logging JSON shape.
func TestNatFlowLogFilters(t *testing.T) {
	for _, c := range []struct {
		cfg  *computev1.RouterNatLogConfig
		want []string
	}{
		{&computev1.RouterNatLogConfig{Enable: true, Filter: "ALL"}, []string{"OK", "DROPPED"}},
		{&computev1.RouterNatLogConfig{Enable: true, Filter: "TRANSLATIONS_ONLY"}, []string{"OK"}},
		{&computev1.RouterNatLogConfig{Enable: true, Filter: "ERRORS_ONLY"}, []string{"DROPPED"}},
		{&computev1.RouterNatLogConfig{Enable: false, Filter: "ALL"}, nil},
		{nil, nil},
	} {
		name := "nil"
		if c.cfg != nil {
			name = c.cfg.Filter
			if !c.cfg.Enable {
				name += "-disabled"
			}
		}
		t.Run(name, func(t *testing.T) {
			s, buf := natTestService(t, c.cfg)
			s.handleNatEvents(natReport{Network: tNet, Events: natTestEvents})
			flows := natFlows(t, buf)
			if len(flows) != len(c.want) {
				t.Fatalf("got %d nat_flows entries, want %v:\n%s", len(flows), c.want, buf)
			}
			for i, e := range flows {
				checkNatFlow(t, e, c.want[i])
			}
		})
	}
}

func checkNatFlow(t *testing.T, e map[string]any, status string) {
	t.Helper()
	res := e["resource"].(map[string]any)
	labels := res["labels"].(map[string]any)
	if res["type"] != "nat_gateway" || labels["gateway_name"] != "nat" || labels["router_id"] != "router" ||
		labels["region"] != "us-central1" || labels["project_id"] != "p" {
		t.Errorf("resource: %v", res)
	}
	p := e["jsonPayload"].(map[string]any)
	if p["allocation_status"] != status {
		t.Errorf("allocation_status %v, want %s", p["allocation_status"], status)
	}
	conn := p["connection"].(map[string]any)
	if conn["src_ip"] != "10.0.0.2" || conn["dest_ip"] != "1.1.1.1" || conn["dest_port"] != 443.0 || conn["protocol"] != 6.0 {
		t.Errorf("connection: %v", conn)
	}
	switch status {
	case "OK":
		if conn["nat_ip"] != "203.0.113.5" || conn["nat_port"] != 40001.0 || e["timestamp"] != "2026-10-08T00:00:00Z" {
			t.Errorf("translation: %v", e)
		}
	case "DROPPED":
		if _, ok := conn["nat_ip"]; ok || conn["src_port"] != 40002.0 || e["timestamp"] != "2026-10-08T00:00:01Z" {
			t.Errorf("drop: %v", e)
		}
	}
	if gw := p["gateway_identifiers"].(map[string]any); gw["gateway_name"] != "nat" || gw["router_name"] != "router" {
		t.Errorf("gateway_identifiers: %v", gw)
	}
	if ep := p["endpoint"].(map[string]any); ep["vm_name"] != "vm-1" || ep["project_id"] != "p" {
		t.Errorf("endpoint: %v", ep)
	}
	if v := p["vpc"].(map[string]any); v["vpc_name"] != "vpc" || v["subnetwork_name"] != "nodes" {
		t.Errorf("vpc: %v", v)
	}
}

// TestNatEgressRequestLog: every internet egress attempt, dropped or not,
// reaches the request log (FR-CORE-061), whatever the NAT log filter.
func TestNatEgressRequestLog(t *testing.T) {
	s, _ := natTestService(t, &computev1.RouterNatLogConfig{Enable: true, Filter: "TRANSLATIONS_ONLY"})
	s.handleNatEvents(natReport{Network: tNet, Events: natTestEvents})
	var got []string
	for _, e := range s.env.RequestLog.Entries("nat") {
		got = append(got, e.Code+" "+e.Resource+" "+e.Method)
		if (e.Code == "DROPPED") != (e.Status == 1) {
			t.Errorf("status %d for %s", e.Status, e.Code)
		}
	}
	want := []string{
		"OK " + tRouter + "/nats/nat EGRESS 10.0.0.2:40000 -> 1.1.1.1:443",
		"DROPPED " + tRouter + "/nats/nat EGRESS 10.0.0.2:40002 -> 1.1.1.1:443",
		"DROPPED " + tNet + " EGRESS 10.0.1.2:5353 -> 8.8.8.8:53",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("request log:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestNatRulesDropChain: refused egress goes through GCPEMU-DROP, which
// NFLOGs new connections for the agent before dropping or rejecting.
func TestNatRulesDropChain(t *testing.T) {
	for _, reject := range []bool{false, true} {
		s, _ := natTestService(t, nil)
		s.env.Config.NATReject = reject
		g := &gateway{s: s, network: tNet, extIP: "172.22.0.2", extGW: "172.22.0.1"}
		rules, err := s.natRules(g, s.netsOf(tNet))
		if err != nil {
			t.Fatal(err)
		}
		accept := strings.Index(rules, "-A GCPEMU-FWD -s 10.0.0.0/24 -o $EXT -j ACCEPT\n")
		jump := strings.Index(rules, "-A GCPEMU-FWD -o $EXT -j GCPEMU-DROP\n")
		if accept < 0 || jump < accept || strings.Contains(rules, "-s 10.0.1.0/24 -o $EXT -j ACCEPT") {
			t.Fatalf("reject=%v: coverage/drop order:\n%s", reject, rules)
		}
		final := "-A GCPEMU-DROP -j DROP\n"
		if reject {
			final = "-A GCPEMU-DROP -j REJECT --reject-with icmp-net-unreachable\n"
		}
		if !strings.Contains(rules, final) || !strings.Contains(rules, ":GCPEMU-DROP - [0:0]\n") || !strings.Contains(rules, "-F GCPEMU-DROP\n") {
			t.Fatalf("reject=%v: drop chain:\n%s", reject, rules)
		}
		if !strings.Contains(rules, "\niptables -I GCPEMU-DROP -m conntrack --ctstate NEW -j NFLOG --nflog-group 15 2>/dev/null || ") {
			t.Fatalf("reject=%v: NFLOG rule:\n%s", reject, rules)
		}
	}
}

// nflogPacketMsg builds an NFULNL_MSG_PACKET carrying payload.
func nflogPacketMsg(payload []byte) []byte {
	var attrs []byte
	attrs = appendAttr(attrs, 1, []byte{0x08, 0x00, 4, 0}) // NFULA_PACKET_HDR
	attrs = appendAttr(attrs, 10, []byte("gcpemu\x00"))    // NFULA_PREFIX
	attrs = appendAttr(attrs, nfulaPayload, payload)
	b := make([]byte, nlmsgHdrLen+4)
	binary.NativeEndian.PutUint16(b[4:], nfnlSubsysULOG<<8|nfulnlMsgPacket)
	b[nlmsgHdrLen] = 2 // AF_INET
	binary.BigEndian.PutUint16(b[nlmsgHdrLen+2:], natDropGroup)
	b = append(b, attrs...)
	binary.NativeEndian.PutUint32(b, uint32(len(b)))
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

func ipv4(proto byte, frag uint16, src, dst [4]byte, l4 []byte) []byte {
	p := make([]byte, 20, 20+len(l4))
	p[0] = 0x45
	binary.BigEndian.PutUint16(p[2:], uint16(20+len(l4)))
	binary.BigEndian.PutUint16(p[6:], frag)
	p[8], p[9] = 64, proto
	copy(p[12:], src[:])
	copy(p[16:], dst[:])
	return append(p, l4...)
}

func TestParseNflog(t *testing.T) {
	tcp := make([]byte, 20)
	binary.BigEndian.PutUint16(tcp, 40000)
	binary.BigEndian.PutUint16(tcp[2:], 443)
	udp := make([]byte, 8)
	binary.BigEndian.PutUint16(udp, 5353)
	binary.BigEndian.PutUint16(udp[2:], 53)
	src, dst := [4]byte{10, 0, 0, 2}, [4]byte{1, 1, 1, 1}

	var dgram []byte
	dgram = append(dgram, nflogPacketMsg(ipv4(6, 0x4000, src, dst, tcp))...) // DF set
	dgram = append(dgram, nflogPacketMsg(ipv4(1, 0, src, dst, []byte{8, 0, 0, 0}))...)
	dgram = append(dgram, nflogPacketMsg(ipv4(17, 0x2000|10, src, dst, udp))...) // non-first fragment
	dgram = append(dgram, nflogPacketMsg([]byte{0x60, 0, 0, 0})...)              // not IPv4
	ack := make([]byte, nlmsgHdrLen+4)
	binary.NativeEndian.PutUint32(ack, uint32(len(ack)))
	binary.NativeEndian.PutUint16(ack[4:], nlmsgError)
	dgram = append(dgram, ack...)

	evs := parseNflog(dgram)
	var got []string
	for _, ev := range evs {
		if ev.Type != "dropped" || ev.Time == "" {
			t.Errorf("%+v", ev)
		}
		got = append(got, ev.Proto+" "+ev.Src+" "+ev.Dst+" "+strconv.Itoa(ev.SrcPort)+" "+strconv.Itoa(ev.DstPort))
	}
	want := "tcp 10.0.0.2 1.1.1.1 40000 443|icmp 10.0.0.2 1.1.1.1 0 0|udp 10.0.0.2 1.1.1.1 0 0"
	if strings.Join(got, "|") != want {
		t.Fatalf("got %q\nwant %q", strings.Join(got, "|"), want)
	}
	if evs := parseNflog(dgram[:7]); len(evs) != 0 {
		t.Fatal("truncated datagram parsed")
	}
}

func TestNflogConfigMsg(t *testing.T) {
	b := nflogConfigMsg(natDropGroup, 7)
	if int(binary.NativeEndian.Uint32(b)) != len(b) || binary.NativeEndian.Uint16(b[4:]) != 0x0401 ||
		binary.NativeEndian.Uint16(b[6:]) != nlmFRequest|nlmFAck || binary.NativeEndian.Uint32(b[8:]) != 7 ||
		binary.BigEndian.Uint16(b[18:]) != natDropGroup {
		t.Fatalf("header % x", b[:20])
	}
	// NFULA_CFG_CMD{BIND}, then NFULA_CFG_MODE{range 128, COPY_PACKET}.
	attrs := b[20:]
	if binary.NativeEndian.Uint16(attrs[2:]) != nfulaCfgCmd || attrs[4] != nfulnlCfgCmdBind {
		t.Fatalf("cmd % x", attrs)
	}
	mode := attrs[8:]
	if binary.NativeEndian.Uint16(mode[2:]) != nfulaCfgMode || binary.BigEndian.Uint32(mode[4:]) != nflogCopyRange || mode[8] != nfulnlCopyPacket {
		t.Fatalf("mode % x", mode)
	}
}
