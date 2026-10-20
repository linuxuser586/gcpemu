package instance

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/linuxuser586/gcpemu/internal/emu"
)

// waitConditionState is one Wait condition in the body of /_emu/v1/conditions.
type waitConditionState struct {
	For string `json:"for"`
	emu.WaitStatus
}

// serveConditions is GET /_emu/v1/conditions?for=KIND=TARGET&for=...
// (FR-CI-007): the state of each Wait condition, evaluated by the Service
// that registered its kind. A malformed condition or an unknown kind is a
// 400.
func (in *Instance) serveConditions(w http.ResponseWriter, r *http.Request) {
	fors := r.URL.Query()["for"]
	if len(fors) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "at least one for=KIND=TARGET is required"})
		return
	}
	kinds := in.waitConditions()
	out := []waitConditionState{}
	met := true
	for _, f := range fors {
		kind, target, err := emu.ParseWaitCondition(f)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		eval, ok := kinds[kind]
		if !ok {
			known := "no running Service has Wait conditions"
			if len(kinds) > 0 {
				known = "want one of " + strings.Join(sortedKinds(kinds), ", ")
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("Wait condition %q: unknown kind %q (%s)", f, kind, known)})
			return
		}
		st, err := eval(r.Context(), target)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("Wait condition %q: %v", f, err)})
			return
		}
		met = met && st.State == emu.WaitMet
		out = append(out, waitConditionState{For: f, WaitStatus: st})
	}
	writeJSON(w, http.StatusOK, map[string]any{"met": met, "conditions": out})
}

// waitConditions collects the Wait condition kinds of the running services.
func (in *Instance) waitConditions() map[string]emu.WaitCondition {
	kinds := map[string]emu.WaitCondition{}
	for _, s := range in.services {
		if c, ok := s.(emu.WaitConditioner); ok {
			for k, f := range c.WaitConditions() {
				kinds[k] = f
			}
		}
	}
	return kinds
}

func sortedKinds(m map[string]emu.WaitCondition) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
