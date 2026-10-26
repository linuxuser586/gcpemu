package link_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/ca"
	"github.com/linuxuser586/gcpemu/internal/link"
)

// linked is a link.Manager serving on a loopback listener, with what a test
// needs to reach it.
type linked struct {
	*link.Manager
	addr  string
	ca    *ca.CA
	dir   string
	store *link.Store
	log   *syncBuffer
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// newLinked starts the link.Manager of an Instance named name, with its own Instance
// CA and pairings file in dir (a new temp dir when "").
func newLinked(t *testing.T, name, dir string, opts ...func(*link.Config)) *linked {
	t.Helper()
	return newLinkedOn(t, name, dir, "127.0.0.1", opts...)
}

// newLinkedOn is newLinked serving on ip.
func newLinkedOn(t *testing.T, name, dir, ip string, opts ...func(*link.Config)) *linked {
	t.Helper()
	return newLinkedAt(t, name, dir, net.JoinHostPort(ip, "0"), opts...)
}

// newLinkedAt is newLinked serving on the address addr.
func newLinkedAt(t *testing.T, name, dir, addr string, opts ...func(*link.Config)) *linked {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	authority, err := ca.Load(dir, name)
	if err != nil {
		t.Fatal(err)
	}
	store, err := link.OpenStore(filepath.Join(dir, link.StoreFile))
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	cfg := link.Config{CA: authority, Instance: name, ID: name + "-id", Store: store, Log: slog.New(slog.NewTextHandler(logs, nil))}
	for _, o := range opts {
		o(&cfg)
	}
	n := link.New(cfg)
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = n.Serve(l) }()
	t.Cleanup(func() { _ = n.Close() })
	return &linked{Manager: n, addr: l.Addr().String(), ca: authority, dir: dir, store: store, log: logs}
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// waitRequest waits until n has an incoming Pairing request and returns it.
func waitRequest(t *testing.T, n *linked) link.Request {
	t.Helper()
	for i := 0; i < 200; i++ {
		if reqs := n.Requests(); len(reqs) > 0 {
			return reqs[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no Pairing request arrived")
	return link.Request{}
}

type pairResult struct {
	p   link.Link
	err error
}

// pairAsync starts from pairing with to, as `gcpemu link pair` does once
// its user has approved to's identity.
func pairAsync(t *testing.T, from, to *linked) <-chan pairResult {
	t.Helper()
	id, err := from.Identify(ctxT(t), to.addr)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan pairResult, 1)
	go func() {
		p, err := from.Pair(ctxT(t), to.addr, id.Fingerprint, "")
		done <- pairResult{p, err}
	}()
	return done
}

// pair pairs a with b, approved on both sides.
func pair(t *testing.T, a, b *linked) {
	t.Helper()
	done := pairAsync(t, a, b)
	if _, err := b.Approve(waitRequest(t, b).ID, ""); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.err != nil {
		t.Fatal(r.err)
	}
}

func names(ps []link.Link) []string {
	out := []string{}
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out
}

// Two Instances pair only after both sides approve: the requesting side
// approves the other's identity before asking, and the other side approves
// the request. Each then lists the other, pinning its Instance CA.
func TestPairingIsApprovedOnBothSides(t *testing.T) {
	laptop, office := newLinked(t, "laptop", ""), newLinked(t, "office", "")

	id, err := laptop.Identify(ctxT(t), office.addr)
	if err != nil {
		t.Fatal(err)
	}
	if id.Name != "office" || id.ID != "office-id" || id.Fingerprint != link.Fingerprint(office.ca.Certificate()) || id.Version != link.Current.Max {
		t.Fatalf("Identify = %+v", id)
	}

	done := pairAsync(t, laptop, office)
	req := waitRequest(t, office)
	if req.Name != "laptop" || req.InstanceID != "laptop-id" || req.Fingerprint != link.Fingerprint(laptop.ca.Certificate()) {
		t.Errorf("request = %+v", req)
	}
	if len(laptop.Links()) != 0 || len(office.Links()) != 0 {
		t.Fatal("paired before the other side approved")
	}
	p, err := office.Approve(req.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "laptop" {
		t.Errorf("office's link = %+v", p)
	}
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.p.Name != "office" || r.p.InstanceID != "office-id" || r.p.Address != office.addr {
		t.Errorf("laptop's link = %+v", r.p)
	}
	if got := names(laptop.Links()); len(got) != 1 || got[0] != "office" {
		t.Errorf("laptop lists %v", got)
	}
	if got := names(office.Links()); len(got) != 1 || got[0] != "laptop" {
		t.Errorf("office lists %v", got)
	}
	if len(office.Requests()) != 0 {
		t.Errorf("request still pending after approval")
	}
	st := laptop.Check(ctxT(t), "office")
	if !st.Reachable || st.Version != link.Current.Max || st.Error != "" {
		t.Errorf("Check over the link = %+v", st)
	}
}

// refused asserts that err is a refusal by the other Instance for reason,
// whose message contains each of want.
func refused(t *testing.T, what string, err error, reason string, want ...string) {
	t.Helper()
	var ref *link.RefusedError
	if !errors.As(err, &ref) || ref.Reason != reason {
		t.Errorf("%s: got %v, want a %s refusal", what, err, reason)
		return
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("%s: %q does not name %q", what, err, w)
		}
	}
}

// A connection from an Instance that the other side never paired with is
// refused, saying so.
func TestUnpairedInstanceIsRefused(t *testing.T) {
	office, stranger := newLinked(t, "office", ""), newLinked(t, "stranger", "")
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
	st := stranger.Check(ctxT(t), "office")
	if st.Reachable || !strings.Contains(st.Error, "not paired") || !strings.Contains(st.Error, "stranger-id") {
		t.Errorf("unpaired Check = %+v", st)
	}
	if !strings.Contains(office.log.String(), "not paired") {
		t.Errorf("office does not report the refusal:\n%s", office.log)
	}
}

// Unpairing on either side removes the trust on that side at once.
func TestUnpairTakesEffectAtOnce(t *testing.T) {
	laptop, office := newLinked(t, "laptop", ""), newLinked(t, "office", "")
	pair(t, laptop, office)
	if err := office.Unpair("laptop"); err != nil {
		t.Fatal(err)
	}
	if len(office.Links()) != 0 {
		t.Errorf("office still lists %v", names(office.Links()))
	}
	if st := laptop.Check(ctxT(t), "office"); st.Reachable || !strings.Contains(st.Error, "not paired") {
		t.Errorf("Check after the other side unpaired = %+v", st)
	}
	if err := office.Unpair("laptop"); err == nil || !strings.Contains(err.Error(), `no link named "laptop"`) {
		t.Errorf("unpairing twice: %v", err)
	}
}

// A rejected Pairing request pins nothing on either side.
func TestRejectedPairing(t *testing.T) {
	laptop, office := newLinked(t, "laptop", ""), newLinked(t, "office", "")
	done := pairAsync(t, laptop, office)
	if err := office.Reject(waitRequest(t, office).ID); err != nil {
		t.Fatal(err)
	}
	refused(t, "rejected Pair", (<-done).err, link.ReasonRejected, `"office" rejected`)
	if len(laptop.Links())+len(office.Links()) != 0 {
		t.Errorf("rejected Pairing pinned %v and %v", names(laptop.Links()), names(office.Links()))
	}
}

// `gcpemu ca rotate` breaks an Instance's Pairings, and both sides report
// it, naming the link.
func TestRotatedCABreaksPairings(t *testing.T) {
	laptop, office := newLinked(t, "laptop", ""), newLinked(t, "office", "")
	pair(t, laptop, office)
	if err := laptop.ca.Rotate("laptop"); err != nil {
		t.Fatal(err)
	}

	// The rotating side knows without dialling.
	ls := laptop.States()
	if len(ls) != 1 || ls[0].State != "broken" || !strings.Contains(ls[0].Problem, `link "office"`) || !strings.Contains(ls[0].Problem, "rotated") {
		t.Errorf("laptop's links after rotating = %+v", ls)
	}
	if st := laptop.Check(ctxT(t), "office"); st.State != "broken" || st.Reachable {
		t.Errorf("laptop's Check = %+v", st)
	}
	// The other side refuses the new CA, naming the link, and sees it
	// when it dials too.
	if !strings.Contains(office.log.String(), `the Instance CA of link \"laptop\"`) {
		t.Errorf("office does not report the mismatch:\n%s", office.log)
	}
	if st := office.Check(ctxT(t), "laptop"); st.Reachable || !strings.Contains(st.Error, `the Instance CA of link "laptop" no longer matches`) {
		t.Errorf("office's Check = %+v", st)
	}
}

// Instances with no protocol version in common are refused at Pairing and
// on connect, naming both versions.
func TestIncompatibleVersionIsRefused(t *testing.T) {
	v2 := func(c *link.Config) { c.Versions = link.Versions{Min: 2, Max: 2} }
	laptop, office := newLinked(t, "laptop", ""), newLinked(t, "future", "", v2)
	id, err := laptop.Identify(ctxT(t), office.addr)
	if err != nil {
		t.Fatal(err)
	}
	_, err = laptop.Pair(ctxT(t), office.addr, id.Fingerprint, "")
	if err == nil || !strings.Contains(err.Error(), "version 1") || !strings.Contains(err.Error(), "version 2") {
		t.Errorf("Pair across versions: %v", err)
	}

	// Paired at version 1, then the office is upgraded past it.
	office = newLinked(t, "office", "")
	pair(t, laptop, office)
	upgraded := newLinked(t, "office", office.dir, v2)
	_ = office.Close()
	if err := laptop.store.Update(func(ps []link.Link) ([]link.Link, error) {
		ps[0].Address = upgraded.addr
		return ps, nil
	}); err != nil {
		t.Fatal(err)
	}
	st := laptop.Check(ctxT(t), "office")
	if st.Reachable || !strings.Contains(st.Error, "version 1") || !strings.Contains(st.Error, "version 2") {
		t.Errorf("Check across versions = %+v", st)
	}
}

// An Instance with no --link-bind connects in only; when its CA is
// rotated, the other side cannot check that itself, so it warns on the
// link, naming it, without calling it broken.
func TestRotationReportedOnInboundOnlyLink(t *testing.T) {
	laptop, office := newLinked(t, "laptop", ""), newLinked(t, "office", "")
	pair(t, laptop, office)
	// The laptop had no listener of its own: the office cannot dial it.
	if err := office.store.Update(func(ps []link.Link) ([]link.Link, error) {
		ps[0].Address = ""
		return ps, nil
	}); err != nil {
		t.Fatal(err)
	}
	if st := office.Check(ctxT(t), "laptop"); !st.InboundOnly || st.State != link.StatePaired {
		t.Errorf("inbound-only Check before rotating = %+v", st)
	}
	if err := laptop.ca.Rotate("laptop"); err != nil {
		t.Fatal(err)
	}
	laptop.Check(ctxT(t), "office")
	ls := office.States()
	if len(ls) != 1 || ls[0].State != link.StatePaired || !strings.Contains(ls[0].Warning, `link "laptop"`) ||
		!strings.Contains(ls[0].Warning, link.Fingerprint(laptop.ca.Certificate())) || !strings.Contains(ls[0].Warning, "unpair and pair again") {
		t.Errorf("office's links after the laptop rotated = %+v", ls)
	}
	if st := office.Check(ctxT(t), "laptop"); st.State != link.StatePaired || st.Warning == "" {
		t.Errorf("office's Check after the laptop rotated = %+v", st)
	}
}

// Approving a request from an Instance already paired under another CA
// replaces that Pairing, so the request says so before anyone approves it.
func TestRequestReplacingAPairingSaysSo(t *testing.T) {
	laptop, office := newLinked(t, "laptop", ""), newLinked(t, "office", "")
	pair(t, laptop, office)
	if err := laptop.ca.Rotate("laptop"); err != nil {
		t.Fatal(err)
	}
	done := pairAsync(t, laptop, office)
	req := waitRequest(t, office)
	if req.Replaces != "laptop" {
		t.Errorf("request = %+v, want it to name the link it replaces", req)
	}
	if _, err := office.Approve(req.ID, ""); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.err != nil {
		t.Fatal(r.err)
	}
	if ls := office.States(); len(ls) != 1 || ls[0].Fingerprint != link.Fingerprint(laptop.ca.Certificate()) || ls[0].State != link.StatePaired {
		t.Errorf("office's links after pairing again = %+v", ls)
	}
}

// The side that did not rotate its CA reports the link broken as soon as
// it dials the other, before the other has connected in.
func TestRotationReportedWhenDiallingOut(t *testing.T) {
	laptop, office := newLinked(t, "laptop", ""), newLinked(t, "office", "")
	pair(t, laptop, office)
	if err := office.ca.Rotate("office"); err != nil {
		t.Fatal(err)
	}
	st := laptop.Check(ctxT(t), "office")
	if st.State != link.StateBroken || st.Reachable || !strings.Contains(st.Error, `the Instance CA of link "office" no longer matches`) {
		t.Errorf("laptop's Check after the office rotated = %+v", st)
	}
	if ls := laptop.States(); len(ls) != 1 || ls[0].State != link.StateBroken {
		t.Errorf("laptop's links after dialling the rotated office = %+v", ls)
	}
	// What the dial found is kept: it is still broken after a restart.
	restarted := newLinked(t, "laptop", laptop.dir)
	if ls := restarted.States(); len(ls) != 1 || ls[0].State != link.StateBroken || !strings.Contains(ls[0].Problem, `link "office"`) {
		t.Errorf("laptop's links after a restart = %+v", ls)
	}
}

// A dial that finds the pinned CA again clears what an earlier dial found.
func TestGoodDialClearsBroken(t *testing.T) {
	laptop, office := newLinked(t, "laptop", ""), newLinked(t, "office", "")
	pair(t, laptop, office)
	if err := laptop.store.Update(func(ls []link.Link) ([]link.Link, error) {
		ls[0].Broken, ls[0].BrokenAt = "an earlier dial found another CA", time.Now()
		return ls, nil
	}); err != nil {
		t.Fatal(err)
	}
	if st := laptop.Check(ctxT(t), "office"); st.State != link.StatePaired || !st.Reachable {
		t.Errorf("Check of a link that is fine again = %+v", st)
	}
	if ls := laptop.Links(); ls[0].Broken != "" || !ls[0].BrokenAt.IsZero() {
		t.Errorf("link after a good dial = %+v", ls[0])
	}
}

// An Instance that merely claims a paired Instance's ID, with another CA,
// is refused and warned about, but cannot make the link broken; the real
// Instance connecting clears the warning.
func TestImpostorOnlyWarns(t *testing.T) {
	laptop, office := newLinked(t, "laptop", ""), newLinked(t, "office", "")
	pair(t, laptop, office)
	impostor := newLinked(t, "laptop", "") // the same Instance ID, its own CA
	if err := impostor.store.Update(func(ls []link.Link) ([]link.Link, error) {
		return append(ls, link.Link{Name: "office", InstanceID: "office-id", Address: office.addr, Fingerprint: link.Fingerprint(office.ca.Certificate()),
			LocalFingerprint: link.Fingerprint(impostor.ca.Certificate()), Version: 1}), nil
	}); err != nil {
		t.Fatal(err)
	}
	impostor.Check(ctxT(t), "office")
	ls := office.States()
	if len(ls) != 1 || ls[0].State != link.StatePaired || !strings.Contains(ls[0].Warning, link.Fingerprint(impostor.ca.Certificate())) {
		t.Fatalf("office's links after the impostor connected = %+v", ls)
	}
	if st := laptop.Check(ctxT(t), "office"); !st.Reachable {
		t.Fatalf("the real laptop's Check = %+v", st)
	}
	if ls := office.States(); ls[0].Warning != "" {
		t.Errorf("warning after the real laptop connected = %q", ls[0].Warning)
	}
}

// A newer Pairing request from the same Instance CA replaces the one still
// waiting, which is refused as superseded.
func TestNewerRequestSupersedes(t *testing.T) {
	laptop, office := newLinked(t, "laptop", ""), newLinked(t, "office", "")
	first := pairAsync(t, laptop, office)
	old := waitRequest(t, office)
	second := pairAsync(t, laptop, office)
	r := <-first
	refused(t, "the older request", r.err, link.ReasonSuperseded, "newer")
	reqs := office.Requests()
	if len(reqs) != 1 || reqs[0].ID == old.ID {
		t.Fatalf("requests after a newer one = %+v", reqs)
	}
	if _, err := office.Approve(reqs[0].ID, ""); err != nil {
		t.Fatal(err)
	}
	if r := <-second; r.err != nil {
		t.Fatal(r.err)
	}
}

// Approving under a link name already taken fails and leaves the request
// waiting, so that it can be approved under another name.
func TestApproveUnderTakenNameKeepsRequest(t *testing.T) {
	laptop, desktop, office := newLinked(t, "laptop", ""), newLinked(t, "desktop", ""), newLinked(t, "office", "")
	pair(t, laptop, office)
	done := pairAsync(t, desktop, office)
	req := waitRequest(t, office)
	if _, err := office.Approve(req.ID, "laptop"); err == nil {
		t.Fatal("approved under a taken name")
	}
	if reqs := office.Requests(); len(reqs) != 1 || reqs[0].ID != req.ID {
		t.Fatalf("requests after a failed approval = %+v", reqs)
	}
	if _, err := office.Approve(req.ID, ""); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.err != nil {
		t.Fatal(r.err)
	}
}

// Requests beyond the cap are refused at once instead of waiting.
func TestPendingRequestsAreCapped(t *testing.T) {
	office := newLinked(t, "office", "")
	for i := 0; i < link.MaxPendingRequests; i++ {
		pairAsync(t, newLinked(t, fmt.Sprintf("laptop%d", i), ""), office)
	}
	ctx := ctxT(t)
	for len(office.Requests()) < link.MaxPendingRequests {
		if ctx.Err() != nil {
			t.Fatalf("%d of %d Pairing requests arrived", len(office.Requests()), link.MaxPendingRequests)
		}
		time.Sleep(10 * time.Millisecond)
	}
	r := <-pairAsync(t, newLinked(t, "one-too-many", ""), office)
	refused(t, "a request beyond the cap", r.err, link.ReasonBusy)
}

// A Project is delegated only once the Linked Instance shares it with this
// Instance, and never when this Instance already has it (ADR 0027).
func TestDelegateNeedsAShare(t *testing.T) {
	local := map[string]bool{"laptop-own": true}
	laptop := newLinked(t, "laptop", "", func(c *link.Config) { c.HasProject = func(id string) bool { return local[id] } })
	office, other := newLinked(t, "office", ""), newLinked(t, "other", "")
	pair(t, laptop, office)
	pair(t, other, office)

	if _, err := laptop.Delegate(ctxT(t), "team-shared", "office"); err == nil || !strings.Contains(err.Error(), "not shared") {
		t.Fatalf("Delegate before Share = %v, want refused as not shared", err)
	}
	if err := office.Share("team-shared", "other"); err != nil {
		t.Fatal(err)
	}
	if _, err := laptop.Delegate(ctxT(t), "team-shared", "office"); err == nil {
		t.Fatal("Delegate of a Project shared with another link only succeeded")
	}
	if err := office.Share("team-shared", ""); err != nil {
		t.Fatal(err)
	}
	if err := office.Share("laptop-own", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := laptop.Delegate(ctxT(t), "laptop-own", "office"); err == nil || !strings.Contains(err.Error(), "already has") {
		t.Fatalf("Delegate of a local Project = %v, want refused", err)
	}
	if _, err := laptop.Delegate(ctxT(t), "team-shared", "nope"); err == nil {
		t.Fatal("Delegate to no link succeeded")
	}
	d, err := laptop.Delegate(ctxT(t), "team-shared", "office")
	if err != nil {
		t.Fatal(err)
	}
	if d.Project != "team-shared" || d.Link != "office" {
		t.Errorf("Delegate = %+v", d)
	}
	if to, ok := laptop.DelegatedTo("team-shared"); !ok || to != "office" {
		t.Errorf("DelegatedTo = %q, %v", to, ok)
	}
	if !office.Shared("laptop", "team-shared") || office.Shared("laptop", "other-proj") {
		t.Error("Shared does not follow the shares")
	}
	if err := office.Share("team-shared", "nope"); err == nil {
		t.Error("Share with no link succeeded")
	}
	if err := laptop.Share("team-shared", ""); err == nil {
		t.Error("Share of a Delegated Project succeeded")
	}

	// Delegations and Shares are configuration, kept in links.json.
	reopened, err := link.OpenStore(filepath.Join(laptop.dir, link.StoreFile))
	if err != nil {
		t.Fatal(err)
	}
	if ds := reopened.Delegations(); len(ds) != 1 || ds[0] != d {
		t.Errorf("Delegations after reopening = %+v", ds)
	}

	if err := office.Unshare("team-shared", ""); err != nil {
		t.Fatal(err)
	}
	if office.Shared("laptop", "team-shared") {
		t.Error("still shared after Unshare")
	}
	if err := laptop.Undelegate("team-shared"); err != nil {
		t.Fatal(err)
	}
	if _, ok := laptop.DelegatedTo("team-shared"); ok {
		t.Error("still delegated after Undelegate")
	}
}

// Check reports the Projects a reachable link shares with this Instance,
// so that the Web console can show them before they are delegated (#622).
func TestCheckReportsWhatTheLinkShares(t *testing.T) {
	laptop, office, other := newLinked(t, "laptop", ""), newLinked(t, "office", ""), newLinked(t, "other", "")
	pair(t, laptop, office)
	pair(t, other, office)
	if st := laptop.Check(ctxT(t), "office"); !st.Reachable || st.Shared == nil || len(st.Shared) != 0 {
		t.Errorf("Check before any Share = %+v, want reachable with no Projects shared", st)
	}
	for _, sh := range []link.Share{{Project: "team-shared"}, {Project: "for-laptop", With: "laptop"}, {Project: "for-other", With: "other"}} {
		if err := office.Share(sh.Project, sh.With); err != nil {
			t.Fatal(err)
		}
	}
	if st := laptop.Check(ctxT(t), "office"); !slices.Equal(st.Shared, []string{"for-laptop", "team-shared"}) {
		t.Errorf("Check's Shared = %v, want [for-laptop team-shared], by Project", st.Shared)
	}
	if err := office.Close(); err != nil {
		t.Fatal(err)
	}
	if st := laptop.Check(ctxT(t), "office"); st.Reachable || st.Shared != nil {
		t.Errorf("Check of an unreachable link = %+v, want no Shared", st)
	}
}

// Unpairing a link drops the Projects delegated to it and shared with it.
func TestUnpairDropsDelegationsAndShares(t *testing.T) {
	laptop, office := newLinked(t, "laptop", ""), newLinked(t, "office", "")
	pair(t, laptop, office)
	if err := office.Share("team-shared", "laptop"); err != nil {
		t.Fatal(err)
	}
	if _, err := laptop.Delegate(ctxT(t), "team-shared", "office"); err != nil {
		t.Fatal(err)
	}
	if err := laptop.Unpair("office"); err != nil {
		t.Fatal(err)
	}
	if err := office.Unpair("laptop"); err != nil {
		t.Fatal(err)
	}
	if _, ok := laptop.DelegatedTo("team-shared"); ok {
		t.Error("delegated to an unpaired link")
	}
	if len(office.Shares()) != 0 {
		t.Errorf("shares with an unpaired link = %+v", office.Shares())
	}
}
