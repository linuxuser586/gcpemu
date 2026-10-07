package gcs

import "encoding/json"

// SetRewriteChunk overrides the default bytes-per-rewrite-call for tests.
func SetRewriteChunk(n int64) func() {
	old := defaultRewriteChunk
	defaultRewriteChunk = n
	return func() { defaultRewriteChunk = old }
}

type jsonRaw = json.RawMessage
