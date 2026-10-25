package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"cloud.google.com/go/iam/credentials/apiv1/credentialspb"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/reqlog"
)

// The Projects a request names, by which the API gateway forwards it to
// the Linked Instance they are delegated to, and by which the Linked
// Instance checks that they are shared with that link (ADR 0027).
func TestRequestProjects(t *testing.T) {
	rest := []struct {
		target string
		want   []string
	}{
		{"/compute/v1/projects/team-shared/global/networks", []string{"team-shared"}},
		{"/v1/projects/team-shared/topics/t:publish", []string{"team-shared"}},
		{"/v1/projects/team-shared:getIamPolicy", []string{"team-shared"}},
		{"/v3/projects/team-shared", []string{"team-shared"}},
		{"/v1/projects/123456789012/secrets", []string{"123456789012"}},
		{"/storage/v1/b?project=team-shared", []string{"team-shared"}},
		{"/v1/projects/team-shared/subscriptions/s/x/projects/other-proj", []string{"team-shared", "other-proj"}},
		// A project parameter names the Project only of the bucket
		// collection, never of a bucket or object it names in its path.
		{"/storage/v1/b/private-bucket/o?project=team-shared", nil},
		{"/storage/v1/b/bucket/o/projects%2Fteam-shared%2Fx", nil},
		// A service account named under projects/- names the Project in
		// its email.
		{"/v1/projects/-/serviceAccounts/sa@team-shared.iam.gserviceaccount.com", []string{"team-shared"}},
		{"/iamcredentials/v1/projects/-/serviceAccounts/sa%40team-shared.iam.gserviceaccount.com:generateAccessToken", []string{"team-shared"}},
		{"/iam/v1/projects/-/serviceAccounts/sa@team-shared.iam.gserviceaccount.com/keys", []string{"team-shared"}},
		{"/v1/projects/-/serviceAccounts/123456789012-compute@developer.gserviceaccount.com:signBlob", []string{"123456789012"}},
		{"/v1/projects/-/serviceAccounts/someone@example.com", nil},
		{"/v1/projects/-/serviceAccounts", nil},
		// A service account's certificates name the Project in its email.
		{"/service_accounts/v1/metadata/x509/sa@team-shared.iam.gserviceaccount.com", []string{"team-shared"}},
		{"/service_accounts/v1/metadata/jwk/sa%40team-shared.iam.gserviceaccount.com", []string{"team-shared"}},
		{"/service_accounts/v1/metadata/x509/someone@example.com", nil},
		{"/service_accounts/v1/metadata/x509", nil},
		{"/storage/v1/b/projects/o", nil},
		// The Admin API, the Web console and agent callbacks name none,
		// however they are escaped.
		{"/_emu/v1/projects/team-shared", nil},
		{"/%5Femu/v1/reset?project=team-shared", nil},
		{"/console/projects/team-shared", nil},
		{"/v1/projects/team-shared/_gcpemu/x", nil},
		{"/", nil},
	}
	for _, c := range rest {
		if got := requestProjects(httptest.NewRequest(http.MethodGet, c.target, nil)); !slices.Equal(got, c.want) {
			t.Errorf("%s names Projects %q, want %q", c.target, got, c.want)
		}
	}

	assertion := func(claims jwt.MapClaims) string {
		s, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("k"))
		return s
	}
	bodies := []struct {
		name, method, host, target, contentType, header, body string
		want                                                  []string
	}{
		{"JWT-bearer grant", http.MethodPost, "", "/oauth2/token", "application/x-www-form-urlencoded", "",
			"grant_type=urn%3Aietf%3Aparams%3Aoauth%3Agrant-type%3Ajwt-bearer&assertion=" + assertion(jwt.MapClaims{"iss": "sa@team-shared.iam.gserviceaccount.com"}), []string{"team-shared"}},
		{"JWT-bearer grant in host mode", http.MethodPost, "oauth2.googleapis.com", "/token", "application/json", "",
			`{"grant_type":"urn:ietf:params:oauth:grant-type:jwt-bearer","assertion":"` + assertion(jwt.MapClaims{"iss": "sa@team-shared.iam.gserviceaccount.com"}) + `"}`, []string{"team-shared"}},
		{"JWT-bearer grant of no service account", http.MethodPost, "", "/oauth2/token", "application/x-www-form-urlencoded", "",
			"grant_type=urn%3Aietf%3Aparams%3Aoauth%3Agrant-type%3Ajwt-bearer&assertion=" + assertion(jwt.MapClaims{"iss": "someone@example.com"}), nil},
		{"refresh grant", http.MethodPost, "", "/oauth2/token", "application/x-www-form-urlencoded", "", "grant_type=refresh_token&refresh_token=x", nil},
		{"XML API bucket", http.MethodPut, "storage.googleapis.com", "/new-bucket", "", "team-shared", "", []string{"team-shared"}},
	}
	for _, c := range bodies {
		r := httptest.NewRequest(c.method, c.target, strings.NewReader(c.body))
		if c.host != "" {
			r.Host = c.host
		}
		if c.contentType != "" {
			r.Header.Set("Content-Type", c.contentType)
		}
		if c.header != "" {
			r.Header.Set("X-Goog-Project-Id", c.header)
		}
		if got := requestProjects(r); !slices.Equal(got, c.want) {
			t.Errorf("%s: names Projects %q, want %q", c.name, got, c.want)
		}
		if b, _ := io.ReadAll(r.Body); string(b) != c.body {
			t.Errorf("%s: body after reading the Projects = %q, want %q", c.name, b, c.body)
		}
	}

	topic := &pubsubpb.PublishRequest{Topic: "projects/team-shared/topics/t"}
	// smuggled names team-shared in a field PublishRequest does not have,
	// ahead of the topic it really publishes to.
	smuggled := protowire.AppendString(protowire.AppendTag(nil, 99, protowire.BytesType), "projects/team-shared/topics/t")
	real, _ := proto.Marshal(&pubsubpb.PublishRequest{Topic: "projects/private/topics/t"})
	grpcs := []struct {
		name     string
		method   string
		body     []byte
		encoding string
		want     []string
	}{
		{"top-level field", "/google.pubsub.v1.Publisher/Publish", marshal(t, topic), "", []string{"team-shared"}},
		{"two fields", "/google.pubsub.v1.Subscriber/CreateSubscription", marshal(t, &pubsubpb.Subscription{Name: "projects/team-shared/subscriptions/s", Topic: "projects/other-proj/topics/t"}), "", []string{"team-shared", "other-proj"}},
		{"nested message", "/google.pubsub.v1.Publisher/UpdateTopic", marshal(t, &pubsubpb.UpdateTopicRequest{Topic: &pubsubpb.Topic{Name: "projects/team-shared/topics/t"}}), "", []string{"team-shared"}},
		{"data and attributes", "/google.pubsub.v1.Publisher/Publish", marshal(t, &pubsubpb.PublishRequest{Topic: "projects/team-shared/topics/t", Messages: []*pubsubpb.PubsubMessage{{Data: []byte("projects/other-proj/x"), Attributes: map[string]string{"k": "projects/other-proj/x"}}}}), "", []string{"team-shared"}},
		{"unknown field", "/google.pubsub.v1.Publisher/Publish", append(smuggled, real...), "", []string{"private"}},
		{"no Project", "/google.pubsub.v1.Publisher/ListTopics", marshal(t, &pubsubpb.ListTopicsRequest{PageToken: "x"}), "", nil},
		{"service account email", "/google.iam.credentials.v1.IAMCredentials/GenerateAccessToken", marshal(t, &credentialspb.GenerateAccessTokenRequest{Name: "projects/-/serviceAccounts/sa@team-shared.iam.gserviceaccount.com"}), "", []string{"team-shared"}},
		{"gzip", "/google.pubsub.v1.Publisher/Publish", gzipped(t, marshal(t, topic)), "gzip", []string{"team-shared"}},
		{"unknown method", "/google.pubsub.v1.Publisher/Nope", marshal(t, topic), "", nil},
	}
	for _, c := range grpcs {
		r := grpcRequest(c.method, c.body, c.encoding)
		want, _ := io.ReadAll(grpcRequest(c.method, c.body, c.encoding).Body)
		if got := requestProjects(r); !slices.Equal(got, c.want) {
			t.Errorf("%s: names Projects %q, want %q", c.name, got, c.want)
		}
		// The message is put back for whoever serves the call.
		if b, _ := io.ReadAll(r.Body); !bytes.Equal(b, want) {
			t.Errorf("%s: body after reading the Projects = %x, want %x", c.name, b, want)
		}
	}
}

func marshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func gzipped(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, _ = w.Write(b)
	_ = w.Close()
	return buf.Bytes()
}

// grpcRequest is a gRPC call to method carrying the message b, compressed
// with encoding unless it is "", as the API gateway receives it.
func grpcRequest(method string, b []byte, encoding string) *http.Request {
	frame := make([]byte, 5, 5+len(b))
	if encoding != "" {
		frame[0] = 1
	}
	binary.BigEndian.PutUint32(frame[1:], uint32(len(b)))
	r := httptest.NewRequest(http.MethodPost, method, bytes.NewReader(append(frame, b...)))
	r.ProtoMajor = 2
	r.Header.Set("Content-Type", "application/grpc")
	if encoding != "" {
		r.Header.Set("Grpc-Encoding", encoding)
	}
	return r
}

// fakeLinks shares and delegates the Projects it lists.
type fakeLinks struct {
	shared    []string
	delegated map[string]string
}

func (f fakeLinks) Delegating() bool         { return len(f.delegated) > 0 }
func (f fakeLinks) DelegatedLinks() []string { return nil }
func (f fakeLinks) DelegatedTo(ref string) (string, bool) {
	l, ok := f.delegated[ref]
	return l, ok
}
func (f fakeLinks) Authenticate(context.Context, string) (string, bool) { return "", false }
func (f fakeLinks) Shared(_, ref string) bool                           { return slices.Contains(f.shared, ref) }
func (f fakeLinks) BucketOwner(string) (string, string, bool)           { return "", "", false }
func (f fakeLinks) RefreshBuckets(context.Context, string)              {}
func (f fakeLinks) Forward(w http.ResponseWriter, _ *http.Request, name, _ string, _ func(http.ResponseWriter, error), _ func(*http.Response)) {
	w.Header().Set("Forwarded-To", name)
}

// A Linked Instance serves a forwarded request only when every Project it
// names is shared with the link, and never the Admin API or Web console.
func TestServeLinkedOnlyServesSharedProjects(t *testing.T) {
	g := New(&emu.Env{Config: &config.Config{}, Log: slog.New(slog.DiscardHandler)}, nil)
	g.SetLinks(fakeLinks{shared: []string{"team-shared"}})
	served := false
	g.mux.HandleFunc("/", func(http.ResponseWriter, *http.Request) { served = true })
	for _, target := range []string{
		"/%5Femu/v1/reset?project=team-shared",
		"/_emu/v1/reset?project=team-shared",
		"/console/projects/team-shared",
		"/storage/v1/b/private-bucket/o?project=team-shared",
		"/v1/projects/team-shared/subscriptions/s/x/projects/other-proj",
		"/v1/projects/other-proj/topics",
	} {
		served = false
		w := httptest.NewRecorder()
		g.ServeLinked(w, httptest.NewRequest(http.MethodPost, target, nil), "laptop", "user:a@example.com")
		if served || w.Code != http.StatusForbidden {
			t.Errorf("%s over the link: served %v, %d %s", target, served, w.Code, w.Body)
		}
	}
	w := httptest.NewRecorder()
	g.ServeLinked(w, httptest.NewRequest(http.MethodGet, "/v1/projects/team-shared/topics", nil), "laptop", "user:a@example.com")
	if !served {
		t.Errorf("a request for the shared Project was not served: %d %s", w.Code, w.Body)
	}
}

// The front Instance forwards a request only when every Project it names
// is delegated to the same link, and refuses one that mixes Projects.
func TestFrontForwardsOnlyWholeRequests(t *testing.T) {
	g := New(&emu.Env{Config: &config.Config{}, Log: slog.New(slog.DiscardHandler)}, reqlog.New(10, slog.New(slog.DiscardHandler)))
	g.SetLinks(fakeLinks{delegated: map[string]string{"team-shared": "office", "other-shared": "other"}})
	g.mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.Header().Set("Served-Locally", "1") })
	for _, c := range []struct{ target, forwarded, local string }{
		{"/v1/projects/team-shared/topics", "office", ""},
		{"/v1/projects/local-proj/topics", "", "1"},
		{"/v1/projects/team-shared/x/projects/local-proj", "", ""},
		{"/v1/projects/team-shared/x/projects/other-shared", "", ""},
	} {
		w := httptest.NewRecorder()
		g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, c.target, nil))
		if w.Header().Get("Forwarded-To") != c.forwarded || w.Header().Get("Served-Locally") != c.local {
			t.Errorf("%s: forwarded to %q, served locally %q; want %q, %q (%d %s)", c.target,
				w.Header().Get("Forwarded-To"), w.Header().Get("Served-Locally"), c.forwarded, c.local, w.Code, w.Body)
		}
		if c.forwarded == "" && c.local == "" && w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400 for Projects on different Instances", c.target, w.Code)
		}
	}
}
