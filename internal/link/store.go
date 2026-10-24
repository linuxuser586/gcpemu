package link

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"
)

// StoreFile holds a Persistent Instance's Links, Shares and Delegations
// in its instance directory. They are configuration, not state, so Reset and State
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

// Share is a Project an Instance shares with the link With, or with
// every link it has when With is "" (ADR 0027).
type Share struct {
	Project string `json:"project"`
	With    string `json:"with,omitempty"`
}

// Delegation is a Project the front Instance delegates to the link Link,
// which shares it.
type Delegation struct {
	Project string `json:"project"`
	Link    string `json:"link"`
}

// stored is the content of StoreFile.
type stored struct {
	Links     []Link       `json:"links"`
	Shared    []Share      `json:"shared,omitempty"`
	Delegated []Delegation `json:"delegated,omitempty"`
}

// Store keeps an Instance's Links, Shares and Delegations in a file, or in
// memory only.
type Store struct {
	path string

	mu sync.Mutex
	s  stored
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
	// Before Shares and Delegations, the file held the Links alone.
	if err := json.Unmarshal(b, &s.s.Links); err == nil {
		return s, nil
	}
	if err := json.Unmarshal(b, &s.s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// List returns the Links by name.
func (s *Store) List() []Link {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Link(nil), s.s.Links...)
}

// Shares returns the Shares by Project.
func (s *Store) Shares() []Share {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Share{}, s.s.Shared...)
}

// Delegations returns the Delegations by Project.
func (s *Store) Delegations() []Delegation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Delegation{}, s.s.Delegated...)
}

// Update replaces the Links with what change makes of them, and saves
// them; an error from change changes nothing.
func (s *Store) Update(change func([]Link) ([]Link, error)) error {
	return s.update(func(st *stored) error {
		ls, err := change(st.Links)
		st.Links = ls
		return err
	})
}

// update replaces what the Store holds with what change makes of a copy
// of it, and saves it; an error from change changes nothing. Shares and
// Delegations naming a link that no longer exists are dropped, so that a
// later link of the same name does not inherit them.
func (s *Store) update(change func(*stored) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := stored{
		Links:     append([]Link(nil), s.s.Links...),
		Shared:    append([]Share(nil), s.s.Shared...),
		Delegated: append([]Delegation(nil), s.s.Delegated...),
	}
	if err := change(&next); err != nil {
		return err
	}
	names := map[string]bool{}
	for _, l := range next.Links {
		names[l.Name] = true
	}
	next.Shared = slices.DeleteFunc(next.Shared, func(sh Share) bool { return sh.With != "" && !names[sh.With] })
	next.Delegated = slices.DeleteFunc(next.Delegated, func(d Delegation) bool { return !names[d.Link] })
	sort.Slice(next.Links, func(i, j int) bool { return next.Links[i].Name < next.Links[j].Name })
	sort.Slice(next.Shared, func(i, j int) bool {
		a, b := next.Shared[i], next.Shared[j]
		return a.Project < b.Project || a.Project == b.Project && a.With < b.With
	})
	sort.Slice(next.Delegated, func(i, j int) bool { return next.Delegated[i].Project < next.Delegated[j].Project })
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
	s.s = next
	return nil
}
