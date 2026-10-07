package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Standard REST query parameters (Section 7.2). `fields` selects a partial
// response with Google's syntax: comma-separated selectors, "/" for
// nesting, "a(b,c)" for sub-selections, "*" for every field; selectors
// apply to every element of an array. `prettyPrint=false` returns compact
// JSON and `prettyPrint=true` indented JSON; without either the service's
// own encoding is kept.

// fieldMask is a parsed `fields` value: nil children select a whole field.
type fieldMask map[string]fieldMask

// parseFields parses a `fields` value.
func parseFields(s string) (fieldMask, error) {
	p := &fieldParser{s: s}
	m, err := p.list()
	if err != nil {
		return nil, err
	}
	if p.i != len(p.s) {
		return nil, fmt.Errorf("invalid fields %q: unexpected %q at %d", s, p.s[p.i], p.i)
	}
	return m, nil
}

type fieldParser struct {
	s string
	i int
}

// list parses "selector (',' selector)*".
func (p *fieldParser) list() (fieldMask, error) {
	m := fieldMask{}
	for {
		if err := p.selector(m); err != nil {
			return nil, err
		}
		if p.i < len(p.s) && p.s[p.i] == ',' {
			p.i++
			continue
		}
		return m, nil
	}
}

// selector parses "name ('/' name)* ['(' list ')']" into m.
func (p *fieldParser) selector(m fieldMask) error {
	cur := m
	for {
		name := p.name()
		if name == "" {
			return fmt.Errorf("invalid fields %q: missing field name at %d", p.s, p.i)
		}
		switch {
		case p.i < len(p.s) && p.s[p.i] == '/':
			p.i++
			next, ok := cur[name]
			if ok && next == nil {
				// The whole field is already selected: parse the rest of
				// the selector into a mask that is dropped.
				cur = fieldMask{}
				continue
			}
			if next == nil {
				next = fieldMask{}
				cur[name] = next
			}
			cur = next
		case p.i < len(p.s) && p.s[p.i] == '(':
			p.i++
			sub, err := p.list()
			if err != nil {
				return err
			}
			if p.i >= len(p.s) || p.s[p.i] != ')' {
				return fmt.Errorf("invalid fields %q: missing ')'", p.s)
			}
			p.i++
			if next, ok := cur[name]; !ok || next != nil {
				cur[name] = merge(next, sub)
			}
			return nil
		default:
			cur[name] = nil
			return nil
		}
	}
}

func (p *fieldParser) name() string {
	start := p.i
	for p.i < len(p.s) && !strings.ContainsRune(",/()", rune(p.s[p.i])) {
		p.i++
	}
	return strings.TrimSpace(p.s[start:p.i])
}

func merge(a, b fieldMask) fieldMask {
	if a == nil {
		return b
	}
	for k, v := range b {
		if cur, ok := a[k]; ok && (cur == nil || v == nil) {
			a[k] = nil
		} else {
			a[k] = merge(cur, v)
		}
	}
	return a
}

// apply returns v restricted to the mask.
func (m fieldMask) apply(v any) any {
	if m == nil {
		return v
	}
	switch t := v.(type) {
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = m.apply(e)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, sub := range m {
			if k == "*" {
				for kk, vv := range t {
					if _, explicit := m[kk]; !explicit {
						out[kk] = sub.apply(vv)
					}
				}
				continue
			}
			if vv, ok := t[k]; ok {
				out[k] = sub.apply(vv)
			}
		}
		return out
	default:
		return v
	}
}

// responseFormat is what the standard parameters ask of a JSON response.
type responseFormat struct {
	mask   fieldMask
	pretty *bool
}

// formatFor reads `fields` and `prettyPrint` from r; nil means neither is
// set (the response passes through untouched).
func formatFor(r *http.Request) (*responseFormat, error) {
	q := r.URL.Query()
	f := &responseFormat{}
	if v := q.Get("fields"); v != "" {
		m, err := parseFields(v)
		if err != nil {
			return nil, err
		}
		f.mask = m
	}
	if v := q.Get("prettyPrint"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("invalid value for prettyPrint: %q", v)
		}
		f.pretty = &b
	}
	if f.mask == nil && f.pretty == nil {
		return nil, nil
	}
	return f, nil
}

// formatWriter reshapes JSON responses per a responseFormat. It decides
// when the handler sends its header: JSON is buffered and rewritten on
// finish, anything else (media downloads, streams) passes straight
// through.
type formatWriter struct {
	w      http.ResponseWriter
	f      *responseFormat
	h      http.Header
	status int
	mode   int // 0 undecided, 1 buffering, 2 passthrough
	body   bytes.Buffer
}

func newFormatWriter(w http.ResponseWriter, f *responseFormat) *formatWriter {
	return &formatWriter{w: w, f: f, h: http.Header{}}
}

func (fw *formatWriter) Header() http.Header {
	if fw.mode == 2 {
		return fw.w.Header()
	}
	return fw.h
}

func (fw *formatWriter) WriteHeader(code int) {
	if fw.mode != 0 {
		if fw.mode == 2 {
			fw.w.WriteHeader(code)
		}
		return
	}
	fw.status = code
	if strings.HasPrefix(fw.h.Get("Content-Type"), "application/json") {
		fw.mode = 1
		return
	}
	fw.mode = 2
	for k, vs := range fw.h {
		fw.w.Header()[k] = vs
	}
	fw.w.WriteHeader(code)
}

func (fw *formatWriter) Write(p []byte) (int, error) {
	if fw.mode == 0 {
		if fw.h.Get("Content-Type") == "" {
			fw.h.Set("Content-Type", http.DetectContentType(p))
		}
		fw.WriteHeader(http.StatusOK)
	}
	if fw.mode == 2 {
		return fw.w.Write(p)
	}
	return fw.body.Write(p)
}

func (fw *formatWriter) Flush() {
	if fw.mode == 2 {
		if f, ok := fw.w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

// finish writes a buffered JSON response, reshaped. Fields filter
// successful responses only; errors keep their envelope.
func (fw *formatWriter) finish() {
	switch fw.mode {
	case 0:
		fw.WriteHeader(http.StatusOK)
		return
	case 2:
		return
	}
	body := fw.f.reshape(fw.body.Bytes(), fw.status)
	for k, vs := range fw.h {
		fw.w.Header()[k] = vs
	}
	fw.w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	fw.w.WriteHeader(fw.status)
	_, _ = fw.w.Write(body)
}

// reshape applies the field mask and the requested indentation to a JSON
// body; a body that isn't valid JSON is returned unchanged. Without
// prettyPrint, responses are indented as Google's default is.
func (f *responseFormat) reshape(body []byte, status int) []byte {
	if len(bytes.TrimSpace(body)) == 0 {
		return body
	}
	pretty := f.pretty == nil || *f.pretty
	var out bytes.Buffer
	if f.mask == nil || status >= 300 {
		var err error
		if pretty {
			err = json.Indent(&out, body, "", "  ")
		} else {
			err = json.Compact(&out, body)
		}
		if err != nil {
			return body
		}
		return append(bytes.TrimSpace(out.Bytes()), '\n')
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if dec.Decode(&v) != nil {
		return body
	}
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if pretty {
		enc.SetIndent("", "  ")
	}
	if enc.Encode(f.mask.apply(v)) != nil {
		return body
	}
	return out.Bytes()
}
