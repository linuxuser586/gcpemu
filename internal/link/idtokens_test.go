package link_test

import (
	"testing"

	"github.com/linuxuser586/gcpemu/internal/link"
)

// verifying has an Instance say that every ID token it is asked about is
// for the service account email.
func verifying(email string) func(*link.Config) {
	return func(c *link.Config) {
		c.VerifyIDToken = func(string) (string, bool) { return email, true }
	}
}

// The office tells the laptop which service account an ID token it signed
// is for, of a Project shared with the laptop only, and the laptop takes
// the answer for a Project delegated to the office only (#702).
func TestLinkedIDToken(t *testing.T) {
	laptop, office := newLinked(t, "laptop", ""), newLinked(t, "office", "", verifying("runner@team-shared.iam.gserviceaccount.com"))
	pair(t, laptop, office)
	if err := office.Share("team-shared", ""); err != nil {
		t.Fatal(err)
	}
	if p, unreachable := laptop.LinkedIDToken(ctxT(t), "tok"); p != "" || len(unreachable) != 0 {
		t.Errorf("LinkedIDToken with nothing delegated = %q %v, want none", p, unreachable)
	}
	if _, err := laptop.Delegate(ctxT(t), "team-shared", "office"); err != nil {
		t.Fatal(err)
	}
	if p, unreachable := laptop.LinkedIDToken(ctxT(t), "tok"); p != "serviceAccount:runner@team-shared.iam.gserviceaccount.com" || len(unreachable) != 0 {
		t.Errorf("LinkedIDToken = %q %v, want the office's service account", p, unreachable)
	}
	if err := office.Unshare("team-shared", ""); err != nil {
		t.Fatal(err)
	}
	if p, err := laptop.AskIDToken(ctxT(t), "office", "tok"); p != "" || err != nil {
		t.Errorf("the office's answer for a Project it no longer shares = %q %v, want none", p, err)
	}
}

// A Linked Instance that predates the route, answering 404, does not
// claim the ID token, and was reached; one that cannot be reached is named
// (#702).
func TestLinkedIDTokenFromOlderOrDownInstance(t *testing.T) {
	laptop, office := delegating(t, &servedKey{})
	link.AskIDTokensAt(t, "/link/v1/no-such-route")
	if p, unreachable := laptop.LinkedIDToken(ctxT(t), "tok"); p != "" || len(unreachable) != 0 {
		t.Errorf("LinkedIDToken of an Instance answering 404 = %q %v, want none, all reached", p, unreachable)
	}
	if err := office.Close(); err != nil {
		t.Fatal(err)
	}
	if p, unreachable := laptop.LinkedIDToken(ctxT(t), "tok"); p != "" || len(unreachable) != 1 || unreachable[0] != "office" {
		t.Errorf("LinkedIDToken with the office down = %q %v, want none, the office unreachable", p, unreachable)
	}
}

// An Instance the office never paired with is refused an answer about an
// ID token (#702).
func TestUnpairedInstanceIsRefusedIDTokens(t *testing.T) {
	office := newLinked(t, "office", "", verifying("runner@team-shared.iam.gserviceaccount.com"))
	stranger := newLinked(t, "stranger", "")
	id, err := stranger.Identify(ctxT(t), office.addr)
	if err != nil {
		t.Fatal(err)
	}
	if err := office.Share("team-shared", ""); err != nil {
		t.Fatal(err)
	}
	// The stranger pins the office's CA on its own, without a Pairing.
	if err := stranger.store.Update(func(ps []link.Link) ([]link.Link, error) {
		return append(ps, link.Link{Name: "office", InstanceID: id.ID, Address: office.addr, Fingerprint: id.Fingerprint,
			LocalFingerprint: link.Fingerprint(stranger.ca.Certificate()), Version: 1}), nil
	}); err != nil {
		t.Fatal(err)
	}
	p, err := stranger.AskIDToken(ctxT(t), "office", "tok")
	if p != "" {
		t.Errorf("the office told an unpaired Instance %q", p)
	}
	refused(t, "asking about an ID token unpaired", err, link.ReasonUnpaired)
}

// The laptop refuses another Linked Instance's answer for a service
// account of a Project delegated to the office, even when that Instance
// shares the Project too (#702).
func TestLinkedIDTokenFromAnotherLink(t *testing.T) {
	laptop, office := newLinked(t, "laptop", ""), newLinked(t, "office", "")
	other := newLinked(t, "other", "", verifying("runner@team-shared.iam.gserviceaccount.com"))
	pair(t, laptop, office)
	pair(t, laptop, other)
	for n, ids := range map[*linked][]string{office: {"team-shared"}, other: {"team-shared", "other-own"}} {
		for _, id := range ids {
			if err := n.Share(id, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := laptop.Delegate(ctxT(t), "team-shared", "office"); err != nil {
		t.Fatal(err)
	}
	if _, err := laptop.Delegate(ctxT(t), "other-own", "other"); err != nil {
		t.Fatal(err)
	}
	if p, err := laptop.AskIDToken(ctxT(t), "other", "tok"); p == "" || err != nil {
		t.Fatalf("other's own answer = %q %v, want it to claim the token", p, err)
	}
	if p, unreachable := laptop.LinkedIDToken(ctxT(t), "tok"); p != "" || len(unreachable) != 0 {
		t.Errorf("LinkedIDToken = %q %v, want other's answer refused, all reached", p, unreachable)
	}
}
