package nat

import (
	"encoding/json"
	"net/http"
)

// The Web console's Cloud NAT view (SRS 4.8.3) toggles the offline sink
// (FR-NAT-005), which GCP has no API for:
//
//	GET /_emu/v1/nat/sink
//	PUT /_emu/v1/nat/sink {"enabled": true}
//
// While it is on, egress a Cloud NAT allows is answered by the sink in
// the NAT egress proxy instead of reaching the internet. `--offline` turns
// it on at start; a switch lasts until the Instance stops. The data plane
// is compute's, which implements sinkSwitch.

type sinkSwitch interface {
	NATSink() (on bool, status int, body string)
	SetNATSink(on bool)
}

// sinkState is the body of /_emu/v1/nat/sink. Status and Body are the
// sink's HTTP response (natSinkResponse), read only.
type sinkState struct {
	Enabled bool   `json:"enabled"`
	Status  int    `json:"status,omitempty"`
	Body    string `json:"body,omitempty"`
}

func (s *Service) sinkSwitch(w http.ResponseWriter) (sinkSwitch, bool) {
	c, ok := s.env.Lookup("compute")
	sw, ok2 := c.(sinkSwitch)
	if !ok || !ok2 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "nat: the compute service (Cloud NAT data plane) is not running"})
		return nil, false
	}
	return sw, true
}

func (s *Service) writeSink(w http.ResponseWriter, sw sinkSwitch) {
	on, status, body := sw.NATSink()
	writeJSON(w, http.StatusOK, sinkState{Enabled: on, Status: status, Body: body})
}

// getSink is GET /_emu/v1/nat/sink.
func (s *Service) getSink(w http.ResponseWriter, r *http.Request) {
	if sw, ok := s.sinkSwitch(w); ok {
		s.writeSink(w, sw)
	}
}

// putSink is PUT /_emu/v1/nat/sink: it switches the sink and re-programs
// the NAT egress proxies.
func (s *Service) putSink(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.Enabled == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "enabled is required"})
		return
	}
	sw, ok := s.sinkSwitch(w)
	if !ok {
		return
	}
	sw.SetNATSink(*req.Enabled)
	s.writeSink(w, sw)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
