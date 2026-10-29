package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// report is the body of every response that can have one (not 204 or
// 304): the request as received, with only the headers named in
// ECHO_HEADERS. Path is the request URI, query included. A header the
// request didn't carry is null. Encoding sorts the headers' keys, so
// identical requests get identical bytes.
type report struct {
	Method  string             `json:"method"`
	Path    string             `json:"path"`
	Headers map[string]*string `json:"headers"`
}

// newHandler answers /status/<code> with that status, any other path with
// 200, and a code that isn't 200 to 599 written plainly with 400. It
// reports the headers in echoHeaders, a comma-separated list of names.
// Go's server keeps Host out of the request's headers, so it is read from
// the request itself.
func newHandler(echoHeaders string) http.Handler {
	var names []string
	for n := range strings.SplitSeq(echoHeaders, ",") {
		if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
			names = append(names, n)
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code := http.StatusOK
		if s, ok := strings.CutPrefix(r.URL.Path, "/status/"); ok {
			code = statusOf(s)
		}
		rep := report{Method: r.Method, Path: r.URL.RequestURI(), Headers: map[string]*string{}}
		for _, n := range names {
			rep.Headers[n] = nil
			vs := r.Header.Values(n)
			if n == "host" && r.Host != "" {
				vs = []string{r.Host}
			}
			if len(vs) > 0 {
				v := strings.Join(vs, ", ")
				rep.Headers[n] = &v
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(rep)
	})
}

// statusOf is the status that /status/<s> asks for, or 400.
func statusOf(s string) int {
	code, err := strconv.Atoi(s)
	if err != nil || strconv.Itoa(code) != s || code < 200 || code > 599 {
		return http.StatusBadRequest
	}
	return code
}
