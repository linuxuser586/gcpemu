package secrets_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// pullEvents drains a Pub/Sub subscription and returns its eventType
// attributes, sorted.
func (e *env) pullEvents(sub string) []string {
	e.t.Helper()
	var out []string
	for {
		var resp struct {
			ReceivedMessages []struct {
				AckID   string `json:"ackId"`
				Message struct {
					Data       string
					Attributes map[string]string
				}
			}
		}
		if c := e.rest("POST", "/pubsub/v1/"+parent+"/subscriptions/"+sub+":pull", map[string]any{"maxMessages": 100, "returnImmediately": true}, &resp); c != 200 {
			e.t.Fatalf("pull: %d", c)
		}
		if len(resp.ReceivedMessages) == 0 {
			sort.Strings(out)
			return out
		}
		var acks []string
		for _, m := range resp.ReceivedMessages {
			a := m.Message.Attributes
			if a["dataFormat"] != "JSON_API_V1" || !strings.HasPrefix(a["secretId"], numbered+"/secrets/") {
				e.t.Fatalf("attributes %v", a)
			}
			data, _ := base64.StdEncoding.DecodeString(m.Message.Data)
			if !json.Valid(data) {
				e.t.Fatalf("data %q", data)
			}
			out = append(out, a["eventType"])
			acks = append(acks, m.AckID)
		}
		e.rest("POST", "/pubsub/v1/"+parent+"/subscriptions/"+sub+":acknowledge", map[string]any{"ackIds": acks}, nil)
	}
}

// near reports whether a is within a few seconds after b (the clock keeps
// running between reading it and the server's use of it).
func near(a, b time.Time) bool { d := a.Sub(b); return d >= 0 && d < 5*time.Second }

// Expiry, rotation notifications and delayed destruction follow the
// Emulator clock; every change is published to the secret's topics.
func TestTimeDrivenAndEvents(t *testing.T) {
	e := start(t, []string{"pubsub"})
	topic := parent + "/topics/sm-events"
	var tr map[string]any
	if c := e.rest("PUT", "/pubsub/v1/"+topic, map[string]any{}, &tr); c != 200 {
		t.Fatalf("topic: %d %v", c, tr)
	}
	if c := e.rest("PUT", "/pubsub/v1/"+parent+"/subscriptions/sm-events", map[string]any{"topic": topic}, nil); c != 200 {
		t.Fatalf("subscription: %d", c)
	}
	topics := []*secretmanagerpb.Topic{{Name: topic}}

	_, err := e.sm.CreateSecret(e.ctx, &secretmanagerpb.CreateSecretRequest{Parent: parent, SecretId: "x", Secret: &secretmanagerpb.Secret{Replication: automatic,
		Topics: []*secretmanagerpb.Topic{{Name: parent + "/topics/missing"}}}})
	wantCode(t, "missing topic", err, codes.NotFound)
	_, err = e.sm.CreateSecret(e.ctx, &secretmanagerpb.CreateSecretRequest{Parent: parent, SecretId: "x", Secret: &secretmanagerpb.Secret{Replication: automatic,
		Rotation: &secretmanagerpb.Rotation{NextRotationTime: timestamppb.New(time.Now().Add(time.Hour))}}})
	wantCode(t, "rotation without topics", err, codes.InvalidArgument)
	_, err = e.sm.CreateSecret(e.ctx, &secretmanagerpb.CreateSecretRequest{Parent: parent, SecretId: "x", Secret: &secretmanagerpb.Secret{Replication: automatic, Topics: topics,
		Rotation: &secretmanagerpb.Rotation{NextRotationTime: timestamppb.New(time.Now().Add(time.Minute))}}})
	wantCode(t, "rotation too soon", err, codes.InvalidArgument)

	now := e.inst.Env.Clock.Now()
	rot := must(e.sm.CreateSecret(e.ctx, &secretmanagerpb.CreateSecretRequest{Parent: parent, SecretId: "rot", Secret: &secretmanagerpb.Secret{
		Replication: automatic, Topics: topics,
		Rotation:          &secretmanagerpb.Rotation{NextRotationTime: timestamppb.New(now.Add(time.Hour)), RotationPeriod: durationpb.New(24 * time.Hour)},
		VersionDestroyTtl: durationpb.New(48 * time.Hour),
	}}))
	exp := must(e.sm.CreateSecret(e.ctx, &secretmanagerpb.CreateSecretRequest{Parent: parent, SecretId: "exp", Secret: &secretmanagerpb.Secret{
		Replication: automatic, Topics: topics, Expiration: &secretmanagerpb.Secret_Ttl{Ttl: durationpb.New(2 * time.Hour)}}}))
	if !near(exp.GetExpireTime().AsTime(), now.Add(2*time.Hour)) {
		t.Fatalf("ttl became expire_time %v", exp.GetExpireTime())
	}
	v := must(e.sm.AddSecretVersion(e.ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: rot.GetName(), Payload: &secretmanagerpb.SecretPayload{Data: []byte("a")}}))
	sched := must(e.sm.DestroySecretVersion(e.ctx, &secretmanagerpb.DestroySecretVersionRequest{Name: v.GetName()}))
	if sched.GetState() != secretmanagerpb.SecretVersion_DISABLED || !near(sched.GetScheduledDestroyTime().AsTime(), now.Add(48*time.Hour)) {
		t.Fatalf("scheduled destroy %v", sched)
	}
	want := "SECRET_CREATE,SECRET_CREATE,SECRET_VERSION_ADD,SECRET_VERSION_DESTROY_SCHEDULED,TOPIC_CONFIGURED,TOPIC_CONFIGURED"
	if got := strings.Join(e.pullEvents("sm-events"), ","); got != want {
		t.Fatalf("events %s, want %s", got, want)
	}

	// +3h: the rotation is due (next moves on by the period) and the TTL
	// secret has expired.
	e.advance(3 * time.Hour)
	got := must(e.sm.GetSecret(e.ctx, &secretmanagerpb.GetSecretRequest{Name: rot.GetName()}))
	if !got.GetRotation().GetNextRotationTime().AsTime().Equal(now.Add(25 * time.Hour)) {
		t.Fatalf("next rotation %v", got.GetRotation().GetNextRotationTime().AsTime())
	}
	_, err = e.sm.GetSecret(e.ctx, &secretmanagerpb.GetSecretRequest{Name: exp.GetName()})
	wantCode(t, "expired secret", err, codes.NotFound)
	if got := strings.Join(e.pullEvents("sm-events"), ","); got != "SECRET_DELETE,SECRET_ROTATE" {
		t.Fatalf("events after 3h: %s", got)
	}

	// +2d: the scheduled destruction happens; another rotation is due.
	e.advance(48 * time.Hour)
	des := must(e.sm.GetSecretVersion(e.ctx, &secretmanagerpb.GetSecretVersionRequest{Name: v.GetName()}))
	if des.GetState() != secretmanagerpb.SecretVersion_DESTROYED {
		t.Fatalf("after ttl %v", des)
	}
	if got := strings.Join(e.pullEvents("sm-events"), ","); got != "SECRET_ROTATE,SECRET_VERSION_DESTROY" {
		t.Fatalf("events after 2d: %s", got)
	}

	// Enabling a version scheduled for destruction cancels it.
	v2 := must(e.sm.AddSecretVersion(e.ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: rot.GetName(), Payload: &secretmanagerpb.SecretPayload{Data: []byte("b")}}))
	must(e.sm.DestroySecretVersion(e.ctx, &secretmanagerpb.DestroySecretVersionRequest{Name: v2.GetName()}))
	en := must(e.sm.EnableSecretVersion(e.ctx, &secretmanagerpb.EnableSecretVersionRequest{Name: v2.GetName()}))
	if en.GetScheduledDestroyTime() != nil {
		t.Fatalf("enable kept the schedule: %v", en)
	}
	e.advance(72 * time.Hour)
	if st := must(e.sm.GetSecretVersion(e.ctx, &secretmanagerpb.GetSecretVersionRequest{Name: v2.GetName()})).GetState(); st != secretmanagerpb.SecretVersion_ENABLED {
		t.Fatalf("cancelled destruction ran: %v", st)
	}
	if c := e.rest("GET", "/secretmanager/v1/"+parent+"/secrets/rot", nil, nil); c != http.StatusOK {
		t.Fatalf("REST get: %d", c)
	}
}
