package link

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// StoreFile holds a Persistent Instance's Links in its instance
// directory. They are configuration, not state, so Reset and State
// snapshots leave them alone (ADR 0027).
const StoreFile = "links.json"

// Link is what one side of a Pairing keeps of the other: the other
// Instance and the Instance CA it pinned.
type Link struct {
	// Name is the link's name on this side, unique within the Instance.
	Name string `json:"name"`
	// Instance and InstanceID are the other Instance's name and ID.
	Instance   string `json:"instance"`
	InstanceID string `json:"instanceId"`
	// Address is the other Instance's --link-bind address, "" when it has
	// none and only connects to this one.
	Address string `json:"address,omitempty"`
	// Fingerprint and CA are the other Instance's pinned Instance CA.
	Fingerprint string `json:"fingerprint"`
	CA          string `json:"ca"`
	// LocalFingerprint is this Instance's CA at Pairing; once it no longer
	// is, `gcpemu ca rotate` has broken the Link.
	LocalFingerprint string `json:"localFingerprint"`
	// Version is the link protocol version negotiated at Pairing.
	Version  int       `json:"version"`
	PairedAt time.Time `json:"pairedAt"`
	// Broken is what the latest dial over the Link found wrong with the
	// other Instance's CA, at BrokenAt; "" once a dial finds it fine.
	Broken   string    `json:"broken,omitempty"`
	BrokenAt time.Time `json:"brokenAt,omitzero"`
}

// Store keeps an Instance's Links in a file, or in memory only.
type Store struct {
	path string

	mu       sync.Mutex
	pairings []Link
}

// OpenStore opens the Links in the file at path; a missing file has
// none. An empty path keeps them in memory, as an Ephemeral Instance does.
func OpenStore(path string) (*Store, error) {
	s := &Store{path: path}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.pairings); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// List returns the Links by name.
func (s *Store) List() []Link {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Link(nil), s.pairings...)
}

// Update replaces the Links with what change makes of them, and saves
// them; an error from change changes nothing.
func (s *Store) Update(change func([]Link) ([]Link, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next, err := change(append([]Link(nil), s.pairings...))
	if err != nil {
		return err
	}
	sort.Slice(next, func(i, j int) bool { return next[i].Name < next[j].Name })
	if s.path != "" {
		b, _ := json.MarshalIndent(next, "", "  ")
		if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
			return err
		}
		tmp := s.path + ".tmp"
		if err := os.WriteFile(tmp, b, 0o600); err != nil {
			return err
		}
		if err := os.Rename(tmp, s.path); err != nil {
			return err
		}
	}
	s.pairings = next
	return nil
}
