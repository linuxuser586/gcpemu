// Package external_test uses emutest as another Go module would: it imports
// only emutest and public Google client libraries, and shares one Instance
// across its tests from TestMain.
package external_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	dns "google.golang.org/api/dns/v1"

	"github.com/linuxuser586/gcpemu/emutest"
)

const project = "external-proj"

var inst *emutest.Instance

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

// run shares one Instance across the package's tests: a Seed file declares
// the project, and strict projects refuse any other.
func run(m *testing.M) int {
	dir, err := os.MkdirTemp("", "emutest-external-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)
	seed := filepath.Join(dir, "seed.yaml")
	if err := os.WriteFile(seed, []byte("projects: ["+project+"]\n"), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	inst, err = emutest.New(ctx, []string{"gcs", "pubsub", "secrets", "dns"},
		emutest.WithSeed(seed), emutest.WithStrictProjects())
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	code := m.Run()
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := inst.Close(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	return code
}

func TestStorage(t *testing.T) {
	ctx := context.Background()
	sc := inst.Storage(t)
	b := sc.Bucket("external-bucket")
	if err := b.Create(ctx, project, nil); err != nil {
		t.Fatal(err)
	}
	w := b.Object("greeting.txt").NewWriter(ctx)
	if _, err := io.WriteString(w, "hello"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("upload: %v", err)
	}
	r, err := b.Object("greeting.txt").NewReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil || string(got) != "hello" {
		t.Fatalf("read = %q, %v; want hello", got, err)
	}
}

func TestPubSub(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := inst.PubSub(t, project)
	topic := "projects/" + project + "/topics/external-topic"
	sub := "projects/" + project + "/subscriptions/external-sub"
	if _, err := c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic}); err != nil {
		t.Fatal(err)
	}
	p := c.Publisher(topic)
	defer p.Stop()
	if _, err := p.Publish(ctx, &pubsub.Message{Data: []byte("hi")}).Get(ctx); err != nil {
		t.Fatalf("publish: %v", err)
	}
	rctx, stop := context.WithCancel(ctx)
	var got string
	err := c.Subscriber(sub).Receive(rctx, func(_ context.Context, m *pubsub.Message) {
		got = string(m.Data)
		m.Ack()
		stop()
	})
	if err != nil || got != "hi" {
		t.Fatalf("receive = %q, %v; want hi", got, err)
	}
}

func TestGRPCOptions(t *testing.T) {
	ctx := context.Background()
	sm, err := secretmanager.NewClient(ctx, inst.GRPCOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	defer sm.Close()
	s, err := sm.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
		Parent:   "projects/" + project,
		SecretId: "external-secret",
		Secret:   &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Secret Manager names the secret by its project's number.
	if _, err := sm.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: s.Name}); err != nil || !strings.HasSuffix(s.Name, "/secrets/external-secret") {
		t.Errorf("secret %q: %v", s.Name, err)
	}
}

func TestHTTPOptions(t *testing.T) {
	ctx := context.Background()
	opts, err := inst.HTTPOptions("dns")
	if err != nil {
		t.Fatal(err)
	}
	svc, err := dns.NewService(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ManagedZones.Create(project, &dns.ManagedZone{Name: "external-zone", DnsName: "external.test.", Description: "d"}).Do(); err != nil {
		t.Fatal(err)
	}
	zones, err := svc.ManagedZones.List(project).Do()
	if err != nil {
		t.Fatal(err)
	}
	if len(zones.ManagedZones) != 1 || zones.ManagedZones[0].Name != "external-zone" {
		t.Errorf("zones = %+v, want external-zone", zones.ManagedZones)
	}
}

func TestHTTPOptionsUnknownAPI(t *testing.T) {
	if _, err := inst.HTTPOptions("nosuchapi"); err == nil {
		t.Error("HTTPOptions(nosuchapi): no error")
	}
	if _, err := inst.HTTPOptions("compute"); err == nil {
		t.Error("HTTPOptions(compute) on an Instance without compute: no error")
	}
}
