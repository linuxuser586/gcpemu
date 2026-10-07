package dns

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

const v1 = "/dns/v1/projects/{project}"

// handler returns the REST router for dns.googleapis.com/dns/v1.
func (s *Service) handler() http.Handler {
	mux := http.NewServeMux()
	z := v1 + "/managedZones/{managedZone}"
	mux.HandleFunc("GET "+v1, s.getProject)
	mux.HandleFunc("POST "+v1+"/managedZones", s.createZone)
	mux.HandleFunc("GET "+v1+"/managedZones", s.listZones)
	mux.HandleFunc("GET "+z, s.getZone)
	mux.HandleFunc("PATCH "+z, s.patchZone)
	mux.HandleFunc("PUT "+z, s.updateZone)
	mux.HandleFunc("DELETE "+z, s.deleteZone)
	mux.HandleFunc("POST "+z+"/changes", s.createChange)
	mux.HandleFunc("GET "+z+"/changes", s.listChanges)
	mux.HandleFunc("GET "+z+"/changes/{changeId}", s.getChange)
	mux.HandleFunc("POST "+z+"/rrsets", s.createRRSet)
	mux.HandleFunc("GET "+z+"/rrsets", s.listRRSets)
	mux.HandleFunc("GET "+z+"/rrsets/{name}/{type}", s.getRRSet)
	mux.HandleFunc("PATCH "+z+"/rrsets/{name}/{type}", s.patchRRSet)
	mux.HandleFunc("DELETE "+z+"/rrsets/{name}/{type}", s.deleteRRSet)
	mux.HandleFunc("GET "+z+"/operations", s.listOperations)
	mux.HandleFunc("GET "+z+"/operations/{operation}", s.getOperation)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		apierr.Write(w, apierr.NotFound("The requested URL %s was not found on this server.", r.URL.Path))
	})
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// decode reads a JSON request body into v.
func decode(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		return apierr.InvalidArgument("Failed to read request body: %v", err).WithLegacy("parseError")
	}
	if len(b) == 0 {
		return apierr.InvalidArgument("Required request body is missing.").WithLegacy("required")
	}
	if err := json.Unmarshal(b, v); err != nil {
		return apierr.InvalidArgument("Invalid JSON payload received. %v", err).WithLegacy("parseError")
	}
	return nil
}

// keyed pairs a list item with its sort/page key.
type keyed[T any] struct {
	key  string
	item T
}

// paginate applies maxResults/pageToken (FR-CORE-024) to items sorted by key.
// The page token is the opaque encoding of the last returned key.
func paginate[T any](r *http.Request, items []keyed[T]) (page []T, next string, err error) {
	sort.SliceStable(items, func(i, j int) bool { return items[i].key < items[j].key })
	q := r.URL.Query()
	max := 0
	if v := q.Get("maxResults"); v != "" {
		max, err = strconv.Atoi(v)
		if err != nil || max < 0 {
			return nil, "", errInvalid("parameters.maxResults", v)
		}
	}
	after := ""
	if tok := q.Get("pageToken"); tok != "" {
		b, derr := base64.RawURLEncoding.DecodeString(tok)
		if derr != nil {
			return nil, "", errInvalid("parameters.pageToken", tok)
		}
		after = string(b)
	}
	for _, it := range items {
		if after != "" && it.key <= after {
			continue
		}
		if max > 0 && len(page) == max {
			next = base64.RawURLEncoding.EncodeToString([]byte(lastKey(items, page, after)))
			break
		}
		page = append(page, it.item)
	}
	return page, next, nil
}

// lastKey returns the key of the last item placed on the page.
func lastKey[T any](items []keyed[T], page []T, after string) string {
	n := 0
	for _, it := range items {
		if after != "" && it.key <= after {
			continue
		}
		n++
		if n == len(page) {
			return it.key
		}
	}
	return ""
}
