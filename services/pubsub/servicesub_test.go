package pubsub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

var _ emu.Subscriber = (*Service)(nil)

// TestServiceSubscription covers the push subscriptions another Service
// owns: they are pushed to in process, ensured idempotently, and deleted
// without IAM.
func TestServiceSubscription(t *testing.T) {
	svc, h, closeFn := openService(t, filepath.Join(t.TempDir(), "state.db"))
	defer closeFn()
	if err := svc.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const topic, sub = "projects/ss-proj/topics/builds", "projects/ss-proj/subscriptions/gcb-x"
	endpoint := "https://owner.googleapis.com/v1/x:webhook"
	if err := svc.EnsurePushSubscription(ctx, sub, topic, endpoint); apierr.From(err).Code != codes.NotFound {
		t.Fatalf("missing topic: %v", err)
	}
	do(t, h, http.MethodPut, "/v1/"+topic, nil)

	got := make(chan string, 4)
	svc.ServePushHost("owner.googleapis.com", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Message struct{ Data string } `json:"message"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		d, _ := base64.StdEncoding.DecodeString(body.Message.Data)
		got <- r.URL.Path + " " + string(d)
	}))
	for range 2 {
		if err := svc.EnsurePushSubscription(ctx, sub, topic, endpoint); err != nil {
			t.Fatalf("EnsurePushSubscription: %v", err)
		}
	}
	if _, err := svc.PublishInternal(ctx, topic, []byte("hello"), nil); err != nil {
		t.Fatal(err)
	}
	select {
	case g := <-got:
		if g != "/v1/x:webhook hello" {
			t.Fatalf("pushed %q", g)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no push")
	}

	if out, err := svc.ExportSeed(ctx); err != nil || len(out.(seedOut).Subscriptions) != 0 {
		t.Fatalf("ExportSeed exports the owned subscription: %+v, %v", out, err)
	}

	for range 2 {
		if err := svc.DeleteSubscriptionInternal(ctx, sub); err != nil {
			t.Fatalf("DeleteSubscriptionInternal: %v", err)
		}
	}
	if code, _ := doStatus(t, h, http.MethodGet, "/v1/"+sub, ""); code != http.StatusNotFound {
		t.Fatalf("subscription after delete: %d", code)
	}
}
