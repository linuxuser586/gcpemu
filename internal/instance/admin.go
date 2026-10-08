package instance

import (
	"encoding/json"
	"net/http"
	"os"
	"time"

	"github.com/linuxuser586/gcpemu/internal/admin"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/fault"
)

// newAdmin returns the /_emu/v1/ admin API (FR-CORE-045, Section 7.3).
func newAdmin(in *Instance) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_emu/v1/openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(admin.OpenAPI)
	})
	mux.HandleFunc("GET /_emu/v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /_emu/v1/ready", func(w http.ResponseWriter, r *http.Request) {
		type svc struct {
			Ready  bool   `json:"ready"`
			Reason string `json:"reason,omitempty"`
		}
		out := map[string]svc{}
		code := http.StatusOK
		for name, err := range in.Readiness() {
			if err != nil {
				out[name] = svc{Reason: err.Error()}
				code = http.StatusServiceUnavailable
			} else {
				out[name] = svc{Ready: true}
			}
		}
		writeJSON(w, code, map[string]any{"ready": code == http.StatusOK, "services": out})
	})
	mux.HandleFunc("GET /_emu/v1/info", func(w http.ResponseWriter, r *http.Request) {
		names := []string{}
		for _, s := range in.services {
			names = append(names, s.Name())
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"instance":       in.Config.Instance,
			"id":             in.ID,
			"pid":            os.Getpid(),
			"version":        Version,
			"dir":            in.Dir,
			"ephemeral":      in.Config.Ephemeral,
			"iamMode":        in.Config.IAMMode,
			"strictProjects": in.Config.StrictProjects,
			"console":        in.consoleServed,
			"services":       names,
			"endpoints":      in.Env.Endpoints.All(),
			"runtime":        in.RuntimeStatus(r.Context()),
		})
	})
	mux.HandleFunc("GET /_emu/v1/endpoints", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, in.Env.Endpoints.All())
	})
	mux.HandleFunc("GET /_emu/v1/containers", func(w http.ResponseWriter, r *http.Request) {
		cts, err := in.Containers(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"containers": cts})
	})
	mux.HandleFunc("GET /_emu/v1/env", in.serveEnv)
	mux.HandleFunc("GET /_emu/v1/hostmode", in.serveHostMode)
	mux.HandleFunc("POST /_emu/v1/seed", func(w http.ResponseWriter, r *http.Request) {
		if err := in.ApplySeed(r.Context(), r.URL.Query().Get("path")); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "seeded"})
	})
	mux.HandleFunc("GET /_emu/v1/requests", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"requests": in.Log.Entries(r.URL.Query().Get("service"))})
	})
	mux.HandleFunc("GET /_emu/v1/resources", func(w http.ResponseWriter, r *http.Request) {
		dump, err := in.Env.Store.Dump()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		ns := r.URL.Query().Get("namespace")
		out := map[string]map[string]json.RawMessage{}
		for name, kv := range dump {
			if ns != "" && name != ns {
				continue
			}
			m := map[string]json.RawMessage{}
			for k, v := range kv {
				if json.Valid(v) {
					m[k] = v
				} else {
					m[k], _ = json.Marshal(string(v))
				}
			}
			out[name] = m
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /_emu/v1/resources/counts", func(w http.ResponseWriter, r *http.Request) {
		out := map[string]map[string]int{}
		for _, s := range in.services {
			if c, ok := s.(emu.ResourceCounter); ok {
				out[s.Name()] = c.ResourceCounts()
			}
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("POST /_emu/v1/reset", func(w http.ResponseWriter, r *http.Request) {
		if err := in.Reset(r.Context()); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "reset"})
	})
	mux.HandleFunc("POST /_emu/v1/time/advance", func(w http.ResponseWriter, r *http.Request) {
		d, err := time.ParseDuration(r.URL.Query().Get("by"))
		if err != nil || d < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "by must be a non-negative duration"})
			return
		}
		if err := in.AdvanceClock(r.Context(), d); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"now": in.Env.Clock.Now().Format(time.RFC3339Nano)})
	})
	mux.HandleFunc("GET /_emu/v1/faults", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"faults": in.Faults().List()})
	})
	mux.HandleFunc("POST /_emu/v1/faults", func(w http.ResponseWriter, r *http.Request) {
		var rule fault.Rule
		if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		added, err := in.Faults().Add(rule)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, added)
	})
	mux.HandleFunc("DELETE /_emu/v1/faults", func(w http.ResponseWriter, r *http.Request) {
		in.Faults().Remove("")
		writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
	})
	mux.HandleFunc("DELETE /_emu/v1/faults/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !in.Faults().Remove(r.PathValue("id")) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such fault"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
	})
	mux.HandleFunc("POST /_emu/v1/shutdown", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "shutting down"})
		in.RequestShutdown()
	})
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
