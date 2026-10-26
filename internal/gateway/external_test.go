package gateway

import (
	"context"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/datastore/apiv1/datastorepb"
	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// serveGateway serves a gateway with no Services on loopback and returns
// it with its address.
func serveGateway(t *testing.T) (*Gateway, string) {
	t.Helper()
	g := New(&emu.Env{Config: &config.Config{}, Log: slog.New(slog.DiscardHandler)}, nil)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(l) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = g.Shutdown(ctx)
	})
	return g, l.Addr().String()
}

// fakeREST is an External emulator's REST surface, answering every request
// with what it received.
func fakeREST(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Got-Host", r.Host)
		w.Header().Set("X-Got-Authorization", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, r.Method+" "+r.URL.RequestURI()+" "+string(body))
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return l.Addr().String()
}

// TestDelegatedREST: a REST request under /API/ on the API gateway's own
// address reaches the External emulator with the prefix taken off and
// nothing else changed (#623).
func TestDelegatedREST(t *testing.T) {
	g, addr := serveGateway(t)
	ext := fakeREST(t)
	if err := g.Delegate("firestore", ext); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/firestore/v1/projects/p/databases/(default)/documents/c?documentId=d",
		strings.NewReader(`{"fields":{}}`))
	req.Header.Set("Authorization", "Bearer owner")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("status = %d, want the External emulator's 418", resp.StatusCode)
	}
	if want := `POST /v1/projects/p/databases/(default)/documents/c?documentId=d {"fields":{}}`; string(b) != want {
		t.Errorf("External emulator got %q, want %q", b, want)
	}
	if got := resp.Header.Get("X-Got-Authorization"); got != "Bearer owner" {
		t.Errorf("Authorization = %q, want it forwarded unchanged", got)
	}
}

// TestDelegatedHost: in Host mode a request for the API's real hostname
// reaches the External emulator unchanged, and the Google front end serves
// that hostname (#623). Bigtable's admin API has a host of its own.
func TestDelegatedHost(t *testing.T) {
	g, addr := serveGateway(t)
	ext := fakeREST(t)
	if err := g.Delegate("bigtable", ext); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"bigtable.googleapis.com", "bigtableadmin.googleapis.com"} {
		if !slices.Contains(g.Hosts(), host) {
			t.Errorf("Hosts() = %v, want %s", g.Hosts(), host)
		}
		req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/v2/projects/p/instances/i/tables", nil)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if want := "GET /v2/projects/p/instances/i/tables "; string(b) != want {
			t.Errorf("%s: External emulator got %q, want %q", host, b, want)
		}
		if got := resp.Header.Get("X-Got-Host"); got != host {
			t.Errorf("%s: Host = %q, want it forwarded unchanged", host, got)
		}
	}
}

// rawCodec passes gRPC messages through as bytes, so that the tests need no
// generated API.
type rawCodec struct{}

func (rawCodec) Marshal(v any) ([]byte, error) { return *v.(*[]byte), nil }
func (rawCodec) Unmarshal(b []byte, v any) error {
	*v.(*[]byte) = append([]byte(nil), b...)
	return nil
}
func (rawCodec) Name() string { return "proto" }

// fakeGRPC is an External emulator's gRPC surface: for every method it
// answers each message with the method, the :authority and the message,
// then ends the call with a trailer.
func fakeGRPC(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.ForceServerCodec(rawCodec{}), grpc.UnknownServiceHandler(func(_ any, ss grpc.ServerStream) error {
		method, _ := grpc.MethodFromServerStream(ss)
		md, _ := metadata.FromIncomingContext(ss.Context())
		for {
			var in []byte
			if err := ss.RecvMsg(&in); err == io.EOF {
				break
			} else if err != nil {
				return err
			}
			out := []byte(method + " " + strings.Join(md.Get(":authority"), "") + " " + string(in))
			if err := ss.SendMsg(&out); err != nil {
				return err
			}
		}
		ss.SetTrailer(metadata.Pairs("x-external", "yes"))
		return nil
	}))
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(srv.Stop)
	return l.Addr().String()
}

// dialGRPC returns a plaintext gRPC connection to addr, as clients make
// with FIRESTORE_EMULATOR_HOST and the like.
func dialGRPC(t *testing.T, addr string, opts ...grpc.DialOption) *grpc.ClientConn {
	t.Helper()
	opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(rawCodec{})))
	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// TestDelegatedGRPC: a gRPC call to one of the API's google.API.* services
// on the API gateway's address reaches the External emulator, unary and
// streaming alike, with its trailers coming back (#623).
func TestDelegatedGRPC(t *testing.T) {
	g, addr := serveGateway(t)
	if err := g.Delegate("firestore", fakeGRPC(t)); err != nil {
		t.Fatal(err)
	}
	conn := dialGRPC(t, addr)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	in, out := []byte("get"), []byte(nil)
	var trailer metadata.MD
	if err := conn.Invoke(ctx, "/google.firestore.v1.Firestore/GetDocument", &in, &out, grpc.Trailer(&trailer)); err != nil {
		t.Fatal(err)
	}
	if want := "/google.firestore.v1.Firestore/GetDocument " + addr + " get"; string(out) != want {
		t.Errorf("unary reply = %q, want %q", out, want)
	}
	if got := trailer.Get("x-external"); len(got) != 1 || got[0] != "yes" {
		t.Errorf("trailer = %v, want the External emulator's", trailer)
	}

	// Listen is bidirectional: each reply must arrive before the next
	// message is sent.
	desc := &grpc.StreamDesc{ClientStreams: true, ServerStreams: true}
	st, err := conn.NewStream(ctx, desc, "/google.firestore.v1.Firestore/Listen")
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range []string{"one", "two"} {
		m := []byte(msg)
		if err := st.SendMsg(&m); err != nil {
			t.Fatal(err)
		}
		var got []byte
		if err := st.RecvMsg(&got); err != nil {
			t.Fatal(err)
		}
		if want := "/google.firestore.v1.Firestore/Listen " + addr + " " + msg; string(got) != want {
			t.Errorf("stream reply = %q, want %q", got, want)
		}
	}
	if err := st.CloseSend(); err != nil {
		t.Fatal(err)
	}
	var end []byte
	if err := st.RecvMsg(&end); err != io.EOF {
		t.Errorf("end of stream = %v, want io.EOF", err)
	}
}

// TestDelegatedOperations: on the API gateway's address, google.longrunning
// calls naming an Operation of a Delegated API, such as a Spanner
// database's, reach its External emulator, while those of the Services'
// own Operations stay with the gateway (#623).
func TestDelegatedOperations(t *testing.T) {
	g, addr := serveGateway(t)
	if err := g.Delegate("spanner", fakeGRPC(t)); err != nil {
		t.Fatal(err)
	}
	conn := dialGRPC(t, addr)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, tc := range []struct {
		method string
		req    proto.Message
	}{
		{"GetOperation", &longrunningpb.GetOperationRequest{Name: "projects/p/instances/i/databases/d/operations/o"}},
		{"ListOperations", &longrunningpb.ListOperationsRequest{Name: "projects/p/instances/i/operations"}},
		{"CancelOperation", &longrunningpb.CancelOperationRequest{Name: "projects/p/instances/i/operations/o"}},
	} {
		in, _ := proto.Marshal(tc.req)
		var out []byte
		method := "/google.longrunning.Operations/" + tc.method
		if err := conn.Invoke(ctx, method, &in, &out); err != nil {
			t.Errorf("%s: %v", tc.method, err)
			continue
		}
		if !strings.HasPrefix(string(out), method+" ") {
			t.Errorf("%s: reply = %q, want the External emulator's", tc.method, out)
		}
	}
	in, _ := proto.Marshal(&longrunningpb.GetOperationRequest{Name: "projects/p/locations/l/operations/o"})
	var out []byte
	err := conn.Invoke(ctx, "/google.longrunning.Operations/GetOperation", &in, &out)
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("a Service's Operation: err = %v, want the gateway's own Unimplemented (no Service here)", err)
	}
}

// TestDelegatedUnreachable: while its External emulator is down, a
// Delegated API answers UNAVAILABLE naming the API and the address, over
// REST and gRPC (#623).
func TestDelegatedUnreachable(t *testing.T) {
	g, addr := serveGateway(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	down := l.Addr().String()
	l.Close()
	if err := g.Delegate("firestore", down); err != nil {
		t.Fatal(err)
	}
	named := func(msg string) bool {
		return strings.Contains(msg, "firestore") && strings.Contains(msg, down)
	}

	resp, err := http.Get("http://" + addr + "/firestore/v1/projects/p/databases/(default)/documents/c/d")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(b), `"UNAVAILABLE"`) || !named(string(b)) {
		t.Errorf("REST: %d %s, want 503 UNAVAILABLE naming firestore and %s", resp.StatusCode, b, down)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	in, out := []byte("get"), []byte(nil)
	err = dialGRPC(t, addr).Invoke(ctx, "/google.firestore.v1.Firestore/GetDocument", &in, &out)
	if s, _ := status.FromError(err); s.Code() != codes.Unavailable || !named(s.Message()) {
		t.Errorf("gRPC: %v, want UNAVAILABLE naming firestore and %s", err, down)
	}
}

// fakeService serves the storage API by path and host, and Pub/Sub's gRPC
// health-like service, as gcs and pubsub do.
type fakeService struct{}

func (fakeService) Name() string { return "fake" }
func (fakeService) Register(r emu.Router) error {
	r.Mount("storage", []string{"storage.googleapis.com"}, http.NotFoundHandler())
	r.Mount("dns", nil, http.NotFoundHandler())
	r.GRPC().RegisterService(&grpc.ServiceDesc{ServiceName: "google.pubsub.v1.Publisher", HandlerType: (*any)(nil)}, struct{}{})
	return nil
}
func (fakeService) Start(context.Context) error { return nil }
func (fakeService) Stop(context.Context) error  { return nil }
func (fakeService) Ready() error                { return nil }

// TestDelegateServed: an API that a Service serves, by its path, its host
// or a gRPC service in its package, is not delegated, and the refusal
// names the Service (#623).
func TestDelegateServed(t *testing.T) {
	g, _ := serveGateway(t)
	if err := g.Register(fakeService{}); err != nil {
		t.Fatal(err)
	}
	for _, api := range []string{"storage", "dns", "pubsub"} {
		err := g.Delegate(api, "localhost:8080")
		if err == nil || !strings.Contains(err.Error(), `"fake"`) {
			t.Errorf("Delegate(%s) = %v, want a refusal naming the fake Service", api, err)
		}
	}
	if err := g.Delegate("firestore", "localhost:8080"); err != nil {
		t.Errorf("Delegate(firestore) = %v", err)
	}
	if err := g.Delegate("firestore", "localhost:8081"); err == nil {
		t.Error("delegating firestore twice: want an error")
	}
}

// TestDelegatedRootREST: REST that a client sends to the API gateway's
// root, as it does to FIRESTORE_EMULATOR_HOST or DATASTORE_EMULATOR_HOST
// (Node's preferRest, firebase-admin, the emulators' own reset endpoint),
// reaches the External emulator unchanged when its path is the API's (#623).
func TestDelegatedRootREST(t *testing.T) {
	g, addr := serveGateway(t)
	ext := fakeREST(t)
	for _, api := range []string{"firestore", "datastore"} {
		if err := g.Delegate(api, ext); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		method, path string
		forwarded    bool
	}{
		{http.MethodGet, "/v1/projects/p/databases/(default)/documents/c/d", true},
		{http.MethodPost, "/v1beta1/projects/p/databases/(default)/documents:runQuery", true},
		{http.MethodDelete, "/emulator/v1/projects/p/databases/(default)/documents", true},
		{http.MethodPost, "/v1/projects/p:lookup", true},
		{http.MethodGet, "/v1/projects/p/topics", false},
	} {
		req, _ := http.NewRequest(tc.method, "http://"+addr+tc.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if got := string(b) == tc.method+" "+tc.path+" "; got != tc.forwarded {
			t.Errorf("%s %s: forwarded = %v (%d %q), want %v", tc.method, tc.path, got, resp.StatusCode, b, tc.forwarded)
		}
	}
}

// TestDelegateHostTaken: an API whose hostname another Delegated API
// already has is refused, as Bigtable's admin API is with Bigtable's.
func TestDelegateHostTaken(t *testing.T) {
	g, _ := serveGateway(t)
	if err := g.Delegate("bigtable", "localhost:8086"); err != nil {
		t.Fatal(err)
	}
	if err := g.Delegate("bigtableadmin", "localhost:8087"); err == nil || !strings.Contains(err.Error(), "bigtable") {
		t.Errorf("Delegate(bigtableadmin) = %v, want a refusal naming bigtable", err)
	}
}

// TestManagedRecordsDatabases: the API gateway records the project and
// database of each request it forwards to a Managed emulator for Firestore
// or Datastore, over gRPC and REST alike, Datastore's default database
// counting as one (#649, ADR 0036).
func TestManagedRecordsDatabases(t *testing.T) {
	g, addr := serveGateway(t)
	rest, grpcAddr := fakeREST(t), fakeGRPC(t)
	dial := func(ctx context.Context, grpc bool) (net.Conn, error) {
		target := rest
		if grpc {
			target = grpcAddr
		}
		var d net.Dialer
		return d.DialContext(ctx, "tcp", target)
	}
	var mu sync.Mutex
	got := map[string]bool{}
	for _, api := range []string{"firestore", "datastore"} {
		if err := g.DelegateManaged(api, dial, func(project, database string) {
			mu.Lock()
			got[api+" "+project+" "+database] = true
			mu.Unlock()
		}); err != nil {
			t.Fatal(err)
		}
	}
	conn := dialGRPC(t, addr)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, c := range []struct {
		method string
		msg    proto.Message
	}{
		{"/google.firestore.v1.Firestore/GetDocument", &firestorepb.GetDocumentRequest{Name: "projects/fs-a/databases/(default)/documents/c/d"}},
		{"/google.firestore.v1.Firestore/Commit", &firestorepb.CommitRequest{Database: "projects/fs-b/databases/named"}},
		{"/google.datastore.v1.Datastore/Lookup", &datastorepb.LookupRequest{ProjectId: "ds-a"}},
		{"/google.datastore.v1.Datastore/RunQuery", &datastorepb.RunQueryRequest{ProjectId: "ds-b", DatabaseId: "named"}},
	} {
		in, _ := proto.Marshal(c.msg)
		var out []byte
		if err := conn.Invoke(ctx, c.method, &in, &out); err != nil {
			t.Fatalf("%s: %v", c.method, err)
		}
	}
	for _, r := range []struct{ path, body string }{
		{"/firestore/v1/projects/fs-c/databases/(default)/documents/c/d", ""},
		{"/v1/projects/fs-d/databases/rest-db/documents:commit", "{}"},
		{"/v1/projects/ds-c:commit", `{"databaseId":"rest-db","mode":"NON_TRANSACTIONAL"}`},
		{"/v1/projects/ds-d:lookup", `{"keys":[]}`},
	} {
		resp, err := http.Post("http://"+addr+r.path, "application/json", strings.NewReader(r.body))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.HasSuffix(string(b), " "+r.body) {
			t.Errorf("%s: the emulator got %q, want the body %q unchanged", r.path, b, r.body)
		}
	}
	want := []string{
		"datastore ds-a ", "datastore ds-b named", "datastore ds-c rest-db", "datastore ds-d ",
		"firestore fs-a (default)", "firestore fs-b named", "firestore fs-c (default)", "firestore fs-d rest-db",
	}
	mu.Lock()
	defer mu.Unlock()
	if keys := slices.Sorted(maps.Keys(got)); !slices.Equal(keys, want) {
		t.Errorf("recorded %q, want %q", keys, want)
	}
}
