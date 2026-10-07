// Package project implements project identity (FR-CORE-020): any project ID
// is accepted, and its number is derived deterministically from the ID.
package project

import (
	"crypto/sha256"
	"encoding/binary"
	"regexp"
	"strconv"
)

var idRE = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)

// ValidID reports whether id is a syntactically valid GCP project ID.
// Legacy domain-scoped IDs ("example.com:proj") are accepted too.
func ValidID(id string) bool {
	if idRE.MatchString(id) {
		return true
	}
	for i := 0; i < len(id); i++ {
		if id[i] == ':' {
			return idRE.MatchString(id[i+1:])
		}
	}
	return false
}

// Number derives a stable 12-digit project number from the ID.
func Number(id string) int64 {
	h := sha256.Sum256([]byte("gcpemu-project:" + id))
	n := binary.BigEndian.Uint64(h[:8]) % 900_000_000_000
	return int64(n) + 100_000_000_000
}

// NumberString is Number formatted as a decimal string.
func NumberString(id string) string { return strconv.FormatInt(Number(id), 10) }
