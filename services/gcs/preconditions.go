package gcs

import (
	"net/http"
	"net/url"
	"strconv"

	storage "google.golang.org/api/storage/v1"
)

// conds holds generation/metageneration preconditions (FR-GCS-003).
type conds struct {
	GenMatch        *int64 `json:"genMatch,omitempty"`
	GenNotMatch     *int64 `json:"genNotMatch,omitempty"`
	MetagenMatch    *int64 `json:"metagenMatch,omitempty"`
	MetagenNotMatch *int64 `json:"metagenNotMatch,omitempty"`
}

func (c conds) empty() bool {
	return c.GenMatch == nil && c.GenNotMatch == nil && c.MetagenMatch == nil && c.MetagenNotMatch == nil
}

// parseConds reads ifGenerationMatch etc. (with an optional prefix such as
// "ifSource") from query parameters.
func parseConds(q url.Values, prefix string) (conds, error) {
	var c conds
	if prefix == "" {
		prefix = "if"
	}
	for _, f := range []struct {
		name string
		dst  **int64
	}{
		{prefix + "GenerationMatch", &c.GenMatch},
		{prefix + "GenerationNotMatch", &c.GenNotMatch},
		{prefix + "MetagenerationMatch", &c.MetagenMatch},
		{prefix + "MetagenerationNotMatch", &c.MetagenNotMatch},
	} {
		v := q.Get(f.name)
		if v == "" {
			continue
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return c, errInvalid("Invalid argument for %s: %q", f.name, v)
		}
		*f.dst = &n
	}
	return c, nil
}

// parseXMLConds reads x-goog-if-generation-match and
// x-goog-if-metageneration-match headers (XML API and XML-path reads).
func parseXMLConds(h http.Header) (conds, error) {
	var c conds
	for _, f := range []struct {
		name string
		dst  **int64
	}{
		{"X-Goog-If-Generation-Match", &c.GenMatch},
		{"X-Goog-If-Metageneration-Match", &c.MetagenMatch},
	} {
		v := h.Get(f.name)
		if v == "" {
			continue
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return c, errInvalid("Invalid argument for %s: %q", f.name, v)
		}
		*f.dst = &n
	}
	return c, nil
}

// check evaluates the preconditions against cur (nil when the object does
// not exist). For reads, a failed *NotMatch condition is reported as 304
// Not Modified like GCS does.
func (c conds) check(cur *storage.Object, read bool) error {
	if c.GenMatch != nil {
		if cur == nil {
			if *c.GenMatch != 0 {
				return errPrecondition()
			}
		} else if cur.Generation != *c.GenMatch {
			return errPrecondition()
		}
	}
	if c.MetagenMatch != nil && (cur == nil || cur.Metageneration != *c.MetagenMatch) {
		return errPrecondition()
	}
	notMatchErr := errPrecondition
	if read {
		notMatchErr = errNotModified
	}
	if c.GenNotMatch != nil {
		if cur == nil {
			if *c.GenNotMatch == 0 {
				return errPrecondition()
			}
		} else if cur.Generation == *c.GenNotMatch {
			return notMatchErr()
		}
	}
	if c.MetagenNotMatch != nil && cur != nil && cur.Metageneration == *c.MetagenNotMatch {
		return notMatchErr()
	}
	return nil
}

// checkBucket evaluates metageneration preconditions on a bucket.
func (c conds) checkBucket(b *storage.Bucket) error {
	if c.MetagenMatch != nil && b.Metageneration != *c.MetagenMatch {
		return errPrecondition()
	}
	if c.MetagenNotMatch != nil && b.Metageneration == *c.MetagenNotMatch {
		return errPrecondition()
	}
	return nil
}
