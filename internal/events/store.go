package events

import (
	"encoding/json"
	"strings"

	"github.com/linuxuser586/gcpemu/internal/store"
)

// ResourceChange is the data of a Resource event.
type ResourceChange struct {
	// Service is the namespace's first segment: a Service ID, or "core"
	// for state the emulator owns (Projects).
	Service   string `json:"service"`
	Namespace string `json:"namespace"`
	Key       string `json:"key"`
	Deleted   bool   `json:"deleted,omitempty"`
}

// OperationChange is the data of an Operation event.
type OperationChange struct {
	Service   string `json:"service"`
	Namespace string `json:"namespace"`
	// Name is the Operation's store key, or its name for
	// google.longrunning Operations.
	Name    string `json:"name"`
	Done    bool   `json:"done"`
	Deleted bool   `json:"deleted,omitempty"`
}

// operationNamespaces hold Operations whatever their wire format; a Cloud
// DNS change is an Operation too (CONTEXT.md).
var operationNamespaces = map[string]bool{
	"lro/operations":     true,
	"compute/operations": true,
	"sql/operations":     true,
	"gke/operations":     true,
	"dns/ops":            true,
	"dns/changes":        true,
}

// StoreObserver returns a store.Observer publishing Resource and Operation
// events to h, and a Reset event when the store is replaced.
func StoreObserver(h *Hub) store.Observer { return storeObserver{h} }

type storeObserver struct{ h *Hub }

func (o storeObserver) Committed(changes []store.Change) {
	for _, c := range changes {
		svc, _, _ := strings.Cut(c.NS, "/")
		if !operationNamespaces[c.NS] {
			o.h.Publish(Resource, ResourceChange{Service: svc, Namespace: c.NS, Key: c.Key, Deleted: c.Deleted})
			continue
		}
		oc := OperationChange{Service: svc, Namespace: c.NS, Name: c.Key, Deleted: c.Deleted}
		if !c.Deleted {
			describeOperation(&oc, c.Val)
		}
		o.h.Publish(Operation, oc)
	}
}

func (o storeObserver) Replaced() { o.h.Publish(Reset, struct{}{}) }

// describeOperation fills the Service, Name and Done of oc from a stored
// Operation: google.longrunning ({"service", "op": {"name", "done"}}),
// Compute, Cloud SQL and GKE ({"status": "DONE"}) or Cloud DNS
// ({"status": "done"}).
func describeOperation(oc *OperationChange, val []byte) {
	var v struct {
		Service string `json:"service"`
		Status  string `json:"status"`
		Op      *struct {
			Name string `json:"name"`
			Done bool   `json:"done"`
		} `json:"op"`
	}
	if json.Unmarshal(val, &v) != nil {
		return
	}
	if v.Op != nil {
		if v.Service != "" {
			oc.Service = v.Service
		}
		oc.Name, oc.Done = v.Op.Name, v.Op.Done
		return
	}
	oc.Done = strings.EqualFold(v.Status, "done")
}
