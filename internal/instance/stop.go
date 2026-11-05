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

// UnidentifiedError is Identify's error when the API gateway answered
// without saying which Instance it serves.
type UnidentifiedError struct {
	Code int
	Body []byte
}

func (e *UnidentifiedError) Error() string {
	return fmt.Sprintf("the API gateway did not say which Instance it serves: %d %s", e.Code, e.Body)
}

// Identify asks the Instance that call reaches which one it is. JSON with no
// ID, when there is one to compare, is not an Instance's answer. It fails
// unless that is the Instance id, when id is not "", and in process pid,
// when pid is not 0. The Instance has to say because the files in a data dir may be a
// crashed Instance's: their port may have another Instance on it, and their
// pid may be dead. call makes an Admin API request, returning the status
// and body.
func Identify(call func(method, path string) (int, []byte, error), id string, pid int) (Stopping, error) {
	var s Stopping
	code, b, err := call(http.MethodGet, "/_emu/v1/info")
	if err != nil {
		return s, err
	}
	if code != http.StatusOK {
		return s, &UnidentifiedError{Code: code, Body: b}
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, &UnidentifiedError{Code: code, Body: b}
	}
	if id != "" && s.ID == "" {
		return s, &UnidentifiedError{Code: code, Body: b}
	}
	if id != "" && s.ID != id {
		return s, fmt.Errorf("the API gateway serves Instance %s, not %s: another Instance took the port", s.ID, id)
	}
	if pid != 0 && s.Pid != pid {
		return s, fmt.Errorf("the API gateway serves process %d, not %d: another Instance took the port", s.Pid, pid)
	}
	return s, nil
}

// AskShutdown asks the Instance that call reaches to shut down, if Identify
// finds it is the Instance id in process pid.
func AskShutdown(call func(method, path string) (int, []byte, error), id string, pid int) (Stopping, error) {
	s, err := Identify(call, id, pid)
	if err != nil {
		return s, err
	}
	code, b, err := call(http.MethodPost, "/_emu/v1/shutdown")
	if err != nil {
		return s, err
	}
	if code >= 300 {
		return s, fmt.Errorf("the Instance refused to shut down: %d %s", code, b)
	}
	return s, nil
}
