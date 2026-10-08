package lro

import (
	"encoding/json"
	"strings"

	"google.golang.org/grpc/codes"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Operations implements emu.OperationLister for the manager's service.
func (m *Manager) Operations() []emu.OperationInfo {
	var out []emu.OperationInfo
	_ = m.env.Store.View(func(tx store.Tx) error {
		tx.Scan(Namespace, "", func(_ string, b []byte) bool {
			var rec record
			if json.Unmarshal(b, &rec) == nil && rec.Service == m.service {
				out = append(out, info(rec))
			}
			return true
		})
		return nil
	})
	return out
}

// info summarises a stored operation. The target and type come from the
// conventional metadata fields (target, verb) when the API has them, and
// otherwise from the name of the resource in the response.
func info(rec record) emu.OperationInfo {
	var op struct {
		Name     string `json:"name"`
		Done     bool   `json:"done"`
		Metadata struct {
			Target string `json:"target"`
			Verb   string `json:"verb"`
		} `json:"metadata"`
		Response struct {
			Name string `json:"name"`
		} `json:"response"`
		Error *struct {
			Code    int32  `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Op, &op)
	oi := emu.OperationInfo{
		Name:      op.Name,
		Type:      op.Metadata.Verb,
		Target:    op.Metadata.Target,
		Done:      op.Done,
		Status:    "RUNNING",
		StartTime: rec.Start,
		EndTime:   rec.End,
		Operation: rec.Op,
	}
	if oi.Target == "" {
		oi.Target = op.Response.Name
	}
	if op.Done {
		oi.Status = "DONE"
	}
	if op.Error != nil {
		oi.Error = &emu.OperationError{Code: apierr.CodeName(codes.Code(op.Error.Code)), Message: op.Error.Message}
	}
	// "projects/P/locations/L/operations/ID"
	segs := strings.Split(op.Name, "/")
	if len(segs) >= 2 && segs[0] == "projects" {
		oi.Project = segs[1]
	}
	if len(segs) >= 4 && segs[2] == "locations" {
		oi.Location = segs[3]
	}
	return oi
}
