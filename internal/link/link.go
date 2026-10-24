// Package link connects an Instance to Linked Instances (ADR 0027). This
// part is Pairing: two Instances, each approving the other, come to pin
// each other's Instance CA, after which each accepts the other over a
// link, a mutual TLS connection to the --link-bind listener on which both
// sides present certificates from their own Instance CA.
//
// The listener accepts any client certificate that chains to the CA the
// client presents, and decides at each request: the hello and Pairing
// requests are open to any Instance, everything else only to one whose CA
// is pinned. A client pins the other side's CA itself, so a Pairing gives
// no authority to a machine that merely presents an approved address.
package link

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/linuxuser586/gcpemu/internal/ca"
)

// DefaultPort is the --link-bind listeners' port unless `--port link=N`
// says otherwise; Endpoint names them in emu.Endpoints and --port.
const (
	DefaultPort = 4511
	Endpoint    = "link"
)

// States of a link.
const (
	StatePaired = "paired"
	// StateBroken: an Instance CA no longer matches the one pinned at
	// Pairing, this side's or the other's.
	StateBroken = "broken"
)

// DefaultApprovalTimeout is how long a Pairing request waits for the
// other side to approve it.
const DefaultApprovalTimeout = 5 * time.Minute

// Headers every link request carries.
const (
	versionsHeader   = "Gcpemu-Link-Versions"
	instanceIDHeader = "Gcpemu-Instance-Id"
)

// Reasons a link request is refused, in the refusal's "reason".
const (
	ReasonUnpaired     = "unpaired"
	ReasonCAMismatch   = "ca-mismatch"
	ReasonIncompatible = "incompatible-version"
	ReasonRejected     = "rejected"
	ReasonTimeout      = "not-approved"
	ReasonSelf         = "self"
	ReasonBusy         = "too-many-requests"
)

// MaxPendingRequests caps the Pairing requests waiting for approval, since
// any Instance may make one.
const MaxPendingRequests = 16

// Config configures a Manager.
type Config struct {
	// CA is this Instance's CA; the Manager follows its rotation.
	CA *ca.CA
	// Instance and ID are this Instance's name and Instance ID.
	Instance, ID string
	Store        *Store
	Log          *slog.Logger
	// Versions is the link protocol range spoken; zero means Current.
	Versions Versions
	// ApprovalTimeout zero means DefaultApprovalTimeout.
	ApprovalTimeout time.Duration
}

// Manager manages an Instance's links: its --link-bind listeners, the
// Pairing requests waiting for approval, and the client that dials other
// Instances' listeners.
type Manager struct {
	cfg Config

	mu       sync.Mutex
	pending  map[string]*pendingRequest
	servers  []*http.Server
	closed   bool
	addrs    []string
	clientCA string // fingerprint clientCert was issued under
	client   *tls.Certificate
	// mismatch holds, by link name, the CA mismatch of the latest
	// connection on that link, in or out, until one succeeds again.
	mismatch map[string]error
}

// New returns cfg's Manager; Serve makes it reachable.
func New(cfg Config) *Manager {
	if cfg.Versions == (Versions{}) {
		cfg.Versions = Current
	}
	if cfg.ApprovalTimeout == 0 {
		cfg.ApprovalTimeout = DefaultApprovalTimeout
	}
	return &Manager{cfg: cfg, pending: map[string]*pendingRequest{}, mismatch: map[string]error{}}
}

// Identity is what an Instance tells another about itself.
type Identity struct {
	Name string `json:"name"`
	ID   string `json:"id"`
	// Fingerprint is that of the Instance CA it presented.
	Fingerprint string `json:"fingerprint"`
	// Version and MinVersion are the link protocol range it speaks.
	Version    int `json:"version"`
	MinVersion int `json:"minVersion"`
}

func (id Identity) versions() Versions { return Versions{id.MinVersion, id.Version} }

// Request is an incoming Pairing request waiting for this side's approval.
type Request struct {
	// Replaces names the link that approving it replaces: one to the same
	// Instance ID, pinned to another CA, as after `gcpemu ca rotate`.
	Replaces string `json:"replaces,omitempty"`
	ID       string `json:"id"`
	// Name and InstanceID are the requesting Instance's.
	Name        string    `json:"name"`
	InstanceID  string    `json:"instanceId"`
	Fingerprint string    `json:"fingerprint"`
	Address     string    `json:"address,omitempty"`
	Version     int       `json:"version"`
	Received    time.Time `json:"received"`
}

type pendingRequest struct {
	Request
	ca     []byte
	decide chan error // nil approves
}

// Fingerprint is how links name an Instance CA: the SHA-256 of its
// certificate.
func Fingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// fingerprint is this Instance's own CA fingerprint.
func (m *Manager) fingerprint() string { return Fingerprint(m.cfg.CA.Certificate()) }

// identity is what this Instance tells others about itself.
func (m *Manager) identity() Identity {
	v := m.cfg.Versions
	return Identity{Name: m.cfg.Instance, ID: m.cfg.ID, Fingerprint: m.fingerprint(), Version: v.Max, MinVersion: v.Min}
}

// Serve serves the link protocol on l over mutual TLS until Close.
func (m *Manager) Serve(l net.Listener) error {
	srv := &http.Server{
		Handler:           m.handler(),
		ReadHeaderTimeout: 30 * time.Second,
		ErrorLog:          slog.NewLogLogger(m.cfg.Log.Handler(), slog.LevelDebug),
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS13,
			ClientAuth: tls.RequireAnyClientCert,
			GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
				host, _, _ := net.SplitHostPort(hello.Conn.LocalAddr().String())
				return m.cfg.CA.ServerCert(host)
			},
		},
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return l.Close()
	}
	m.servers = append(m.servers, srv)
	m.addrs = append(m.addrs, l.Addr().String())
	m.mu.Unlock()
	if err := srv.ServeTLS(l, "", ""); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Addresses returns the addresses Serve serves.
func (m *Manager) Addresses() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.addrs...)
}

// Close stops serving and refuses every waiting Pairing request.
func (m *Manager) Close() error {
	m.mu.Lock()
	servers := m.servers
	m.servers, m.closed = nil, true
	for id, p := range m.pending {
		p.decide <- errors.New("the Instance is shutting down")
		delete(m.pending, id)
	}
	m.mu.Unlock()
	var errs []error
	for _, s := range servers {
		errs = append(errs, s.Close())
	}
	return errors.Join(errs...)
}

// --- the listener's side ---

// handler serves the link protocol: hello and Pairing requests to any
// Instance, everything else only to paired ones.
func (m *Manager) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /link/v1/hello", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, m.identity())
	})
	mux.HandleFunc("POST /link/v1/pairings", m.servePair)
	mux.HandleFunc("GET /link/v1/status", func(w http.ResponseWriter, r *http.Request) {
		p, v, ok := m.admit(w, r)
		if !ok {
			return
		}
		w.Header().Set(versionsHeader, m.cfg.Versions.String())
		writeJSON(w, http.StatusOK, map[string]any{"name": m.cfg.Instance, "id": m.cfg.ID, "version": v, "link": p.Name})
	})
	return mux
}

// errCAMismatch says that link name presented the CA presented, not the
// one pinned at Pairing.
func errCAMismatch(name, pinned, presented string) error {
	return caMismatchError{fmt.Errorf("the Instance CA of link %q no longer matches the one pinned at Pairing: pinned %s, presented %s; if it was rotated (`gcpemu ca rotate`), unpair and pair again",
		name, pinned, presented)}
}

// caMismatchError is an errCAMismatch.
type caMismatchError struct{ error }

// errSelf refuses Pairing an Instance with itself.
var errSelf = errors.New("an Instance cannot pair with itself")

// refusal is the body of a refused link request.
type refusal struct {
	Reason string `json:"reason"`
	Error  string `json:"error"`
}

// refuse writes a refusal for reason.
func (m *Manager) refuse(w http.ResponseWriter, code int, reason, msg string) {
	writeJSON(w, code, refusal{Reason: reason, Error: msg})
}

// admit decides a request that needs a Pairing: it comes from an Instance
// whose CA this one pinned, speaking a common protocol version. Otherwise
// it refuses, naming the reason, and logs it so that this side reports it
// too.
func (m *Manager) admit(w http.ResponseWriter, r *http.Request) (Pairing, int, bool) {
	root, err := presentedCA(r.TLS.PeerCertificates, x509.ExtKeyUsageClientAuth)
	if err != nil {
		m.refuse(w, http.StatusForbidden, ReasonUnpaired, err.Error())
		return Pairing{}, 0, false
	}
	fp, id := Fingerprint(root), r.Header.Get(instanceIDHeader)
	var claimed *Pairing
	for _, p := range m.cfg.Store.List() {
		if p.Fingerprint == fp {
			v, err := Negotiate(m.cfg.Versions, parseVersions(r.Header.Get(versionsHeader)), strconv.Quote(p.Name))
			if err != nil {
				m.cfg.Log.Warn("link refused", "link", p.Name, "err", err)
				m.refuse(w, http.StatusConflict, ReasonIncompatible, err.Error())
				return Pairing{}, 0, false
			}
			m.mu.Lock()
			delete(m.mismatch, p.Name)
			m.mu.Unlock()
			return p, v, true
		}
		if p.InstanceID == id {
			claimed = &p
		}
	}
	if claimed != nil {
		err := errCAMismatch(claimed.Name, claimed.Fingerprint, fp)
		m.mu.Lock()
		m.mismatch[claimed.Name] = err
		m.mu.Unlock()
		m.cfg.Log.Warn("link refused", "link", claimed.Name, "err", err)
		m.refuse(w, http.StatusForbidden, ReasonCAMismatch, err.Error())
		return Pairing{}, 0, false
	}
	msg := fmt.Sprintf("Instance %s (CA %s) is not paired with Instance %q; pair them with `gcpemu link pair`", id, fp, m.cfg.Instance)
	m.cfg.Log.Warn("link refused", "err", msg)
	m.refuse(w, http.StatusForbidden, ReasonUnpaired, msg)
	return Pairing{}, 0, false
}

// pairBody is the body of a Pairing request.
type pairBody struct {
	Identity
	Address string `json:"address,omitempty"`
}

// pairReply is the body of an approved Pairing request.
type pairReply struct {
	Identity
	Negotiated int `json:"negotiated"`
}

// servePair serves a Pairing request: it waits for this side's approval
// or rejection, the approval timeout, or the requester to give up.
func (m *Manager) servePair(w http.ResponseWriter, r *http.Request) {
	root, err := presentedCA(r.TLS.PeerCertificates, x509.ExtKeyUsageClientAuth)
	if err != nil {
		m.refuse(w, http.StatusForbidden, ReasonUnpaired, err.Error())
		return
	}
	var body pairBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		m.refuse(w, http.StatusBadRequest, "", err.Error())
		return
	}
	fp := Fingerprint(root)
	if fp == m.fingerprint() {
		m.refuse(w, http.StatusBadRequest, ReasonSelf, errSelf.Error())
		return
	}
	v, err := Negotiate(m.cfg.Versions, body.versions(), fmt.Sprintf("Instance %q", body.Name))
	if err != nil {
		m.cfg.Log.Warn("Pairing refused", "instance", body.Name, "instanceId", body.ID, "err", err)
		m.refuse(w, http.StatusConflict, ReasonIncompatible, err.Error())
		return
	}
	reply := pairReply{Identity: m.identity(), Negotiated: v}
	replaces := ""
	for _, p := range m.cfg.Store.List() {
		switch {
		case p.Fingerprint == fp && p.InstanceID == body.ID:
			writeJSON(w, http.StatusOK, reply) // already paired
			return
		case p.InstanceID == body.ID:
			replaces = p.Name
		}
	}

	req := &pendingRequest{
		Request: Request{Replaces: replaces, ID: randomID(), Name: body.Name, InstanceID: body.ID, Fingerprint: fp, Address: body.Address, Version: v, Received: time.Now()},
		ca:      certPEM(root),
		decide:  make(chan error, 1),
	}
	m.mu.Lock()
	if len(m.pending) >= MaxPendingRequests {
		m.mu.Unlock()
		m.cfg.Log.Warn("Pairing refused: too many requests waiting", "instance", body.Name, "instanceId", body.ID)
		m.refuse(w, http.StatusTooManyRequests, ReasonBusy,
			fmt.Sprintf("Instance %q already has %d Pairing requests waiting for approval; try again later", m.cfg.Instance, MaxPendingRequests))
		return
	}
	m.pending[req.ID] = req
	m.mu.Unlock()
	m.cfg.Log.Info("Pairing requested; approve it with `gcpemu link approve "+req.ID+"` or refuse it with `gcpemu link reject "+req.ID+"`",
		"request", req.ID, "instance", body.Name, "instanceId", body.ID, "fingerprint", fp)
	timer := time.NewTimer(m.cfg.ApprovalTimeout)
	defer timer.Stop()
	var decision error
	decided := false
	select {
	case decision = <-req.decide:
		decided = true
	case <-r.Context().Done():
		decision = r.Context().Err()
	case <-timer.C:
		decision = fmt.Errorf("Instance %q did not approve the Pairing request within %s", m.cfg.Instance, m.cfg.ApprovalTimeout)
	}
	if !decided {
		m.mu.Lock()
		_, waiting := m.pending[req.ID]
		delete(m.pending, req.ID)
		m.mu.Unlock()
		if !waiting {
			// Approve, Reject or Close took it first: their decision
			// stands, so that the two sides agree on it.
			decision = <-req.decide
		}
	}
	switch {
	case decision == nil:
		writeJSON(w, http.StatusOK, reply)
	case errors.Is(decision, errRejected):
		m.refuse(w, http.StatusForbidden, ReasonRejected, fmt.Sprintf("Instance %q rejected the Pairing request", m.cfg.Instance))
	default:
		m.refuse(w, http.StatusRequestTimeout, ReasonTimeout, decision.Error())
	}
}

// errRejected decides a Pairing request that this side rejected.
var errRejected = errors.New("rejected")

// Requests returns the incoming Pairing requests, oldest first.
func (m *Manager) Requests() []Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Request{}
	for _, p := range m.pending {
		out = append(out, p.Request)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Received.Before(out[j].Received) })
	return out
}

// takeRequest removes the incoming Pairing request id.
func (m *Manager) takeRequest(id string) (*pendingRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pending[id]
	if !ok {
		return nil, fmt.Errorf("no Pairing request %q is waiting; `gcpemu link requests` lists them", id)
	}
	delete(m.pending, id)
	return p, nil
}

// Approve approves the incoming Pairing request id, pinning the
// requester's Instance CA under the link name name (default the
// requester's Instance name).
func (m *Manager) Approve(id, name string) (Pairing, error) {
	m.mu.Lock()
	p, ok := m.pending[id]
	m.mu.Unlock()
	if !ok {
		_, err := m.takeRequest(id)
		return Pairing{}, err
	}
	pairing := Pairing{
		Name: name, Instance: p.Name, InstanceID: p.InstanceID, Address: p.Address,
		Fingerprint: p.Fingerprint, CA: string(p.ca), LocalFingerprint: m.fingerprint(),
		Version: p.Version, PairedAt: time.Now().UTC(),
	}
	// Refused while still waiting: the user may approve it under another
	// name.
	if err := m.checkName(&pairing); err != nil {
		return Pairing{}, err
	}
	if _, err := m.takeRequest(id); err != nil {
		return Pairing{}, err
	}
	if err := m.pin(&pairing); err != nil {
		p.decide <- fmt.Errorf("Instance %q could not record the Pairing: %w", m.cfg.Instance, err)
		return Pairing{}, err
	}
	p.decide <- nil
	return pairing, nil
}

// Reject refuses the incoming Pairing request id.
func (m *Manager) Reject(id string) error {
	p, err := m.takeRequest(id)
	if err != nil {
		return err
	}
	p.decide <- errRejected
	return nil
}

// --- the dialling side ---

// Identify asks the Instance whose link listener is at addr who it is,
// without pairing.
func (m *Manager) Identify(ctx context.Context, addr string) (Identity, error) {
	var id Identity
	root, err := m.call(ctx, addr, http.MethodGet, "/link/v1/hello", nil, nil, &id)
	if err != nil {
		return Identity{}, err
	}
	id.Fingerprint = Fingerprint(root)
	return id, nil
}

// Pair asks the Instance at addr, whose Instance CA has the fingerprint
// the user approved, to pair, and waits for its approval. It then pins
// that CA under the link name name (default the other Instance's name).
func (m *Manager) Pair(ctx context.Context, addr, fingerprint, name string) (Pairing, error) {
	addr = Dialable(addr)
	if fingerprint == m.fingerprint() {
		return Pairing{}, errSelf
	}
	pinned := pinnedTo(fingerprint, func(got string) error {
		return fmt.Errorf("the Instance at %s presents CA %s, not the approved %s", addr, got, fingerprint)
	})
	var id Identity
	if _, err := m.call(ctx, addr, http.MethodGet, "/link/v1/hello", pinned, nil, &id); err != nil {
		return Pairing{}, err
	}
	v, err := Negotiate(m.cfg.Versions, id.versions(), fmt.Sprintf("Instance %q", id.Name))
	if err != nil {
		return Pairing{}, err
	}
	pairing := Pairing{
		Name: name, Instance: id.Name, InstanceID: id.ID, Address: addr, Fingerprint: fingerprint,
		LocalFingerprint: m.fingerprint(), Version: v,
	}
	// Refuse a name already taken before bothering the other side.
	if err := m.checkName(&pairing); err != nil {
		return Pairing{}, err
	}
	body := pairBody{Identity: m.identity()}
	if a := m.Addresses(); len(a) > 0 {
		body.Address = a[0]
	}
	var reply pairReply
	root, err := m.call(ctx, addr, http.MethodPost, "/link/v1/pairings", pinned, body, &reply)
	if err != nil {
		return Pairing{}, err
	}
	pairing.Version = reply.Negotiated
	pairing.CA = string(certPEM(root))
	pairing.PairedAt = time.Now().UTC()
	if err := m.pin(&pairing); err != nil {
		return Pairing{}, err
	}
	return pairing, nil
}

// Status is a link's state as this side sees it.
type Status struct {
	Name string `json:"name"`
	// State is StatePaired or StateBroken; "" for no such link.
	State string `json:"state"`
	// InboundOnly: the other Instance has no --link-bind address, so this
	// side cannot dial it; it connects in.
	InboundOnly bool `json:"inboundOnly,omitempty"`
	// Reachable reports whether the other Instance answered over the link,
	// speaking Version.
	Reachable bool   `json:"reachable"`
	Version   int    `json:"version,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Link is a Pairing and its State, as `gcpemu link list` shows it.
type Link struct {
	Pairing
	State   string `json:"state"`
	Problem string `json:"problem,omitempty"`
}

// Pairings returns this Instance's Pairings by name.
func (m *Manager) Pairings() []Pairing { return m.cfg.Store.List() }

// Links returns the Pairings and their State, without dialling anything.
func (m *Manager) Links() []Link {
	out := []Link{}
	for _, p := range m.Pairings() {
		l := Link{Pairing: p, State: StatePaired}
		if err := m.broken(p); err != nil {
			l.State, l.Problem = StateBroken, err.Error()
		}
		out = append(out, l)
	}
	return out
}

// broken reports how p broke since Pairing, if it did: this Instance's CA
// was rotated, or the other Instance last presented another CA.
func (m *Manager) broken(p Pairing) error {
	if p.LocalFingerprint != m.fingerprint() {
		return fmt.Errorf("this Instance's CA was rotated after Pairing with link %q (`gcpemu ca rotate`), so %q no longer accepts it; unpair and pair again", p.Name, p.Name)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mismatch[p.Name]
}

// Check dials the link name and reports its Status.
func (m *Manager) Check(ctx context.Context, name string) Status {
	st := Status{Name: name, State: StatePaired}
	var p *Pairing
	for _, q := range m.Pairings() {
		if q.Name == name {
			p = &q
		}
	}
	if p == nil {
		st.State, st.Error = "", fmt.Sprintf("no link named %q", name)
		return st
	}
	if p.Address == "" {
		st.InboundOnly = true
		st.Error = fmt.Sprintf("link %q has no --link-bind address; it connects to this Instance", name)
	}
	if err := m.broken(*p); err != nil {
		st.State, st.Error = StateBroken, err.Error()
	}
	if p.Address == "" {
		return st
	}
	// A broken link is dialled all the same, so that the other side
	// reports it too.
	var reply struct{ Version int }
	if _, err := m.callPaired(ctx, *p, http.MethodGet, "/link/v1/status", &reply); err != nil {
		if errors.As(err, new(caMismatchError)) {
			m.mu.Lock()
			m.mismatch[p.Name] = err
			m.mu.Unlock()
			st.State = StateBroken
		}
		if st.Error == "" {
			st.Error = err.Error()
		}
		return st
	}
	if st.State == StateBroken {
		return st
	}
	m.mu.Lock()
	delete(m.mismatch, p.Name)
	m.mu.Unlock()
	st.Reachable, st.Version = true, reply.Version
	return st
}

// Unpair removes the link name, so that this side no longer accepts the
// other Instance, at once.
func (m *Manager) Unpair(name string) error {
	m.mu.Lock()
	delete(m.mismatch, name)
	m.mu.Unlock()
	return m.cfg.Store.Update(func(ps []Pairing) ([]Pairing, error) {
		for i, p := range ps {
			if p.Name == name {
				return append(ps[:i], ps[i+1:]...), nil
			}
		}
		return nil, fmt.Errorf("no link named %q; `gcpemu link list` lists them", name)
	})
}

// nameRE matches a valid link name, a DNS label.
var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// checkName defaults p's name to the other Instance's, and refuses one
// that is malformed or names a link to another Instance.
func (m *Manager) checkName(p *Pairing) error {
	if p.Name == "" {
		p.Name = p.Instance
	}
	if !nameRE.MatchString(p.Name) {
		return fmt.Errorf("invalid link name %q: use lowercase letters, digits and hyphens (pass --name)", p.Name)
	}
	for _, q := range m.Pairings() {
		if q.Name == p.Name && q.InstanceID != p.InstanceID {
			return fmt.Errorf("a link named %q already exists, to Instance %s; pass --name to name this one", p.Name, q.InstanceID)
		}
	}
	return nil
}

// pin stores p, replacing any earlier Pairing with the same Instance, as
// when pairing again after `gcpemu ca rotate`.
func (m *Manager) pin(p *Pairing) error {
	if err := m.checkName(p); err != nil {
		return err
	}
	err := m.cfg.Store.Update(func(ps []Pairing) ([]Pairing, error) {
		out := ps[:0]
		for _, q := range ps {
			if q.InstanceID != p.InstanceID && q.Name != p.Name {
				out = append(out, q)
			}
		}
		return append(out, *p), nil
	})
	if err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.mismatch, p.Name)
	m.mu.Unlock()
	m.cfg.Log.Info("paired", "link", p.Name, "instanceId", p.InstanceID, "fingerprint", p.Fingerprint)
	return nil
}

// callPaired makes a request over the link p, trusting only its pinned CA.
func (m *Manager) callPaired(ctx context.Context, p Pairing, method, path string, out any) (*x509.Certificate, error) {
	pinned := pinnedTo(p.Fingerprint, func(got string) error { return errCAMismatch(p.Name, p.Fingerprint, got) })
	return m.call(ctx, p.Address, method, path, pinned, nil, out)
}

// pinnedTo verifies that a presented CA has the fingerprint want, and
// otherwise returns mismatch of the one it has.
func pinnedTo(want string, mismatch func(got string) error) func(*x509.Certificate) error {
	return func(root *x509.Certificate) error {
		if got := Fingerprint(root); got != want {
			return mismatch(got)
		}
		return nil
	}
}

// certPEM encodes c as PEM.
func certPEM(c *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
}

// call makes a link request to addr, which presents a CA that verify
// accepts (any, when nil), and decodes a 200 reply into out. A refusal
// is returned as an error saying why. It returns the CA presented.
func (m *Manager) call(ctx context.Context, addr, method, path string, verify func(*x509.Certificate) error, in, out any) (*x509.Certificate, error) {
	addr = Dialable(addr)
	var (
		mu   sync.Mutex
		root *x509.Certificate
		verr error
	)
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS13,
			// The chain is checked against the CA the server presents, and
			// that CA against the pinned one, in VerifyConnection.
			InsecureSkipVerify:   true,
			GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return m.clientCert() },
			VerifyConnection: func(cs tls.ConnectionState) error {
				r, err := presentedCA(cs.PeerCertificates, x509.ExtKeyUsageServerAuth)
				if err == nil && verify != nil {
					err = verify(r)
				}
				mu.Lock()
				root, verr = r, err
				mu.Unlock()
				return err
			},
		},
	}
	defer tr.CloseIdleConnections()
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://"+addr+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set(versionsHeader, m.cfg.Versions.String())
	req.Header.Set(instanceIDHeader, m.cfg.ID)
	resp, err := (&http.Client{Transport: tr}).Do(req)
	mu.Lock()
	presented, verifyErr := root, verr
	mu.Unlock()
	if verifyErr != nil {
		return nil, verifyErr
	}
	if err != nil {
		return nil, fmt.Errorf("link %s: %w", addr, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var ref refusal
		if json.Unmarshal(b, &ref) == nil && ref.Error != "" {
			return nil, &RefusedError{Reason: ref.Reason, Msg: ref.Error}
		}
		return nil, fmt.Errorf("link %s: %s %s", addr, resp.Status, bytes.TrimSpace(b))
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return nil, fmt.Errorf("link %s: %w", addr, err)
		}
	}
	return presented, nil
}

// RefusedError is a link request the other Instance refused.
type RefusedError struct {
	Reason, Msg string
}

func (e *RefusedError) Error() string { return e.Msg }

// clientCert is this Instance's link client certificate, issued again
// once the Instance CA is rotated.
func (m *Manager) clientCert() (*tls.Certificate, error) {
	fp := m.fingerprint()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.client != nil && m.clientCA == fp && time.Now().Before(m.client.Leaf.NotAfter.Add(-24*time.Hour)) {
		return m.client, nil
	}
	c, err := m.cfg.CA.Issue(ca.Leaf{DNSNames: []string{"gcpemu-link"}, ClientOnly: true, Organization: "gcpemu"})
	if err != nil {
		return nil, err
	}
	m.client, m.clientCA = c, fp
	return c, nil
}

// presentedCA returns the Instance CA that chain, as the other Instance
// presented it in a TLS handshake (a leaf for usage, then its CA), ends
// in, having checked that the CA is self-signed and signed the leaf.
func presentedCA(chain []*x509.Certificate, usage x509.ExtKeyUsage) (*x509.Certificate, error) {
	if len(chain) < 2 {
		return nil, errors.New("the other Instance presented no Instance CA with its certificate")
	}
	root := chain[len(chain)-1]
	if !root.IsCA || root.CheckSignatureFrom(root) != nil {
		return nil, errors.New("the other Instance's certificate chain does not end in a self-signed Instance CA")
	}
	pool := x509.NewCertPool()
	pool.AddCert(root)
	if _, err := chain[0].Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{usage}}); err != nil {
		return nil, fmt.Errorf("the other Instance's certificate: %w", err)
	}
	return root, nil
}

// Dialable adds DefaultPort to an address without a port.
func Dialable(addr string) string {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return net.JoinHostPort(addr, strconv.Itoa(DefaultPort))
	}
	return addr
}

// randomID names a Pairing request.
func randomID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// writeJSON writes v as a JSON reply with status code.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
