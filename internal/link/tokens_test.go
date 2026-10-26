package link_test

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/link"
)

const runner = "serviceAccount:runner@team-shared.iam.gserviceaccount.com"

// issuing has an Instance say that the access token "tok" is runner's.
func issuing(c *link.Config) {
	c.Authenticate = func(_ context.Context, token string) (string, bool) { return runner, token == "tok" }
}

// tokenDelegating pairs a laptop, configured by opts, with an office that
// issues runner's access tokens and shares team-shared, which the laptop
// delegates.
func tokenDelegating(t *testing.T, opts ...func(*link.Config)) (laptop, office *linked) {
	t.Helper()
	laptop, office = newLinked(t, "laptop", "", opts...), newLinked(t, "office", "", issuing)
	pair(t, laptop, office)
	if err := office.Share("team-shared", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := laptop.Delegate(ctxT(t), "team-shared", "office"); err != nil {
		t.Fatal(err)
	}
	return laptop, office
}

// setLink changes the laptop's link named name.
func setLink(t *testing.T, laptop *linked, name string, change func(*link.Link)) {
	t.Helper()
	if err := laptop.store.Update(func(ls []link.Link) ([]link.Link, error) {
		for i := range ls {
			if ls[i].Name == name {
				change(&ls[i])
			}
		}
		return ls, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A broken link is unreachable, named with why, and the answer is not
// kept: once the link is fixed, the token works at once (#733).
func TestAuthenticateWithBrokenLink(t *testing.T) {
	laptop, _ := tokenDelegating(t)
	setLink(t, laptop, "office", func(l *link.Link) { l.Broken = "an earlier dial found another CA" })
	p, unreachable := laptop.Authenticate(ctxT(t), "tok")
	if err := unreachable["office"]; p != "" || len(unreachable) != 1 || err == nil || !strings.Contains(err.Error(), "an earlier dial found another CA") {
		t.Errorf("Authenticate over a broken link = %q %v, want none, the office unreachable saying why", p, unreachable)
	}
	setLink(t, laptop, "office", func(l *link.Link) { l.Broken = "" })
	if p, unreachable := laptop.Authenticate(ctxT(t), "tok"); p != runner || len(unreachable) != 0 {
		t.Errorf("Authenticate once the link is fixed = %q %v, want %s", p, unreachable, runner)
	}
}

// A link with no --link-bind address, and one whose Linked Instance does
// not answer, are unreachable too, and why is logged, as a failed call is;
// once the link is back, the token works at once (#733).
func TestAuthenticateWithUnusableLink(t *testing.T) {
	logs := &syncBuffer{}
	debug := func(c *link.Config) {
		c.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	laptop, office := tokenDelegating(t, debug)

	setLink(t, laptop, "office", func(l *link.Link) { l.Address = "" })
	p, unreachable := laptop.Authenticate(ctxT(t), "tok")
	if err := unreachable["office"]; p != "" || len(unreachable) != 1 || err == nil || !strings.Contains(err.Error(), "--link-bind") {
		t.Errorf("Authenticate over a link with no address = %q %v, want none, the office unreachable saying why", p, unreachable)
	}
	if !strings.Contains(logs.String(), "--link-bind") {
		t.Errorf("the reason the link cannot be used was not logged: %s", logs)
	}
	setLink(t, laptop, "office", func(l *link.Link) { l.Address = office.addr })

	if err := office.Close(); err != nil {
		t.Fatal(err)
	}
	if p, unreachable := laptop.Authenticate(ctxT(t), "tok"); p != "" || len(unreachable) != 1 || !strings.Contains(fmt.Sprint(unreachable["office"]), `link "office" does not answer`) {
		t.Errorf("Authenticate with the office down = %q %v, want none, the office unreachable", p, unreachable)
	}
	newLinkedAt(t, "office", office.dir, office.addr, issuing)
	if p, unreachable := laptop.Authenticate(ctxT(t), "tok"); p != runner || len(unreachable) != 0 {
		t.Errorf("Authenticate once the office is back = %q %v, want %s", p, unreachable, runner)
	}
}

// A Linked Instance that claims the token is believed whatever the state
// of the others (#733).
func TestAuthenticateClaimedPastUnreachableLink(t *testing.T) {
	laptop, _ := tokenDelegating(t)
	branch := newLinked(t, "branch", "")
	pair(t, laptop, branch)
	if err := branch.Share("branch-own", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := laptop.Delegate(ctxT(t), "branch-own", "branch"); err != nil {
		t.Fatal(err)
	}
	setLink(t, laptop, "branch", func(l *link.Link) { l.Broken = "an earlier dial found another CA" })
	if p, unreachable := laptop.Authenticate(ctxT(t), "tok"); p != runner || len(unreachable) != 0 {
		t.Errorf("Authenticate with the branch broken = %q %v, want the office's %s", p, unreachable, runner)
	}
}

// When every link is reached and none claims the token, that answer is
// kept, so the token stays unknown while the office is down (#733).
func TestAuthenticateKeepsAnswerOfEveryLink(t *testing.T) {
	laptop, office := tokenDelegating(t)
	if p, unreachable := laptop.Authenticate(ctxT(t), "other"); p != "" || len(unreachable) != 0 {
		t.Errorf("Authenticate of a token no Instance issued = %q %v, want none, all reached", p, unreachable)
	}
	if err := office.Close(); err != nil {
		t.Fatal(err)
	}
	if p, unreachable := laptop.Authenticate(ctxT(t), "other"); p != "" || len(unreachable) != 0 {
		t.Errorf("Authenticate of the same token with the office down = %q %v, want the kept answer", p, unreachable)
	}
}
