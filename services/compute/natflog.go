package compute

import (
	"encoding/binary"
	"net"
	"strconv"
	"time"
)

// NFLOG over netlink (linux/netfilter/nfnetlink_log.h). The gateway's
// drop chain copies the first packet of every refused connection to NFLOG
// group natDropGroup; the agent reads them and reports "dropped" events
// (FR-NAT-004). Parsing is platform-independent so it is unit-tested
// everywhere; the socket lives in natflog_linux.go.

// natDropGroup is the NFLOG group of the gateway's drop chain.
const natDropGroup = 15

const (
	nlmsgHdrLen      = 16
	nlmsgError       = 2
	nfnlSubsysULOG   = 4
	nfulnlMsgPacket  = 0
	nfulnlMsgConfig  = 1
	nfulaPayload     = 9
	nfulaCfgCmd      = 1
	nfulaCfgMode     = 2
	nfulnlCfgCmdBind = 1
	nfulnlCopyPacket = 2
	nlaTypeMask      = 0x3fff
	nlmFRequest      = 0x1
	nlmFAck          = 0x4
	// nflogCopyRange is enough for the IPv4 and transport headers.
	nflogCopyRange = 128
)

func align4(n int) int { return (n + 3) &^ 3 }

// nflogConfigMsg builds an NFULNL_MSG_CONFIG request for group: bind it
// and copy packet headers to user space.
func nflogConfigMsg(group uint16, seq uint32) []byte {
	var attrs []byte
	attrs = appendAttr(attrs, nfulaCfgCmd, []byte{nfulnlCfgCmdBind})
	mode := make([]byte, 6) // struct nfulnl_msg_config_mode
	binary.BigEndian.PutUint32(mode, nflogCopyRange)
	mode[4] = nfulnlCopyPacket
	attrs = appendAttr(attrs, nfulaCfgMode, mode)

	b := make([]byte, nlmsgHdrLen+4, nlmsgHdrLen+4+len(attrs))
	binary.NativeEndian.PutUint16(b[4:], nfnlSubsysULOG<<8|nfulnlMsgConfig)
	binary.NativeEndian.PutUint16(b[6:], nlmFRequest|nlmFAck)
	binary.NativeEndian.PutUint32(b[8:], seq)
	// struct nfgenmsg: family AF_UNSPEC, version NFNETLINK_V0, res_id (group).
	binary.BigEndian.PutUint16(b[nlmsgHdrLen+2:], group)
	b = append(b, attrs...)
	binary.NativeEndian.PutUint32(b, uint32(len(b)))
	return b
}

func appendAttr(b []byte, typ uint16, v []byte) []byte {
	h := make([]byte, 4)
	binary.NativeEndian.PutUint16(h, uint16(4+len(v)))
	binary.NativeEndian.PutUint16(h[2:], typ)
	b = append(append(b, h...), v...)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

// nlmsgs splits a netlink datagram into (type, body) messages.
func nlmsgs(b []byte, fn func(typ uint16, body []byte)) {
	for len(b) >= nlmsgHdrLen {
		l := int(binary.NativeEndian.Uint32(b))
		if l < nlmsgHdrLen || l > len(b) {
			return
		}
		fn(binary.NativeEndian.Uint16(b[4:]), b[nlmsgHdrLen:l])
		b = b[min(align4(l), len(b)):]
	}
}

// parseNflog extracts drop events from an NFLOG datagram: one
// NFULNL_MSG_PACKET per packet, whose NFULA_PAYLOAD is the IPv4 packet.
func parseNflog(b []byte) []natEvent {
	var out []natEvent
	nlmsgs(b, func(typ uint16, body []byte) {
		if typ != nfnlSubsysULOG<<8|nfulnlMsgPacket || len(body) < 4 {
			return
		}
		attrs := body[4:] // past struct nfgenmsg
		for len(attrs) >= 4 {
			l := int(binary.NativeEndian.Uint16(attrs))
			if l < 4 || l > len(attrs) {
				return
			}
			if binary.NativeEndian.Uint16(attrs[2:])&nlaTypeMask == nfulaPayload {
				if ev, ok := parseIPv4(attrs[4:l]); ok {
					out = append(out, ev)
				}
				return
			}
			attrs = attrs[min(align4(l), len(attrs)):]
		}
	})
	return out
}

// parseIPv4 turns a dropped packet's headers into a "dropped" event.
func parseIPv4(p []byte) (natEvent, bool) {
	if len(p) < 20 || p[0]>>4 != 4 {
		return natEvent{}, false
	}
	ihl := int(p[0]&0xf) * 4
	ev := natEvent{Type: "dropped", Time: time.Now().UTC().Format(time.RFC3339Nano),
		Src: net.IP(p[12:16]).String(), Dst: net.IP(p[16:20]).String()}
	first := binary.BigEndian.Uint16(p[6:])&0x1fff == 0 // fragment offset 0
	switch p[9] {
	case 6, 17:
		ev.Proto = "tcp"
		if p[9] == 17 {
			ev.Proto = "udp"
		}
		if first && ihl >= 20 && len(p) >= ihl+4 {
			ev.SrcPort = int(binary.BigEndian.Uint16(p[ihl:]))
			ev.DstPort = int(binary.BigEndian.Uint16(p[ihl+2:]))
		}
	case 1:
		ev.Proto = "icmp"
	default:
		ev.Proto = strconv.Itoa(int(p[9]))
	}
	return ev, true
}
