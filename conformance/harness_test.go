//go:build conformance

package conformance

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	htransport "google.golang.org/api/transport/http"
	"google.golang.org/genproto/googleapis/rpc/code"
	_ "google.golang.org/genproto/googleapis/rpc/errdetails" // so protojson can print error details
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/escapeguard"
	"github.com/linuxuser586/gcpemu/internal/project"
)

const (
	recordEnv  = "GCPEMU_CONFORMANCE_RECORD"
	projectEnv = "GCPEMU_CONFORMANCE_PROJECT"
	// labelKey labels every resource whose API takes labels; its value is
	// the run's suffix, which dates it for the sweeper.
	labelKey = "gcpemu-conformance"
	// emuProject is the project in compare mode. It must not be a string
	// the tests send for themselves, such as labelKey: the normaliser
	// replaces it everywhere with <project>.
	emuProject = "conformance-proj"
	// maxAttempts bounds Retry against GCP.
	maxAttempts = 10
	// cloudPlatform is the OAuth scope record mode asks ADC for.
	cloudPlatform = "https://www.googleapis.com/auth/cloud-platform"
)

// services are the Services the emulator runs for the suite.
var services = []string{"iam", "gcs", "pubsub", "secrets", "dns", "ar"}

// shared is the Instance the package's tests share, started by the first
// test that needs one and stopped by TestMain.
var shared struct {
	once sync.Once
	inst *emutest.Instance
	err  error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if shared.inst != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = shared.inst.Close(ctx)
		cancel()
	}
	os.Exit(code)
}

// sharedInstance is the Instance the package's tests share. It is not Deterministic: the
// normaliser handles generated values as it does GCP's.
func sharedInstance(t testing.TB) *emutest.Instance {
	t.Helper()
	shared.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		shared.inst, shared.err = emutest.New(ctx, services)
	})
	if shared.err != nil {
		t.Fatal(shared.err)
	}
	return shared.inst
}

// target is what a test runs against: the emulator or a real project.
// Test bodies never look at it; conn hands out its client options.
type target struct {
	gcp             bool
	project, number string
	inst            *emutest.Instance // the shared Instance, when !gcp
}

// emulatorTarget is the shared Instance, serving emuProject.
func emulatorTarget(t testing.TB) *target {
	return &target{project: emuProject, number: project.NumberString(emuProject), inst: sharedInstance(t)}
}

var gcpOnce struct {
	once sync.Once
	tg   *target
	err  error
}

// gcpTarget is the project GCPEMU_CONFORMANCE_PROJECT names, reached with
// Application Default Credentials.
func gcpTarget(t testing.TB) *target {
	t.Helper()
	gcpOnce.once.Do(func() {
		id := os.Getenv(projectEnv)
		if id == "" {
			gcpOnce.err = fmt.Errorf("%s=1 needs %s, the project to record against", recordEnv, projectEnv)
			return
		}
		ctx := context.Background()
		crm, err := cloudresourcemanager.NewService(ctx, option.WithScopes(cloudPlatform))
		if err != nil {
			gcpOnce.err = err
			return
		}
		p, err := crm.Projects.Get(id).Context(ctx).Do()
		if err != nil {
			gcpOnce.err = fmt.Errorf("project %s: %w", id, err)
			return
		}
		gcpOnce.tg = &target{gcp: true, project: id, number: strconv.FormatInt(p.ProjectNumber, 10)}
	})
	if gcpOnce.err != nil {
		t.Fatal(gcpOnce.err)
	}
	return gcpOnce.tg
}

// recordHint is how a skipped test, or one whose Recording is stale, says
// its Recording is made.
const recordHint = "run `task conformance:record` to record it against GCP"

// mode is how a test runs: whether it records, where Recordings are, what
// it runs against and how Mismatches are reported.
type mode struct {
	record bool
	dir    string
	target func(testing.TB) *target
	report func(t testing.TB, file string, ms []Mismatch)
}

// envMode is the mode `task conformance` and `task conformance:record`
// select: compare against the emulator, or with GCPEMU_CONFORMANCE_RECORD=1
// record against GCPEMU_CONFORMANCE_PROJECT.
func envMode() mode {
	m := mode{dir: "testdata", target: emulatorTarget, report: reportMismatches}
	if os.Getenv(recordEnv) == "1" {
		m.record, m.target = true, gcpTarget
	}
	return m
}

// reportMismatches fails t, listing each Mismatch with its field path.
func reportMismatches(t testing.TB, file string, ms []Mismatch) {
	t.Errorf("%d Mismatches with the Recording %s:\n%s", len(ms), file, mismatchText(ms))
}

// scenario is one Conformance suite test: the Product its Recording is
// filed under, its Options and its body.
type scenario struct {
	product string
	opts    []Option
	body    func(t testing.TB, c *conn)
}

// run runs s as the test t in m, with its Recording named name.
func (s scenario) run(t testing.TB, m mode, name string) {
	t.Helper()
	s.body(t, begin(t, m, s.product, name, s.opts...))
}

// test runs s as the top-level test t, in the mode the environment selects.
func (s scenario) test(t *testing.T) { s.run(t, envMode(), t.Name()) }

// conn is one test's connection to its target. It captures every
// exchange its clients make until the test body returns.
type conn struct {
	t      testing.TB
	ctx    context.Context
	mode   mode
	target *target
	file   string
	suffix string
	norm   *normaliser
	guard  *escapeguard.Guard // in force against the emulator

	mu        sync.Mutex
	raw       []Exchange
	capturing bool
}

// begin starts a test: in compare mode it skips when the test has no
// Recording, before touching the target.
func begin(t testing.TB, m mode, product, name string, opts ...Option) *conn {
	t.Helper()
	file := filepath.Join(m.dir, product, name+".json")
	if !m.record {
		if _, err := os.Stat(file); errors.Is(err, fs.ErrNotExist) {
			t.Skipf("no Recording %s yet: %s", file, recordHint)
		}
	}
	tg := m.target(t)
	c := &conn{t: t, ctx: context.Background(), mode: m, target: tg, file: file, suffix: newSuffix(time.Now()), capturing: true}
	var err error
	if c.norm, err = newNormaliser(runIDs{tg.project, tg.number, c.suffix}, opts...); err != nil {
		t.Fatal(err)
	}
	if !tg.gcp {
		c.guard = escapeguard.New(t)
	}
	t.Cleanup(c.finish)
	return c
}

// Project is the target's project ID.
func (c *conn) Project() string { return c.target.project }

// Name is a resource name unique to this run: gcpemu-<suffix>-base.
func (c *conn) Name(base string) string { return "gcpemu-" + c.suffix + "-" + base }

// Labels are the labels of every resource whose API takes them.
func (c *conn) Labels() map[string]string { return map[string]string{labelKey: c.suffix} }

// Cleanup deletes what the test made, after it ends, without capturing.
func (c *conn) Cleanup(f func(ctx context.Context)) {
	c.t.Cleanup(func() {
		c.stop()
		f(context.Background())
	})
}

// HTTP is the client options of the REST client library for api
// ("storage", "dns", "iam", …), capturing its Exchanges.
func (c *conn) HTTP(api string) []option.ClientOption {
	c.t.Helper()
	if c.target.gcp {
		tr, err := htransport.NewTransport(c.ctx, capture{c, http.DefaultTransport}, option.WithScopes(cloudPlatform))
		if err != nil {
			c.t.Fatal(err)
		}
		return []option.ClientOption{option.WithHTTPClient(&http.Client{Transport: tr})}
	}
	opts, err := c.target.inst.HTTPOptions(api)
	if err != nil {
		c.t.Fatal(err)
	}
	return append(opts, option.WithHTTPClient(&http.Client{Transport: capture{c, guardedTransport(c.guard)}}))
}

// GRPC is the client options of a gRPC client library, capturing its
// Exchanges.
func (c *conn) GRPC() []option.ClientOption {
	opts := []option.ClientOption{
		option.WithGRPCDialOption(grpc.WithChainUnaryInterceptor(c.unary)),
		option.WithGRPCDialOption(grpc.WithChainStreamInterceptor(c.stream)),
	}
	if c.target.gcp {
		return opts
	}
	return append(append(c.target.inst.GRPCOptions(), opts...),
		option.WithGRPCDialOption(grpc.WithContextDialer(guardedDialer(c.guard))))
}

// Retry runs step until it returns nil. Against GCP, which is eventually
// consistent, it tries up to maxAttempts times and keeps only the last
// attempt's Exchanges; against the emulator it tries once, so a Mismatch
// cannot hide behind a retry.
func (c *conn) Retry(step func() error) {
	c.t.Helper()
	attempts := 1
	if c.target.gcp {
		attempts = maxAttempts
	}
	for i := 1; ; i++ {
		c.mu.Lock()
		mark := len(c.raw)
		c.mu.Unlock()
		err := step()
		if err == nil {
			return
		}
		if i == attempts {
			c.t.Fatalf("after %d attempts: %v", i, err)
		}
		c.mu.Lock()
		c.raw = c.raw[:mark]
		c.mu.Unlock()
		time.Sleep(time.Duration(i) * time.Second)
	}
}

// listed reads a whole listing, so the pages it captures do not depend on
// when the item turns up, and fails unless an item satisfies want. GCP may
// list a new resource late, so callers wrap it in Retry.
func listed[T any](next func() (T, error), what string, want func(T) bool) error {
	found := false
	for {
		x, err := next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return err
		}
		found = found || want(x)
	}
	if !found {
		return fmt.Errorf("%s is not listed", what)
	}
	return nil
}

// stop ends capturing.
func (c *conn) stop() {
	c.mu.Lock()
	c.capturing = false
	c.mu.Unlock()
}

// add captures e, except a long-running Operation poll, whose number
// depends on timing.
func (c *conn) add(e Exchange) {
	if e.Request.Path == "/google.longrunning.Operations/GetOperation" ||
		e.Request.Method == http.MethodGet && strings.Contains(e.Request.Path, "/operations/") {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.capturing {
		c.raw = append(c.raw, e)
	}
}

// finish writes the Recording in record mode, or compares with it.
func (c *conn) finish() {
	t := c.t
	c.stop()
	if c.guard != nil {
		for _, h := range c.guard.Take() {
			t.Errorf("a request left the machine for %s: the suite must reach only the emulator", h)
		}
	}
	if t.Failed() || t.Skipped() {
		return
	}
	got := c.norm.recording(c.raw)
	if c.mode.record {
		if err := writeRecording(c.file, got); err != nil {
			t.Error(err)
		}
		return
	}
	want, err := readRecording(c.file)
	if err != nil {
		t.Error(err)
		return
	}
	ms, err := compare(want, got, c.norm.rules.messages)
	if err != nil {
		t.Errorf("Stale Recording %s: %v: %s", c.file, err, recordHint)
		return
	}
	if len(ms) > 0 {
		c.mode.report(t, c.file, ms)
	}
}

// newSuffix is a run's suffix: now in Unix seconds as 6 base-36 digits,
// so the sweeper can date it, and 4 random ones.
func newSuffix(now time.Time) string {
	s := strconv.FormatInt(now.Unix(), 36)
	s = strings.Repeat("0", max(0, 6-len(s))) + s
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = "0123456789abcdefghijklmnopqrstuvwxyz"[int(b[i])%36]
	}
	return s + string(b)
}

// suffixTime is when the run with suffix s started.
func suffixTime(s string) (time.Time, bool) {
	if len(s) != 10 {
		return time.Time{}, false
	}
	n, err := strconv.ParseInt(s[:6], 36, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(n, 0), true
}

// capture is an http.RoundTripper that captures each exchange at the
// wire, so a field the client library does not know is still compared.
type capture struct {
	c    *conn
	next http.RoundTripper
}

func (x capture) RoundTrip(r *http.Request) (*http.Response, error) {
	var reqBody []byte
	if r.Body != nil {
		var err error
		if reqBody, err = io.ReadAll(r.Body); err != nil {
			return nil, err
		}
		_ = r.Body.Close()
		r = r.Clone(r.Context())
		r.Body = io.NopCloser(bytes.NewReader(reqBody))
	}
	resp, err := x.next.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	respBody, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(respBody))
	x.c.add(Exchange{
		Request:  Request{Method: r.Method, Path: r.URL.RequestURI(), Body: requestBody(reqBody, r.Header.Get("Content-Type"))},
		Response: Response{Status: resp.StatusCode, Body: responseBody(respBody)},
	})
	return resp, nil
}

// requestBody is a request body as kept: its JSON, or for another body
// only its media type (a multipart upload's boundary is random).
func requestBody(b []byte, contentType string) any {
	if len(b) == 0 {
		return nil
	}
	if v, err := decodeJSON(b); err == nil {
		return v
	}
	mt, _, _ := mime.ParseMediaType(contentType)
	return map[string]any{"contentType": mt}
}

// responseBody is a response body as compared: its JSON, its text, or
// {"base64": …} for binary content.
func responseBody(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	if v, err := decodeJSON(b); err == nil {
		return v
	}
	if utf8.Valid(b) {
		return string(b)
	}
	return map[string]any{"base64": base64.StdEncoding.EncodeToString(b)}
}

// unary captures a unary gRPC call.
func (c *conn) unary(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	err := invoker(ctx, method, req, reply, cc, opts...)
	c.add(grpcExchange(method, req, reply, err))
	return err
}

// stream captures each message a streaming gRPC call receives, as an
// exchange with the last message sent.
func (c *conn) stream(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	s, err := streamer(ctx, desc, cc, method, opts...)
	if err != nil {
		c.add(grpcExchange(method, nil, nil, err))
		return nil, err
	}
	return &capturedStream{ClientStream: s, c: c, method: method}, nil
}

type capturedStream struct {
	grpc.ClientStream
	c      *conn
	method string
	mu     sync.Mutex
	last   any
}

func (s *capturedStream) SendMsg(m any) error {
	s.mu.Lock()
	s.last = m
	s.mu.Unlock()
	return s.ClientStream.SendMsg(m)
}

func (s *capturedStream) RecvMsg(m any) error {
	err := s.ClientStream.RecvMsg(m)
	if err == io.EOF {
		return err
	}
	s.mu.Lock()
	last := s.last
	s.mu.Unlock()
	s.c.add(grpcExchange(s.method, last, m, err))
	return err
}

// grpcExchange is a gRPC call as an Exchange: its messages, or on error
// the google.rpc.Status, in protojson.
func grpcExchange(method string, req, reply any, err error) Exchange {
	e := Exchange{Request: Request{Method: "GRPC", Path: method, Body: protoBody(req)}}
	if err != nil {
		st := status.Convert(err)
		e.Response = Response{Code: code.Code(st.Code()).String(), Body: protoBody(st.Proto())}
	} else {
		e.Response = Response{Code: code.Code_OK.String(), Body: protoBody(reply)}
	}
	return e
}

// protoBody is m in protojson, decoded.
func protoBody(m any) any {
	msg, ok := m.(proto.Message)
	if !ok || msg == nil {
		return nil
	}
	b, err := protojson.Marshal(msg)
	if err != nil {
		return map[string]any{"protojsonError": err.Error()}
	}
	v, _ := decodeJSON(b)
	return v
}

// guardedTransport sends each request that would leave the machine to the
// Escape guard g, which refuses it and remembers the host.
func guardedTransport(g *escapeguard.Guard) http.RoundTripper {
	proxy := &url.URL{Scheme: "http", Host: g.Addr()}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = func(r *http.Request) (*url.URL, error) {
		if config.IsLoopback(r.URL.Hostname()) {
			return nil, nil
		}
		return proxy, nil
	}
	return tr
}

// guardedDialer dials loopback addresses and asks the Escape guard g to
// tunnel to any other, which it refuses, remembering the host.
func guardedDialer(g *escapeguard.Guard) func(context.Context, string) (net.Conn, error) {
	return func(ctx context.Context, addr string) (net.Conn, error) {
		var d net.Dialer
		if host, _, _ := net.SplitHostPort(addr); config.IsLoopback(host) {
			return d.DialContext(ctx, "tcp", addr)
		}
		conn, err := d.DialContext(ctx, "tcp", g.Addr())
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", addr, addr)
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err == nil && resp.StatusCode == http.StatusOK {
			return conn, nil
		}
		_ = conn.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("escape guard refused %s: %s", addr, resp.Status)
	}
}
