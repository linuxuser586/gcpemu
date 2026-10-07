package pubsub_test

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// fakeIAM stands in for the IAM service as the OIDC token minter.
type fakeIAM struct {
	emu.Service
	mu        sync.Mutex
	audiences []string
	emails    []string
}

func (f *fakeIAM) PublicKeys(context.Context, string) ([]*rsa.PublicKey, error) { return nil, nil }
func (f *fakeIAM) SignBlob(context.Context, string, []byte) (string, []byte, error) {
	return "", nil, nil
}
func (f *fakeIAM) AccessToken(context.Context, emu.Principal) (string, int, error) { return "", 0, nil }
func (f *fakeIAM) IDToken(_ context.Context, email, aud string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.emails = append(f.emails, email)
	f.audiences = append(f.audiences, aud)
	return "fake-id-token", nil
}

// installFakeIAM replaces the "iam" peer seen through env.Lookup.
func installFakeIAM(inst *emutest.Instance) *fakeIAM {
	m := map[string]emu.Service{}
	for _, s := range inst.Services() {
		m[s.Name()] = s
	}
	f := &fakeIAM{Service: m["iam"]}
	m["iam"] = f
	inst.Env.SetServices(m)
	return f
}

type pushed struct {
	header http.Header
	body   []byte
}

func TestPushWrapped(t *testing.T) {
	inst, c := newEmulatorClient(t)
	fake := installFakeIAM(inst)

	var mu sync.Mutex
	var reqs []pushed
	got := make(chan struct{}, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, pushed{r.Header.Clone(), b})
		n := len(reqs)
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable) // first attempt fails → retried
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
		got <- struct{}{}
	}))
	defer srv.Close()

	mustTopic(t, c, "push-topic")
	sub := subName("push-sub")
	mustSub(t, c, &pubsubpb.Subscription{
		Name: sub, Topic: topicName("push-topic"),
		PushConfig: &pubsubpb.PushConfig{
			PushEndpoint:         srv.URL + "/handler",
			AuthenticationMethod: &pubsubpb.PushConfig_OidcToken_{OidcToken: &pubsubpb.PushConfig_OidcToken{ServiceAccountEmail: "pusher@test-proj.iam.gserviceaccount.com"}},
		},
		RetryPolicy:      &pubsubpb.RetryPolicy{MinimumBackoff: durationpb.New(100 * time.Millisecond), MaximumBackoff: durationpb.New(time.Second)},
		DeadLetterPolicy: &pubsubpb.DeadLetterPolicy{DeadLetterTopic: topicName("push-topic"), MaxDeliveryAttempts: 10},
	})
	ids := publish(t, c, topicName("push-topic"), &pubsub.Message{Data: []byte("hello"), Attributes: map[string]string{"k": "v"}})

	for range 2 {
		select {
		case <-got:
		case <-time.After(10 * time.Second):
			t.Fatal("push not delivered")
		}
	}
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(reqs) != 2 {
		t.Fatalf("push requests = %d, want 2 (fail then ack)", len(reqs))
	}
	r := reqs[1]
	if r.header.Get("Authorization") != "Bearer fake-id-token" {
		t.Errorf("authorization = %q", r.header.Get("Authorization"))
	}
	if fake.audiences[0] != srv.URL+"/handler" || fake.emails[0] != "pusher@test-proj.iam.gserviceaccount.com" {
		t.Errorf("IDToken(%v, %v)", fake.emails, fake.audiences)
	}
	var body struct {
		Message struct {
			Data        string            `json:"data"`
			Attributes  map[string]string `json:"attributes"`
			MessageID   string            `json:"messageId"`
			MessageID2  string            `json:"message_id"`
			PublishTime string            `json:"publishTime"`
		} `json:"message"`
		Subscription    string `json:"subscription"`
		DeliveryAttempt int    `json:"deliveryAttempt"`
	}
	if err := json.Unmarshal(r.body, &body); err != nil {
		t.Fatal(err)
	}
	data, _ := base64.StdEncoding.DecodeString(body.Message.Data)
	if string(data) != "hello" || body.Message.Attributes["k"] != "v" || body.Message.MessageID != ids[0] ||
		body.Message.MessageID2 != ids[0] || body.Subscription != sub || body.Message.PublishTime == "" {
		t.Errorf("body = %s", r.body)
	}
	if body.DeliveryAttempt != 2 {
		t.Errorf("deliveryAttempt = %d, want 2", body.DeliveryAttempt)
	}
}

func TestPushUnwrapped(t *testing.T) {
	_, c := newEmulatorClient(t)
	got := make(chan pushed, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- pushed{r.Header.Clone(), b}
	}))
	defer srv.Close()
	mustTopic(t, c, "push-topic")
	sub := subName("raw-sub")
	mustSub(t, c, &pubsubpb.Subscription{Name: sub, Topic: topicName("push-topic"), PushConfig: &pubsubpb.PushConfig{
		PushEndpoint: srv.URL,
		Wrapper:      &pubsubpb.PushConfig_NoWrapper_{NoWrapper: &pubsubpb.PushConfig_NoWrapper{WriteMetadata: true}},
	}})
	ids := publish(t, c, topicName("push-topic"), &pubsub.Message{Data: []byte("raw-bytes"), Attributes: map[string]string{"color": "red"}})
	select {
	case p := <-got:
		if string(p.body) != "raw-bytes" {
			t.Errorf("body = %q", p.body)
		}
		if p.header.Get("X-Goog-Pubsub-Message-Id") != ids[0] || p.header.Get("X-Goog-Pubsub-Subscription-Name") != sub ||
			p.header.Get("Color") != "red" || p.header.Get("X-Goog-Pubsub-Publish-Time") == "" {
			t.Errorf("headers = %v", p.header)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("push not delivered")
	}
	// Switching to pull stops pushing (ModifyPushConfig with an empty config).
	if err := c.SubscriptionAdminClient.ModifyPushConfig(context.Background(), &pubsubpb.ModifyPushConfigRequest{Subscription: sub, PushConfig: &pubsubpb.PushConfig{}}); err != nil {
		t.Fatal(err)
	}
	publish(t, c, topicName("push-topic"), &pubsub.Message{Data: []byte("pulled")})
	if m := pullWait(t, c, sub, 1, 5*time.Second); len(m) != 1 || string(m[0].Message.Data) != "pulled" {
		t.Fatalf("pull after push disabled: %v", m)
	}
}
