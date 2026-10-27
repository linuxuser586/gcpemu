//go:build conformance

package conformance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Recording is the file a test's responses are kept in:
// testdata/<product>/<TestName>.json.
type Recording struct {
	Exchanges []Exchange `json:"exchanges"`
}

// Exchange is one request and its normalised response, in the order the
// test made them.
type Exchange struct {
	Request  Request  `json:"request"`
	Response Response `json:"response"`
}

// Request is kept for context and readable Drift diffs; it is never
// compared. Method is "GRPC" for a gRPC call, whose Path is the full
// method name.
type Request struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Body   any    `json:"body,omitempty"`
}

// Response is an HTTP Status or a gRPC Code (its name, "OK" on success),
// and the body: decoded JSON, a gRPC message or google.rpc.Status in
// protojson, or a string for a body that is not JSON.
type Response struct {
	Status int    `json:"status,omitempty"`
	Code   string `json:"code,omitempty"`
	Body   any    `json:"body,omitempty"`
}

// failed reports whether r is an error: an HTTP status of 400 or more, or
// a gRPC code other than OK.
func (r Response) failed() bool { return r.Status >= 400 || r.Code != "" && r.Code != "OK" }

// marshal is r as written to its file: two-space indentation, keys in
// order, HTML characters unescaped and a trailing newline.
func (r Recording) marshal() ([]byte, error) {
	if r.Exchanges == nil {
		r.Exchanges = []Exchange{}
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// parseRecording reads a Recording file's bytes.
func parseRecording(b []byte) (Recording, error) {
	var r Recording
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&r); err != nil {
		return r, err
	}
	return r, nil
}

// readRecording reads the Recording at path.
func readRecording(path string) (Recording, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Recording{}, err
	}
	r, err := parseRecording(b)
	if err != nil {
		return r, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}

// writeRecording writes r to path, creating its directory.
func writeRecording(path string, r Recording) error {
	b, err := r.marshal()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// decodeJSON decodes one JSON value, keeping numbers as written.
func decodeJSON(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing data after JSON value")
	}
	return v, nil
}
