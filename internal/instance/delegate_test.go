package instance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/fault"
	"github.com/linuxuser586/gcpemu/internal/link"
	"github.com/linuxuser586/gcpemu/services/iam"
	ps "github.com/linuxuser586/gcpemu/services/pubsub"
)

const shared = "team-shared"

// startWithProjects starts an Ephemeral Instance named name running the
// pubsub and iam Services, its link listener on loopback.
func startWithProjects(t *testing.T, name string, change func(*config.Config)) *Instance {
	t.Helper()
	cfg := config.Defaults()
	cfg.Instance = name
	cfg.DataDir = t.TempDir()
	cfg.Ephemeral = true
	cfg.Services = []string{"iam", "pubsub"}
	cfg.Ports[config.AllPorts] = 0
	cfg.LinkBind = []string{"127.0.0.1"}
	if change != nil {
		change(&cfg)
	}
	in, err := New(&cfg, map[string]Factory{"iam": iam.New, "pubsub": ps.New}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := in.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := in.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = in.Shutdown(context.Background()) })
	return in
}

// pairInstances pairs front with linked, approved on both sides.
func pairInstances(t *testing.T, front, linked *Instance) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	addr := linked.Env.Endpoints.Get(link.Endpoint)
	id, err := front.link.Identify(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := front.link.Pair(ctx, addr, id.Fingerprint, "")
		done <- err
	}()
	for i := 0; ; i++ {
		if reqs := linked.link.Requests(); len(reqs) > 0 {
			if _, err := linked.link.Approve(reqs[0].ID, ""); err != nil {
				t.Fatal(err)
			}
			break
		}
		if i == 500 {
			t.Fatal("no Pairing request arrived")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// adminStatus calls in's Admin API and returns the status and body.
func adminStatus(t *testing.T, in *Instance, method, path string, body any) (int, string) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://"+in.Env.Endpoints.Get("gateway")+path, r)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// delegated pairs a laptop with an office that shares team-shared, which
// the laptop delegates.
func delegated(t *testing.T, laptopCfg, officeCfg func(*config.Config)) (laptop, office *Instance) {
	t.Helper()
	laptop, office = startWithProjects(t, "laptop", laptopCfg), startWithProjects(t, "office", officeCfg)
	pairInstances(t, laptop, office)
	callAdmin(t, office, http.MethodPost, "/_emu/v1/links/shares", map[string]string{"project": shared}, nil)
	callAdmin(t, laptop, http.MethodPost, "/_emu/v1/links/delegations", map[string]string{"project": shared, "link": "office"}, nil)
	return laptop, office
}

// pubsubClient is a Go SDK Pub/Sub client of in's API gateway.
func pubsubClient(t *testing.T, in *Instance) *pubsub.Client {
	t.Helper()
	c, err := pubsub.NewClient(context.Background(), shared, option.WithEndpoint(in.Env.Endpoints.Get("gateway")),
		option.WithoutAuthentication(), option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// rawPublisher is a gRPC Publisher client of in's API gateway that, unlike
// the SDK's, does not retry UNAVAILABLE or ABORTED.
func rawPublisher(t *testing.T, in *Instance) pubsubpb.PublisherClient {
	t.Helper()
	conn, err := grpc.NewClient(in.Env.Endpoints.Get("gateway"), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return pubsubpb.NewPublisherClient(conn)
}

func topicNames(t *testing.T, c *pubsub.Client) []string {
	t.Helper()
	var out []string
	it := c.TopicAdminClient.ListTopics(context.Background(), &pubsubpb.ListTopicsRequest{Project: "projects/" + shared})
	for {
		tp, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out
		}
		if err != nil {
			t.Fatalf("list topics: %v", err)
		}
		out = append(out, tp.Name)
	}
}

// restGet gets path from in's API gateway, returning the status and body.
func restGet(t *testing.T, in *Instance, method, path string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, "http://"+in.Env.Endpoints.Get("gateway")+path, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// With team-shared delegated, a client of the laptop creates, lists and
// deletes resources in it, over gRPC and REST, and they exist only on the
// office. The laptop's Reset and State snapshots leave them alone, its
// Request log marks the forwarded requests, and its fault rules still
// match them (ADR 0027).
func TestDelegatedProjectIsForwarded(t *testing.T) {
	laptop, office := delegated(t, nil, nil)
	ctx := context.Background()
	lc, oc := pubsubClient(t, laptop), pubsubClient(t, office)
	topic := "projects/" + shared + "/topics/topic-a"

	if _, err := lc.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatalf("create through the laptop: %v", err)
	}
	sub := "projects/" + shared + "/subscriptions/sub-a"
	if _, err := lc.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic}); err != nil {
		t.Fatalf("create subscription through the laptop: %v", err)
	}
	if id, err := lc.Publisher(topic).Publish(ctx, &pubsub.Message{Data: []byte("hi")}).Get(ctx); err != nil || id == "" {
		t.Fatalf("publish through the laptop: %q %v", id, err)
	}
	// StreamingPull, a bidirectional stream, through the laptop.
	rctx, stop := context.WithTimeout(ctx, 20*time.Second)
	var got []byte
	err := lc.Subscriber(sub).Receive(rctx, func(_ context.Context, m *pubsub.Message) {
		got = m.Data
		m.Ack()
		stop()
	})
	stop()
	if err != nil || string(got) != "hi" {
		t.Errorf("receive through the laptop = %q, %v", got, err)
	}
	if got := topicNames(t, lc); !slices.Equal(got, []string{topic}) {
		t.Errorf("topics through the laptop = %v", got)
	}
	if got := topicNames(t, oc); !slices.Equal(got, []string{topic}) {
		t.Errorf("topics on the office = %v", got)
	}
	if code, body := restGet(t, laptop, http.MethodGet, "/pubsub/v1/projects/"+shared+"/topics"); code != http.StatusOK || !strings.Contains(body, topic) {
		t.Errorf("REST list through the laptop = %d %s", code, body)
	}
	if slices.Contains(laptop.Env.Projects(), shared) {
		t.Error("the laptop created the Delegated Project locally")
	}

	var delegatedEntries int
	for _, e := range laptop.Log.Entries("") {
		if e.Delegated == "office" {
			delegatedEntries++
		}
	}
	if delegatedEntries == 0 {
		t.Error("the laptop's Request log does not mark forwarded requests")
	}

	if _, err := laptop.Faults().Add(fault.Rule{Service: "pubsub", Method: "GetTopic", Code: "ABORTED"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rawPublisher(t, laptop).GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: topic}); status.Code(err) != codes.Aborted {
		t.Errorf("GetTopic under a fault rule = %v, want ABORTED", err)
	}

	callAdmin(t, laptop, http.MethodPost, "/_emu/v1/reset", nil, nil)
	callAdmin(t, laptop, http.MethodPost, "/_emu/v1/snapshots", map[string]string{"name": "before"}, nil)
	callAdmin(t, laptop, http.MethodPost, "/_emu/v1/snapshots/before/restore", nil, nil)
	if got := topicNames(t, oc); !slices.Equal(got, []string{topic}) {
		t.Errorf("topics on the office after the laptop's Reset and restore = %v", got)
	}
	if got := topicNames(t, lc); !slices.Equal(got, []string{topic}) {
		t.Errorf("topics through the laptop after its Reset and restore = %v; the delegation is configuration", got)
	}

	if code, body := restGet(t, laptop, http.MethodDelete, "/pubsub/v1/"+topic); code != http.StatusOK {
		t.Fatalf("REST delete through the laptop = %d %s", code, body)
	}
	if got := topicNames(t, oc); len(got) != 0 {
		t.Errorf("topics on the office after delete = %v", got)
	}
}

// A caller the office's IAM denies gets PERMISSION_DENIED through the
// laptop, naming the Principal the laptop asserted.
func TestDelegatedProjectDeniedByLinkedIAM(t *testing.T) {
	laptop, _ := delegated(t,
		func(c *config.Config) { c.DefaultPrincipal = "user:alice@example.com" },
		func(c *config.Config) { c.IAMMode = config.IAMEnforce })
	_, err := pubsubClient(t, laptop).TopicAdminClient.GetTopic(context.Background(), &pubsubpb.GetTopicRequest{Topic: "projects/" + shared + "/topics/topic-a"})
	if status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "user:alice@example.com") {
		t.Errorf("GetTopic = %v, want PERMISSION_DENIED naming user:alice@example.com", err)
	}
}

// A Project the laptop already has, or one not shared with it, cannot be
// delegated.
func TestDelegationRefusals(t *testing.T) {
	laptop, office := startWithProjects(t, "laptop", nil), startWithProjects(t, "office", nil)
	pairInstances(t, laptop, office)
	if code, body := adminStatus(t, laptop, http.MethodPost, "/_emu/v1/links/delegations", map[string]string{"project": shared, "link": "office"}); code != http.StatusBadRequest || !strings.Contains(body, "not shared") {
		t.Errorf("delegating an unshared Project = %d %s", code, body)
	}
	if _, err := pubsubClient(t, laptop).TopicAdminClient.CreateTopic(context.Background(), &pubsubpb.Topic{Name: "projects/laptop-own/topics/topic-a"}); err != nil {
		t.Fatal(err)
	}
	callAdmin(t, office, http.MethodPost, "/_emu/v1/links/shares", map[string]string{"project": "laptop-own"}, nil)
	if code, body := adminStatus(t, laptop, http.MethodPost, "/_emu/v1/links/delegations", map[string]string{"project": "laptop-own", "link": "office"}); code != http.StatusBadRequest || !strings.Contains(body, "already has") {
		t.Errorf("delegating a Project the laptop has = %d %s", code, body)
	}
}

// Under --strict-projects, the Delegated Project counts as existing: the
// laptop forwards requests for it. Anything that would create state for
// it on the laptop is refused instead, naming the link. While the office
// is down, requests for team-shared fail with UNAVAILABLE naming it,
// create nothing on the laptop, and leave the laptop ready.
func TestDelegatedProjectWhileLinkedInstanceIsDown(t *testing.T) {
	laptop, office := delegated(t, func(c *config.Config) { c.StrictProjects = true }, nil)
	if _, err := rawPublisher(t, laptop).CreateTopic(context.Background(), &pubsubpb.Topic{Name: "projects/" + shared + "/topics/topic-z"}); err != nil {
		t.Errorf("CreateTopic in the Delegated Project under --strict-projects = %v", err)
	}
	if err := laptop.Env.EnsureProject(shared); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), `"office"`) {
		t.Errorf("EnsureProject of the Delegated Project = %v, want FAILED_PRECONDITION naming the link", err)
	}
	if err := office.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := rawPublisher(t, laptop).CreateTopic(context.Background(), &pubsubpb.Topic{Name: "projects/" + shared + "/topics/topic-a"})
	if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), `"office"`) {
		t.Errorf("CreateTopic with the office down = %v, want UNAVAILABLE naming it", err)
	}
	if code, body := restGet(t, laptop, http.MethodGet, "/pubsub/v1/projects/"+shared+"/topics"); code != http.StatusServiceUnavailable || !strings.Contains(body, `\"office\"`) {
		t.Errorf("REST list with the office down = %d %s", code, body)
	}
	if slices.Contains(laptop.Env.Projects(), shared) {
		t.Error("the laptop created the Delegated Project locally")
	}
	if !laptop.Ready() {
		t.Errorf("the laptop is not ready: %v", laptop.Readiness())
	}
}
