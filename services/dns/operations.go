package dns

import (
	"encoding/json"
	"strings"

	dnsv1 "google.golang.org/api/dns/v1"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Operations implements emu.OperationLister. A change is an Operation on
// its managed zone (CONTEXT.md), pending until the configured latency has
// passed; managedZoneOperations complete at once.
func (s *Service) Operations() []emu.OperationInfo {
	var out []emu.OperationInfo
	_ = s.env.Store.View(func(tx store.Tx) error {
		tx.Scan(nsChanges, "", func(k string, b []byte) bool {
			var rec changeRec
			if json.Unmarshal(b, &rec) != nil || rec.Change == nil {
				return true
			}
			ch := s.withStatus(rec)
			raw, _ := json.Marshal(ch)
			oi := zoneOp(k, "changes", ch.Id, "change", raw)
			oi.Status, oi.Done, oi.StartTime = ch.Status, ch.Status == "done", emu.ParseTime(ch.StartTime)
			if oi.Done {
				oi.EndTime = rec.DoneAt
			}
			out = append(out, oi)
			return true
		})
		tx.Scan(nsOps, "", func(k string, b []byte) bool {
			var op dnsv1.Operation
			if json.Unmarshal(b, &op) != nil {
				return true
			}
			oi := zoneOp(k, "operations", op.Id, op.Type, b)
			oi.Status, oi.Done, oi.StartTime = op.Status, op.Status == "done", emu.ParseTime(op.StartTime)
			if oi.Done {
				oi.EndTime = oi.StartTime
			}
			out = append(out, oi)
			return true
		})
		return nil
	})
	return out
}

// zoneOp starts the summary of an Operation stored under key
// "project/zone/seq" in collection of its zone.
func zoneOp(key, collection, id, typ string, raw []byte) emu.OperationInfo {
	segs := strings.SplitN(key, "/", 3)
	if len(segs) < 2 {
		segs = append(segs, "", "")
	}
	zone := "projects/" + segs[0] + "/managedZones/" + segs[1]
	return emu.OperationInfo{
		Name: zone + "/" + collection + "/" + id, Project: segs[0], Location: "global",
		Type: typ, Target: zone, Operation: raw,
	}
}
