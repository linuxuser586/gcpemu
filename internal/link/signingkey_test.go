package link_test

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/link"
)

// servedKey is the signing key a test's Instance serves, which the
// test changes as an Ephemeral Instance's restart would.
type servedKey struct {
	mu  sync.Mutex
	key link.SigningKey
}

func (s *servedKey) set(kid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.key = link.SigningKey{KeyID: kid, JWK: map[string]string{"kty": "RSA", "kid": kid, "n": "n-" + kid, "e": "AQAB"}, CertPEM: "cert-" + kid}
}

func (s *servedKey) get() (link.SigningKey, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.key, s.key.KeyID != ""
}

func signing(s *servedKey) func(*link.Config) {
	return func(c *link.Config) { c.SigningKey = s.get }
}

// keyIDs returns the key IDs of the signing keys n remembers.
func keyIDs(n *linked) []string {
	out := []string{}
	for _, k := range n.LinkedSigningKeys() {
		out = append(out, k.KeyID)
	}
	return out
}

// waitKeyIDs waits until n remembers the signing keys want.
func waitKeyIDs(t *testing.T, n *linked, want ...string) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if slices.Equal(keyIDs(n), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("signing keys = %v, want %v", keyIDs(n), want)
}

// delegating pairs a watching laptop with an office that serves the
// signing key of key and shares team-shared, which the laptop delegates.
func delegating(t *testing.T, key *servedKey) (laptop, office *linked) {
	t.Helper()
	laptop, office = newLinked(t, "laptop", ""), newLinked(t, "office", "", signing(key))
	pair(t, laptop, office)
	laptop.StartWatching()
	if err := office.Share("team-shared", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := laptop.Delegate(ctxT(t), "team-shared", "office"); err != nil {
		t.Fatal(err)
	}
	return laptop, office
}

// A front Instance remembers the ID token signing key of each Linked
// Instance it delegates a Project to from when Delegate returns, and
// forgets it once no Project is delegated to that link (#693).
func TestDelegatedLinkSigningKey(t *testing.T) {
	key := &servedKey{}
	key.set("office-key")
	laptop, _ := delegating(t, key)
	if got := keyIDs(laptop); !slices.Equal(got, []string{"office-key"}) {
		t.Fatalf("signing keys right after delegating = %v, want [office-key]", got)
	}
	k := laptop.LinkedSigningKeys()[0]
	if k.JWK["n"] != "n-office-key" || k.CertPEM != "cert-office-key" {
		t.Errorf("signing key = %+v, want the office's JWK and certificate", k)
	}
	if err := laptop.Undelegate("team-shared"); err != nil {
		t.Fatal(err)
	}
	waitKeyIDs(t, laptop)
}

// While the Linked Instance cannot be reached, the front Instance keeps
// the signing key it last sent; once the link reconnects, it has the key
// the Linked Instance has now, as after an Ephemeral Instance's restart.
func TestLinkedSigningKeyKeptAndRefreshed(t *testing.T) {
	key := &servedKey{}
	key.set("first-key")
	laptop, office := delegating(t, key)
	if err := office.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // the watch reconnects, and fails
	if got := keyIDs(laptop); !slices.Equal(got, []string{"first-key"}) {
		t.Fatalf("signing keys while the office is down = %v, want [first-key] kept", got)
	}
	key.set("second-key")
	newLinkedAt(t, "office", office.dir, office.addr, signing(key))
	waitKeyIDs(t, laptop, "second-key")
}

// Unpairing a link forgets its signing key.
func TestUnpairForgetsSigningKey(t *testing.T) {
	key := &servedKey{}
	key.set("office-key")
	laptop, _ := delegating(t, key)
	if err := laptop.Unpair("office"); err != nil {
		t.Fatal(err)
	}
	waitKeyIDs(t, laptop)
}

// A front Instance that never reached the Linked Instance has no key for
// it.
func TestUnreachedLinkHasNoSigningKey(t *testing.T) {
	key := &servedKey{}
	key.set("office-key")
	laptop, office := delegating(t, key)
	if err := office.Close(); err != nil {
		t.Fatal(err)
	}
	if err := laptop.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := newLinked(t, "laptop", laptop.dir)
	restarted.StartWatching()
	time.Sleep(100 * time.Millisecond)
	if got := keyIDs(restarted); len(got) != 0 {
		t.Errorf("signing keys of a link never reached = %v, want none", got)
	}
}

// A Linked Instance that answers 404 for its signing key, as one from
// before the signing key route does, has none.
func TestLinkWithoutSigningKeyRouteHasNone(t *testing.T) {
	key := &servedKey{}
	key.set("office-key")
	laptop, office := delegating(t, key)
	if err := office.Close(); err != nil {
		t.Fatal(err)
	}
	newLinkedAt(t, "office", office.dir, office.addr)
	waitKeyIDs(t, laptop)
}

// Only a paired Instance gets an Instance's signing key.
func TestSigningKeyRefusedToUnpaired(t *testing.T) {
	key := &servedKey{}
	key.set("office-key")
	office, stranger := newLinked(t, "office", "", signing(key)), newLinked(t, "stranger", "")
	id, err := stranger.Identify(ctxT(t), office.addr)
	if err != nil {
		t.Fatal(err)
	}
	// The stranger pins the office's CA on its own, without a Pairing.
	if err := stranger.store.Update(func(ps []link.Link) ([]link.Link, error) {
		return append(ps, link.Link{Name: "office", InstanceID: id.ID, Address: office.addr, Fingerprint: id.Fingerprint,
			LocalFingerprint: link.Fingerprint(stranger.ca.Certificate()), Version: 1}), nil
	}); err != nil {
		t.Fatal(err)
	}
	k, err := stranger.FetchSigningKey(ctxT(t), "office")
	refused(t, "the signing key asked by an unpaired Instance", err, link.ReasonUnpaired)
	if k.KeyID != "" {
		t.Errorf("an unpaired Instance got the signing key %+v", k)
	}
}
