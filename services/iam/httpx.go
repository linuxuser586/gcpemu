package iam

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// writeJSON writes v as a 200 JSON response.
func writeJSON(w http.ResponseWriter, v any) {
	writeJSONStatus(w, http.StatusOK, v)
}

func writeJSONStatus(w http.ResponseWriter, code int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		apierr.Write(w, apierr.Internal("encode response: %v", err))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(code)
	_, _ = w.Write(b)
}

// readJSON decodes a JSON request body into v; an empty body is allowed.
func readJSON(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return apierr.InvalidArgument("read body: %v", err)
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, v); err != nil {
		return apierr.InvalidArgument("Invalid JSON payload received. %v", err)
	}
	return nil
}

// route is a parsed REST path: its segments and an optional ":verb" suffix
// on the last segment (AIP-136 custom methods).
type route struct {
	segs []string
	verb string
}

func parseRoute(path string) route {
	path = strings.Trim(path, "/")
	var rt route
	if path == "" {
		return rt
	}
	rt.segs = strings.Split(path, "/")
	last := rt.segs[len(rt.segs)-1]
	if i := strings.LastIndexByte(last, ':'); i >= 0 {
		rt.segs[len(rt.segs)-1], rt.verb = last[:i], last[i+1:]
		if rt.segs[len(rt.segs)-1] == "" { // "/v1/roles:queryGrantableRoles"
			rt.segs = rt.segs[:len(rt.segs)-1]
		}
	}
	return rt
}

// match reports whether the route has the given shape; "*" matches any
// single segment. Captured values are returned in order.
func (rt route) match(verb string, pattern ...string) ([]string, bool) {
	if rt.verb != verb || len(rt.segs) != len(pattern) {
		return nil, false
	}
	var caps []string
	for i, p := range pattern {
		if p == "*" {
			caps = append(caps, rt.segs[i])
			continue
		}
		if p != rt.segs[i] {
			return nil, false
		}
	}
	return caps, true
}

// notFoundRoute writes the error GCP returns for an unknown URL.
func notFoundRoute(w http.ResponseWriter, r *http.Request) {
	apierr.Write(w, apierr.NotFound("The requested URL %s was not found on this server.", r.URL.Path))
}

// pageParams parses pageSize/pageToken. The token is a decimal offset.
func pageParams(r *http.Request, def, maxSize int) (size, offset int, err error) {
	q := r.URL.Query()
	size = def
	if v := q.Get("pageSize"); v != "" {
		if size, err = strconv.Atoi(v); err != nil || size < 0 {
			return 0, 0, apierr.InvalidArgument("Invalid pageSize %q.", v)
		}
		if size == 0 {
			size = def
		}
	}
	if size > maxSize {
		size = maxSize
	}
	if v := q.Get("pageToken"); v != "" {
		if offset, err = strconv.Atoi(v); err != nil || offset < 0 {
			return 0, 0, apierr.InvalidArgument("Invalid pageToken %q.", v)
		}
	}
	return size, offset, nil
}

// page slices items and returns the next page token ("" at the end).
func page[T any](items []T, size, offset int) ([]T, string) {
	if offset >= len(items) {
		return nil, ""
	}
	end := min(offset+size, len(items))
	next := ""
	if end < len(items) {
		next = strconv.Itoa(end)
	}
	return items[offset:end], next
}
