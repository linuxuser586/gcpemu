package instance

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Stopping is the Instance that accepted a shutdown, as it reported itself.
type Stopping struct {
	ID       string `json:"id"`
	Pid      int    `json:"pid"`
	PIDScope string `json:"pidScope"`
}

// Watchable reports whether this process can tell when the Instance's
// process exits. A Containerized Instance's pid means nothing here: it may
// be missing or name an unrelated process (ADR 0064). So may the pid of
// an Instance too old to report its scope. Pid 1 in this process's own
// scope is the container this process runs in, which it cannot outlive.
func (s Stopping) Watchable() bool {
	return s.Pid > 1 && s.PIDScope != "" && s.PIDScope == PIDScope()
}

// AskShutdown asks the Instance that call reaches to shut down, if it is
// the Instance id, unless id is "", and in process pid, unless pid is 0.
// The Instance says which one it is because the files in a data dir may
// be a crashed Instance's: their port may have another Instance on it, and
// their pid may be dead. call makes an Admin API request, returning the
// status and body.
func AskShutdown(call func(method, path string) (int, []byte, error), id string, pid int) (Stopping, error) {
	var s Stopping
	code, b, err := call(http.MethodGet, "/_emu/v1/info")
	if err != nil {
		return s, err
	}
	if code != http.StatusOK {
		return s, fmt.Errorf("the API gateway did not say which Instance it serves: %d %s", code, b)
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, err
	}
	if id != "" && s.ID != id {
		return s, fmt.Errorf("the API gateway serves Instance %s, not %s: another Instance took the port", s.ID, id)
	}
	if pid != 0 && s.Pid != pid {
		return s, fmt.Errorf("the API gateway serves process %d, not %d: another Instance took the port", s.Pid, pid)
	}
	code, b, err = call(http.MethodPost, "/_emu/v1/shutdown")
	if err != nil {
		return s, err
	}
	if code >= 300 {
		return s, fmt.Errorf("the Instance refused to shut down: %d %s", code, b)
	}
	return s, nil
}
