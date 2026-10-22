package link_test

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/ca"
	"github.com/linuxuser586/gcpemu/internal/link"
)

// node is a link.Manager serving on a loopback listener, with what a test
// needs to reach it.
type node struct {
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

// newNode starts the Node of an Instance named name, with its own Instance
// CA and pairings file in dir (a new temp dir when "").
func newNode(t *testing.T, name, dir string, opts ...func(*link.Config)) *node {
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
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = n.Serve(l) }()
	t.Cleanup(func() { _ = n.Close() })
	return &node{Manager: n, addr: l.Addr().String(), ca: authority, dir: dir, store: store, log: logs}
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// waitRequest waits until n has an incoming Pairing request and returns it.
func waitRequest(t *testing.T, n *node) link.Request {
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
	p   link.Pairing
	err error
}

// pairAsync starts from pairing with to, as `gcpemu link pair` does once
// its user has approved to's identity.
func pairAsync(t *testing.T, from, to *node) <-chan pairResult {
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
func pair(t *testing.T, a, b *node) {
	t.Helper()
	done := pairAsync(t, a, b)
	if _, err := b.Approve(waitRequest(t, b).ID, ""); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.err != nil {
		t.Fatal(r.err)
	}
}

func names(ps []link.Pairing) []string {
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
	laptop, office := newNode(t, "laptop", ""), newNode(t, "office", "")

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
	if len(laptop.Pairings()) != 0 || len(office.Pairings()) != 0 {
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
	if got := names(laptop.Pairings()); len(got) != 1 || got[0] != "office" {
		t.Errorf("laptop lists %v", got)
	}
	if got := names(office.Pairings()); len(got) != 1 || got[0] != "laptop" {
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
	office, stranger := newNode(t, "office", ""), newNode(t, "stranger", "")
	id, err := stranger.Identify(ctxT(t), office.addr)
	if err != nil {
		t.Fatal(err)
	}
	// The stranger pins the office's CA on its own, without a Pairing.
	if err := stranger.store.Update(func(ps []link.Pairing) ([]link.Pairing, error) {
		return append(ps, link.Pairing{Name: "office", InstanceID: id.ID, Address: office.addr, Fingerprint: id.Fingerprint,
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
	laptop, office := newNode(t, "laptop", ""), newNode(t, "office", "")
	pair(t, laptop, office)
	if err := office.Unpair("laptop"); err != nil {
		t.Fatal(err)
	}
	if len(office.Pairings()) != 0 {
		t.Errorf("office still lists %v", names(office.Pairings()))
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
	laptop, office := newNode(t, "laptop", ""), newNode(t, "office", "")
	done := pairAsync(t, laptop, office)
	if err := office.Reject(waitRequest(t, office).ID); err != nil {
		t.Fatal(err)
	}
	refused(t, "rejected Pair", (<-done).err, link.ReasonRejected, `"office" rejected`)
	if len(laptop.Pairings())+len(office.Pairings()) != 0 {
		t.Errorf("rejected Pairing pinned %v and %v", names(laptop.Pairings()), names(office.Pairings()))
	}
}

// `gcpemu ca rotate` breaks an Instance's Pairings, and both sides report
// it, naming the link.
func TestRotatedCABreaksPairings(t *testing.T) {
	laptop, office := newNode(t, "laptop", ""), newNode(t, "office", "")
	pair(t, laptop, office)
	if err := laptop.ca.Rotate("laptop"); err != nil {
		t.Fatal(err)
	}

	// The rotating side knows without dialling.
	ls := laptop.Links()
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
	laptop, office := newNode(t, "laptop", ""), newNode(t, "future", "", v2)
	id, err := laptop.Identify(ctxT(t), office.addr)
	if err != nil {
		t.Fatal(err)
	}
	_, err = laptop.Pair(ctxT(t), office.addr, id.Fingerprint, "")
	if err == nil || !strings.Contains(err.Error(), "version 1") || !strings.Contains(err.Error(), "version 2") {
		t.Errorf("Pair across versions: %v", err)
	}

	// Paired at version 1, then the office is upgraded past it.
	office = newNode(t, "office", "")
	pair(t, laptop, office)
	upgraded := newNode(t, "office", office.dir, v2)
	_ = office.Close()
	if err := laptop.store.Update(func(ps []link.Pairing) ([]link.Pairing, error) {
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
// rotated, the other side still reports the link broken once it tries,
// naming it, without dialling back.
func TestRotationReportedOnInboundOnlyLink(t *testing.T) {
	laptop, office := newNode(t, "laptop", ""), newNode(t, "office", "")
	pair(t, laptop, office)
	// The laptop had no listener of its own: the office cannot dial it.
	if err := office.store.Update(func(ps []link.Pairing) ([]link.Pairing, error) {
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
	ls := office.Links()
	if len(ls) != 1 || ls[0].State != link.StateBroken || !strings.Contains(ls[0].Problem, `the Instance CA of link "laptop" no longer matches`) {
		t.Errorf("office's links after the laptop rotated = %+v", ls)
	}
	if st := office.Check(ctxT(t), "laptop"); st.State != link.StateBroken {
		t.Errorf("office's Check after the laptop rotated = %+v", st)
	}
}

// Approving a request from an Instance already paired under another CA
// replaces that Pairing, so the request says so before anyone approves it.
func TestRequestReplacingAPairingSaysSo(t *testing.T) {
	laptop, office := newNode(t, "laptop", ""), newNode(t, "office", "")
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
	if ls := office.Links(); len(ls) != 1 || ls[0].Fingerprint != link.Fingerprint(laptop.ca.Certificate()) || ls[0].State != link.StatePaired {
		t.Errorf("office's links after pairing again = %+v", ls)
	}
}
