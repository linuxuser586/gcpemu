package instance

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/linuxuser586/gcpemu/internal/admin"
	"github.com/linuxuser586/gcpemu/internal/config"
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
		if err := in.containers.disrupted(r.Context()); err != nil {
			out["containers"] = svc{Reason: err.Error()}
			code = http.StatusServiceUnavailable
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
			"iamMode":        in.Env.Auth.Mode(),
			"strictProjects": in.Config.StrictProjects,
			"console":        in.consoleServed,
			"services":       names,
			"endpoints":      in.Env.Endpoints.All(),
			"runtime":        in.RuntimeStatus(r.Context()),
		})
	})
	mux.HandleFunc("GET /_emu/v1/iam/enforcement", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, enforcement{Level: in.Env.Auth.Mode()})
	})
	mux.HandleFunc("PUT /_emu/v1/iam/enforcement", in.setEnforcement)
	mux.HandleFunc("GET /_emu/v1/iam/denials", func(w http.ResponseWriter, r *http.Request) {
		denials := []emu.Denial{}
		if a, ok := in.Env.Auth.(*emu.PolicyAuthorizer); ok {
			denials = a.Denials(r.Context(), r.URL.Query().Get("project"))
		}
		writeJSON(w, http.StatusOK, map[string]any{"denials": denials})
	})
	mux.HandleFunc("DELETE /_emu/v1/iam/denials", func(w http.ResponseWriter, r *http.Request) {
		if a, ok := in.Env.Auth.(*emu.PolicyAuthorizer); ok {
			a.ClearDenials()
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
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
	mux.HandleFunc("GET /_emu/v1/external", in.serveExternal)
	mux.HandleFunc("GET /_emu/v1/hostmode", in.serveHostMode)
	mux.HandleFunc("POST /_emu/v1/adc-key", in.serveADCKey)
	mux.HandleFunc("POST /_emu/v1/seed", func(w http.ResponseWriter, r *http.Request) {
		var err error
		if path := r.URL.Query().Get("path"); path != "" {
			err = in.ApplySeed(r.Context(), path)
		} else {
			// An uploaded seed file: its relative paths resolve against the instance dir.
			var b []byte
			if b, err = io.ReadAll(http.MaxBytesReader(w, r.Body, maxSeedUpload)); err == nil {
				err = in.ApplySeedData(r.Context(), b, in.Dir)
			}
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "seeded"})
	})
	mux.HandleFunc("GET /_emu/v1/seed", func(w http.ResponseWriter, r *http.Request) {
		b, err := in.ExportSeed(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/yaml")
		w.Header().Set("Content-Disposition", `attachment; filename="seed.yaml"`)
		_, _ = w.Write(b)
	})
	mux.Handle("GET /_emu/v1/events", in.Events)
	mux.HandleFunc("GET /_emu/v1/requests", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"requests": in.Log.Entries(r.URL.Query().Get("service"))})
	})
	mux.HandleFunc("GET /_emu/v1/requests/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		d, ok := in.Log.Get(id)
		if err != nil || !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such request log entry; it may no longer be retained"})
			return
		}
		writeJSON(w, http.StatusOK, d)
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
	mux.HandleFunc("GET /_emu/v1/conditions", in.serveConditions)
	mux.HandleFunc("GET /_emu/v1/operations", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"operations": in.Operations(r.URL.Query().Get("service"), r.URL.Query().Get("project")),
		})
	})
	mux.HandleFunc("POST /_emu/v1/reset", func(w http.ResponseWriter, r *http.Request) {
		if err := in.Reset(r.Context()); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "reset"})
	})
	mux.HandleFunc("GET /_emu/v1/snapshots", func(w http.ResponseWriter, r *http.Request) {
		snaps, err := in.Snapshots()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"snapshots": snaps})
	})
	mux.HandleFunc("POST /_emu/v1/snapshots", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		info, err := in.SaveSnapshot(r.Context(), body.Name)
		if err != nil {
			writeJSON(w, snapshotStatus(err), map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, info)
	})
	mux.HandleFunc("POST /_emu/v1/snapshots/{name}/restore", func(w http.ResponseWriter, r *http.Request) {
		if err := in.RestoreSnapshot(r.Context(), r.PathValue("name")); err != nil {
			writeJSON(w, snapshotStatus(err), map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "restored"})
	})
	mux.HandleFunc("DELETE /_emu/v1/snapshots/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := in.DeleteSnapshot(r.PathValue("name")); err != nil {
			writeJSON(w, snapshotStatus(err), map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
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
	mux.HandleFunc("PATCH /_emu/v1/faults/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Disabled *bool `json:"disabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Disabled == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {\"disabled\": true|false}"})
			return
		}
		rule, ok := in.Faults().SetDisabled(r.PathValue("id"), *body.Disabled)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such fault"})
			return
		}
		writeJSON(w, http.StatusOK, rule)
	})
	mux.HandleFunc("DELETE /_emu/v1/faults/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !in.Faults().Remove(r.PathValue("id")) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such fault"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
	})
	in.linkRoutes(mux)
	mux.HandleFunc("POST /_emu/v1/shutdown", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "shutting down"})
		in.RequestShutdown()
	})
	return mux
}

// enforcement is the body of /_emu/v1/iam/enforcement.
type enforcement struct {
	Level string `json:"level"`
}

// setEnforcement is PUT /_emu/v1/iam/enforcement (FR-IAM-004, SRS 4.8.3):
// it switches the IAM enforcement level until the Instance stops. As at
// startup, a gateway bound beyond loopback is not left without IAM unless
// --i-understand-this-is-insecure was given.
func (in *Instance) setEnforcement(w http.ResponseWriter, r *http.Request) {
	var req enforcement
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	switch req.Level {
	case config.IAMOff, config.IAMAudit, config.IAMEnforce:
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("invalid IAM enforcement level %q (want off, audit or enforce)", req.Level)})
		return
	}
	if req.Level == config.IAMOff && !config.IsLoopback(in.Config.Bind) && !in.Config.Insecure {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("refusing to turn IAM off with the gateway bound to %s; start with --i-understand-this-is-insecure", in.Config.Bind)})
		return
	}
	setter, ok := in.Env.Auth.(interface{ SetMode(string) })
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "the IAM enforcement level cannot be changed"})
		return
	}
	if was := in.Env.Auth.Mode(); was != req.Level {
		setter.SetMode(req.Level)
		in.Env.Log.Info("IAM enforcement level changed", "from", was, "to", req.Level)
	}
	writeJSON(w, http.StatusOK, enforcement{Level: in.Env.Auth.Mode()})
}

// maxSeedUpload bounds an uploaded seed file, which may hold object content.
const maxSeedUpload = 64 << 20

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
