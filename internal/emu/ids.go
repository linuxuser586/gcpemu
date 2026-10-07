package emu

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sync/atomic"
)

// IDs generates resource IDs. In deterministic mode IDs come from a counter
// so golden-file tests are reproducible (FR-CORE-022, NFR-REL-002).
type IDs struct {
	deterministic bool
	n             atomic.Uint64
}

// NewIDs returns an ID generator.
func NewIDs(deterministic bool) *IDs { return &IDs{deterministic: deterministic} }

// Uint64 returns a numeric ID in the 19-digit range GCP uses.
func (g *IDs) Uint64() uint64 {
	if g.deterministic {
		return 1_000_000_000_000_000_000 + g.n.Add(1)
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	return 1_000_000_000_000_000_000 + binary.BigEndian.Uint64(b[:])%8_000_000_000_000_000_000
}

// Numeric returns Uint64 as a decimal string.
func (g *IDs) Numeric() string { return fmt.Sprintf("%d", g.Uint64()) }

// Hex returns a random (or counter-derived) hex string of n bytes.
func (g *IDs) Hex(n int) string {
	b := make([]byte, n)
	if g.deterministic {
		var c [8]byte
		binary.BigEndian.PutUint64(c[:], g.n.Add(1))
		copy(b[max(0, n-8):], c[max(0, 8-n):])
	} else {
		_, _ = rand.Read(b)
	}
	return hex.EncodeToString(b)
}

// Operation returns an operation name suffix like "operation-1700000000000-5f1a...".
func (g *IDs) Operation() string {
	return "operation-" + g.Hex(16)
}
