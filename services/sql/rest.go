package sql

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

type versionKey struct{}

// ServeHTTP routes the sqladmin REST API. Both API versions share one
// implementation (the resource shapes are identical): v1beta4 at
// "/sql/v1beta4/..." (gcloud, OpenTofu, the Cloud SQL connector) and v1 at
// "/v1/..." (google.golang.org/api/sqladmin/v1). The agent endpoint lives
// at "/_agent/v1/...".
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	var ver, rest string
	switch {
	case strings.HasPrefix(p, "/sql/v1beta4/"):
		ver, rest = "v1beta4", strings.TrimPrefix(p, "/sql/v1beta4")
	case strings.HasPrefix(p, "/v1/"):
		ver, rest = "v1", strings.TrimPrefix(p, "/v1")
	case strings.HasPrefix(p, "/_agent/v1/"):
		ver, rest = "agent", strings.TrimPrefix(p, "/_agent/v1")
		rest = "/_agent" + rest
	case strings.HasPrefix(p, "/projects/") || p == "/flags":
		// Version-less form, for clients whose base URL already includes
		// the version (e.g. an override of ".../sql/v1beta4/" style).
		ver, rest = "v1beta4", p
	default:
		apierr.Write(w, apierr.NotFound("The requested URL %s was not found on this server.", p))
		return
	}
	// Custom methods "instances/I:generateEphemeralCert" become a segment.
	if i := strings.LastIndex(rest, ":"); i > strings.LastIndex(rest, "/") {
		rest = rest[:i] + "/" + rest[i+1:]
	}
	r2 := r.Clone(context.WithValue(r.Context(), versionKey{}, ver))
	r2.URL.Path = rest
	r2.URL.RawPath = ""
	s.mux.ServeHTTP(w, r2)
}

// routes builds the method router (paths relative to the version root).
func (s *Service) routes() *http.ServeMux {
	mux := http.NewServeMux()
	p := "/projects/{project}"
	i := p + "/instances/{instance}"
	mux.HandleFunc("GET /flags", s.listFlags)
	mux.HandleFunc("GET "+p+"/tiers", s.listTiers)

	mux.HandleFunc("POST "+p+"/instances", s.insertInstance)
	mux.HandleFunc("GET "+p+"/instances", s.listInstances)
	mux.HandleFunc("GET "+i, s.getInstanceH)
	mux.HandleFunc("PATCH "+i, s.patchInstance)
	mux.HandleFunc("PUT "+i, s.updateInstance)
	mux.HandleFunc("DELETE "+i, s.deleteInstance)
	mux.HandleFunc("POST "+i+"/restart", s.restartInstance)
	mux.HandleFunc("POST "+i+"/clone", s.cloneInstance)
	mux.HandleFunc("POST "+i+"/import", s.importInstance)
	mux.HandleFunc("POST "+i+"/export", s.exportInstance)
	mux.HandleFunc("POST "+i+"/restoreBackup", s.restoreBackup)
	mux.HandleFunc("POST "+i+"/resetSslConfig", s.resetSslConfig)
	mux.HandleFunc("GET "+i+"/listServerCas", s.listServerCas)
	mux.HandleFunc("GET "+i+"/listServerCertificates", s.listServerCertificates)

	mux.HandleFunc("GET "+i+"/connectSettings", s.connectSettings)
	mux.HandleFunc("POST "+i+"/generateEphemeralCert", s.generateEphemeralCert)
	mux.HandleFunc("POST "+i+"/createEphemeral", s.createEphemeral)
	mux.HandleFunc("POST "+i+"/executeSql", s.executeSQL)

	mux.HandleFunc("POST "+i+"/sslCerts", s.insertSslCert)
	mux.HandleFunc("GET "+i+"/sslCerts", s.listSslCerts)
	mux.HandleFunc("GET "+i+"/sslCerts/{fp}", s.getSslCert)
	mux.HandleFunc("DELETE "+i+"/sslCerts/{fp}", s.deleteSslCert)

	mux.HandleFunc("POST "+i+"/databases", s.insertDatabase)
	mux.HandleFunc("GET "+i+"/databases", s.listDatabases)
	mux.HandleFunc("GET "+i+"/databases/{database}", s.getDatabase)
	mux.HandleFunc("PATCH "+i+"/databases/{database}", s.patchDatabase)
	mux.HandleFunc("PUT "+i+"/databases/{database}", s.patchDatabase)
	mux.HandleFunc("DELETE "+i+"/databases/{database}", s.deleteDatabase)

	mux.HandleFunc("POST "+i+"/users", s.insertUser)
	mux.HandleFunc("GET "+i+"/users", s.listUsers)
	mux.HandleFunc("PUT "+i+"/users", s.updateUser)
	mux.HandleFunc("DELETE "+i+"/users", s.deleteUser)
	mux.HandleFunc("GET "+i+"/users/{name}", s.getUser)

	mux.HandleFunc("POST "+i+"/backupRuns", s.insertBackupRun)
	mux.HandleFunc("GET "+i+"/backupRuns", s.listBackupRuns)
	mux.HandleFunc("GET "+i+"/backupRuns/{id}", s.getBackupRun)
	mux.HandleFunc("DELETE "+i+"/backupRuns/{id}", s.deleteBackupRun)

	mux.HandleFunc("GET "+p+"/operations", s.listOperations)
	mux.HandleFunc("GET "+p+"/operations/{operation}", s.getOperation)
	mux.HandleFunc("POST "+p+"/operations/{operation}/wait", s.waitOperation)
	mux.HandleFunc("POST "+p+"/operations/{operation}/cancel", s.cancelOperation)

	mux.HandleFunc("POST /_agent"+i+"/login", s.agentLogin)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		apierr.Write(w, apierr.NotFound("The requested URL %s was not found on this server.", r.URL.Path))
	})
	return mux
}

// writeJSON renders v; for the v1 API, self links point at /v1/.
func writeJSON(w http.ResponseWriter, r *http.Request, v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		apierr.Write(w, apierr.Internal("%v", err))
		return
	}
	if ver, _ := r.Context().Value(versionKey{}).(string); ver == "v1" {
		b = bytes.ReplaceAll(b, []byte(linkBase), []byte(linkV1))
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(b, '\n'))
}

// decode reads a JSON request body into v. An empty body is allowed when
// optional is true.
func decode(r *http.Request, v any, optional bool) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		return apierr.InvalidArgument("Failed to read request body: %v", err).WithLegacy("parseError")
	}
	if len(bytes.TrimSpace(b)) == 0 {
		if optional {
			return nil
		}
		return apierr.InvalidArgument("Invalid request: request body is missing.").WithLegacy("required")
	}
	if err := json.Unmarshal(b, v); err != nil {
		return apierr.InvalidArgument("Invalid JSON payload received. %v", err).WithLegacy("parseError")
	}
	return nil
}

// readBody returns the raw JSON body (for merge-patch semantics).
func readBody(r *http.Request) (map[string]any, error) {
	var m map[string]any
	if err := decode(r, &m, false); err != nil {
		return nil, err
	}
	return m, nil
}

// paginate applies maxResults/pageToken (FR-CORE-024) to items in key order.
func paginate[T any](r *http.Request, keys []string, items map[string]T) ([]T, string, error) {
	sort.Strings(keys)
	q := r.URL.Query()
	max := 0
	if v := q.Get("maxResults"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, "", errInvalid("Invalid value for maxResults: %s", v)
		}
		max = n
	}
	after := ""
	if tok := q.Get("pageToken"); tok != "" {
		b, err := base64.RawURLEncoding.DecodeString(tok)
		if err != nil {
			return nil, "", errInvalid("Invalid page token.")
		}
		after = string(b)
	}
	var out []T
	last := ""
	for _, k := range keys {
		if after != "" && k <= after {
			continue
		}
		if max > 0 && len(out) == max {
			return out, base64.RawURLEncoding.EncodeToString([]byte(last)), nil
		}
		out = append(out, items[k])
		last = k
	}
	return out, "", nil
}
