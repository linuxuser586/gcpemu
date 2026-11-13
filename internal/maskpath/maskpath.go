// Package maskpath parses one update_mask path into its field segment and,
// for a path into a map field, the key it names (AIP-161, ADR 0075).
//
// Only the parsing is shared. Each Service checks the field and merges the
// key into its own resource, since Services store resources differently.
package maskpath

import (
	"strings"
	"unicode"
)

// Path is one update_mask path.
type Path struct {
	// Field is the first segment, as written: snake_case or lowerCamel.
	Field string
	// Key is the map key after the field, exactly as written, without
	// backticks. It is set only when HasKey is true.
	Key    string
	HasKey bool
}

// Parse splits p at its first dot. The rest is a map key: bare, or
// backtick-quoted so it may hold dots. Parse reports false for an empty
// field or key, an unbalanced quote, or segments after the key; the
// returned Path still holds the field segment then.
func Parse(p string) (Path, bool) {
	field, rest, hasKey := strings.Cut(p, ".")
	path := Path{Field: field}
	if field == "" {
		return path, false
	}
	if !hasKey {
		return path, true
	}
	key, ok := mapKey(rest)
	if !ok {
		return path, false
	}
	path.Key, path.HasKey = key, true
	return path, true
}

// mapKey returns the key s names: a bare key, or a backtick-quoted one
// that may hold dots. It reports false for an empty key or one with more
// segments.
func mapKey(s string) (string, bool) {
	if q, ok := strings.CutPrefix(s, "`"); ok {
		k, ok := strings.CutSuffix(q, "`")
		return k, ok && k != "" && !strings.Contains(k, "`")
	}
	return s, s != "" && !strings.ContainsAny(s, ".`")
}

// SnakeCase converts a lowerCamel field name to snake_case. A snake_case
// name is returned unchanged.
func SnakeCase(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsUpper(r) {
			b.WriteByte('_')
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// LowerCamel converts a snake_case field name to lowerCamel. A lowerCamel
// name is returned unchanged.
func LowerCamel(s string) string {
	var b strings.Builder
	up := false
	for _, r := range s {
		if r == '_' {
			up = true
			continue
		}
		if up {
			r, up = unicode.ToUpper(r), false
		}
		b.WriteRune(r)
	}
	return b.String()
}
