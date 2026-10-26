package link_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/link"
)

// serving has an Instance serve a forwarding rule web in each of projects,
// at port 8080 of the --link-bind address each request arrived on.
func serving(projects ...string) func(*link.Config) {
	return func(c *link.Config) {
		c.LBFrontends = func(lanIP string) []link.LBFrontend {
			var out []link.LBFrontend
			for _, p := range projects {
				out = append(out, link.LBFrontend{Name: "lb:" + p + "/us-central1/web", Project: p,
					What: "forwarding rule projects/" + p + "/regions/us-central1/forwardingRules/web", Address: lanIP + ":8080"})
			}
			return out
		}
	}
}

// endpointNames returns the names of the endpoints of each link in got.
func endpointNames(got []link.LinkFrontends) map[string][]string {
	out := map[string][]string{}
	for _, l := range got {
		out[l.Link] = []string{}
		for _, e := range l.Frontends {
			out[l.Link] = append(out[l.Link], e.Name)
		}
	}
	return out
}

// A front Instance learns the Load balancing frontends of the Projects it
// delegates, as the Linked Instance tells a LAN caller on the --link-bind
// address it arrived at, and never those of other Projects (#715).
func TestLinkedLBFrontends(t *testing.T) {
	laptop := newLinked(t, "laptop", "")
	office := newLinked(t, "office", "", serving("team-shared", "shared-only", "private"), func(c *link.Config) { c.LBBind = func() bool { return true } })
	pair(t, laptop, office)
	for _, id := range []string{"team-shared", "shared-only"} {
		if err := office.Share(id, ""); err != nil {
			t.Fatal(err)
		}
	}
	if got := laptop.LinkedLBFrontends(ctxT(t)); len(got) != 0 {
		t.Errorf("LinkedLBFrontends with nothing delegated = %+v, want none", got)
	}
	if _, err := laptop.Delegate(ctxT(t), "team-shared", "office"); err != nil {
		t.Fatal(err)
	}
	got := laptop.LinkedLBFrontends(ctxT(t))
	if len(got) != 1 || got[0].Link != "office" || got[0].Error != "" || !got[0].LBBind || len(got[0].Frontends) != 1 {
		t.Fatalf("LinkedLBFrontends = %+v, want the office's team-shared endpoint only, and its --lb-bind", got)
	}
	if e := got[0].Frontends[0]; e.Name != "lb:team-shared/us-central1/web" || e.Address != "127.0.0.1:8080" {
		t.Errorf("endpoint = %+v, want lb:team-shared/us-central1/web at the --link-bind address 127.0.0.1:8080", e)
	}

	// The office itself answers only for the Projects it shares.
	raw, err := laptop.AskLBFrontends(ctxT(t), "office")
	if err != nil {
		t.Fatal(err)
	}
	var projects []string
	for _, e := range raw {
		projects = append(projects, e.Project)
	}
	slices.Sort(projects)
	if !slices.Equal(projects, []string{"shared-only", "team-shared"}) {
		t.Errorf("the office answered for %v, want the shared Projects only", projects)
	}
}

// An unreachable Linked Instance is named, with no endpoints (#715).
func TestLinkedLBFrontendsWhileLinkedInstanceIsDown(t *testing.T) {
	laptop, office := newLinked(t, "laptop", ""), newLinked(t, "office", "", serving("team-shared"))
	pair(t, laptop, office)
	if err := office.Share("team-shared", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := laptop.Delegate(ctxT(t), "team-shared", "office"); err != nil {
		t.Fatal(err)
	}
	if err := office.Close(); err != nil {
		t.Fatal(err)
	}
	got := laptop.LinkedLBFrontends(ctxT(t))
	if len(got) != 1 || got[0].Link != "office" || got[0].Error == "" || len(got[0].Frontends) != 0 {
		t.Errorf("LinkedLBFrontends with the office down = %+v, want the office named, unreachable", got)
	}
	if names := endpointNames(got); len(names["office"]) != 0 {
		t.Errorf("endpoints of an unreachable office: %v", names)
	}
}

// An unpaired Instance is refused the endpoints (#715).
func TestUnpairedInstanceIsRefusedLBFrontends(t *testing.T) {
	office := newLinked(t, "office", "", serving("team-shared"))
	stranger := newLinked(t, "stranger", "")
	id, err := stranger.Identify(ctxT(t), office.addr)
	if err != nil {
		t.Fatal(err)
	}
	if err := office.Share("team-shared", ""); err != nil {
		t.Fatal(err)
	}
	if err := stranger.store.Update(func(ps []link.Link) ([]link.Link, error) {
		return append(ps, link.Link{Name: "office", InstanceID: id.ID, Address: office.addr, Fingerprint: id.Fingerprint,
			LocalFingerprint: link.Fingerprint(stranger.ca.Certificate()), Version: link.Current.Max}), nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := stranger.AskLBFrontends(ctxT(t), "office")
	if len(got) != 0 {
		t.Errorf("the office told an unpaired Instance %+v", got)
	}
	refused(t, "asking for Load balancing frontends unpaired", err, link.ReasonUnpaired)
}

// The Load balancing frontends came with version 2: an Instance that
// speaks only version 1 is refused, naming both (#715).
func TestVersion1IsRefused(t *testing.T) {
	if link.Current.Min < 2 {
		t.Fatalf("link.Current = %s, want version 1 no longer spoken", link.Current)
	}
	v1 := func(c *link.Config) { c.Versions = link.Versions{Min: 1, Max: 1} }
	laptop, old := newLinked(t, "laptop", ""), newLinked(t, "old", "", v1)
	id, err := laptop.Identify(ctxT(t), old.addr)
	if err != nil {
		t.Fatal(err)
	}
	_, err = laptop.Pair(ctxT(t), old.addr, id.Fingerprint, "")
	if err == nil || !strings.Contains(err.Error(), "version 1") || !strings.Contains(err.Error(), "version "+link.Current.String()) {
		t.Errorf("Pair with a version 1 Instance: %v", err)
	}
}
