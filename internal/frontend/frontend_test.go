package frontend_test

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/frontend"
)

// dialFrontend returns a dialer that sends every connection to the
// frontend, as DNS pointing the real hostnames at it would.
func dialFrontend(addr string) func(ctx context.Context, network, _ string) (net.Conn, error) {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
}

// TestFrontendRealClients drives the unmodified storage (JSON over HTTP/2)
// and Pub/Sub (gRPC) clients at their default endpoints, with only the
// dialer redirected to the frontend (FR-INT-007, FR-CORE-043).
func TestFrontendRealClients(t *testing.T) {
	inst := emutest.Start(t, []string{"gcs", "pubsub", "ar"})
	fe := inst.Endpoint(frontend.Endpoint)
	if fe == "" {
		t.Fatal("no frontend endpoint")
	}
	pool := inst.Env.CA.Pool()
	ctx := context.Background()

	tr := &http.Transport{
		DialContext:       dialFrontend(fe),
		TLSClientConfig:   &tls.Config{RootCAs: pool},
		ForceAttemptHTTP2: true,
	}
	hc := &http.Client{Transport: tr, Timeout: 30 * time.Second}

	t.Run("Storage", func(t *testing.T) {
		sc, err := storage.NewClient(ctx, option.WithHTTPClient(hc))
		if err != nil {
			t.Fatal(err)
		}
		defer sc.Close()
		b := sc.Bucket("fe-bucket")
		if err := b.Create(ctx, "fe-project", nil); err != nil {
			t.Fatalf("create bucket: %v", err)
		}
		w := b.Object("hello.txt").NewWriter(ctx)
		_, _ = io.WriteString(w, "hello via storage.googleapis.com")
		if err := w.Close(); err != nil {
			t.Fatalf("upload: %v", err)
		}
		r, err := b.Object("hello.txt").NewReader(ctx)
		if err != nil {
			t.Fatalf("download: %v", err)
		}
		got, _ := io.ReadAll(r)
		r.Close()
		if string(got) != "hello via storage.googleapis.com" {
			t.Errorf("object = %q", got)
		}
		resp, err := hc.Get("https://storage.googleapis.com/storage/v1/b/fe-bucket")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 || resp.ProtoMajor != 2 {
			t.Errorf("GET bucket = %d over %s, want 200 over HTTP/2", resp.StatusCode, resp.Proto)
		}
	})

	t.Run("PubSubGRPC", func(t *testing.T) {
		pc, err := pubsub.NewClient(ctx, "fe-project", option.WithoutAuthentication(),
			option.WithGRPCDialOption(grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return dialFrontend(fe)(ctx, "tcp", "")
			})),
			option.WithGRPCDialOption(grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: pool}))))
		if err != nil {
			t.Fatal(err)
		}
		defer pc.Close()
		topic := "projects/fe-project/topics/fe-topic"
		if _, err := pc.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
			t.Fatalf("create topic over gRPC/TLS: %v", err)
		}
		id, err := pc.Publisher(topic).Publish(ctx, &pubsub.Message{Data: []byte("hi")}).Get(ctx)
		if err != nil || id == "" {
			t.Fatalf("publish: %q %v", id, err)
		}
	})

	t.Run("Registry", func(t *testing.T) {
		resp, err := hc.Get("https://us-docker.pkg.dev/v2/")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 && resp.StatusCode != 401 {
			t.Errorf("GET /v2/ on us-docker.pkg.dev = %d", resp.StatusCode)
		}
		if resp.Header.Get("Docker-Distribution-Api-Version") == "" {
			t.Errorf("not answered by the registry: %v", resp.Header)
		}
	})

	t.Run("UnservedName", func(t *testing.T) {
		_, err := hc.Get("https://logging.googleapis.com/")
		if err == nil || !strings.Contains(err.Error(), "tls") {
			t.Errorf("unserved name: err = %v, want a TLS failure", err)
		}
	})

	t.Run("NoSNI", func(t *testing.T) {
		// Clients dialing an IP (kube-apiserver → admission webhook) send
		// no SNI; the certificate covers the host's addresses.
		c, err := tls.Dial("tcp", fe, &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"})
		if err != nil {
			t.Fatalf("no-SNI handshake: %v", err)
		}
		c.Close()
	})
}

func TestServes(t *testing.T) {
	hosts := []string{"pubsub.googleapis.com", "storage.googleapis.com"}
	for name, want := range map[string]bool{
		"storage.googleapis.com":         true,
		"Storage.GoogleAPIs.com.":        true,
		"us-docker.pkg.dev":              true,
		"europe-west1-docker.pkg.dev":    true,
		"docker.pkg.dev":                 false,
		"a.b-docker.pkg.dev":             false,
		"us-python.pkg.dev":              false,
		"logging.googleapis.com":         false,
		"bucket.storage.googleapis.com":  false,
		"storage.googleapis.com.evil.io": false,
	} {
		if got := frontend.Serves(hosts, name); got != want {
			t.Errorf("Serves(%q) = %v, want %v", name, got, want)
		}
	}
}
