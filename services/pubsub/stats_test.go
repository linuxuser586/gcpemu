package pubsub_test

import (
	"encoding/base64"
	"net/http"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/emutest"
)

// TestStats checks the subscription statistics the Web console shows
// (GET /_emu/v1/pubsub/subscriptions): backlog, oldest unacked message
// and dead-lettered messages, and that seeking to the reported now
// purges the backlog.
func TestStats(t *testing.T) {
	inst := emutest.Start(t, []string{"pubsub"})
	v1 := inst.GatewayURL() + "/pubsub/v1/projects/" + project
	stats := func(project string) map[string]any {
		t.Helper()
		return restCall(t, http.MethodGet, inst.GatewayURL()+"/_emu/v1/pubsub/subscriptions?project="+project, nil, 200)
	}
	sub := func(out map[string]any, name string) map[string]any {
		t.Helper()
		for _, s := range out["subscriptions"].([]any) {
			if m := s.(map[string]any); m["name"] == subName(name) {
				return m
			}
		}
		t.Fatalf("no %s in %v", name, out)
		return nil
	}

	restCall(t, http.MethodPut, v1+"/topics/src", nil, 200)
	restCall(t, http.MethodPut, v1+"/topics/dlq", nil, 200)
	restCall(t, http.MethodPut, v1+"/subscriptions/dlq-sub", map[string]any{"topic": topicName("dlq")}, 200)
	restCall(t, http.MethodPut, v1+"/subscriptions/src-sub", map[string]any{
		"topic":            topicName("src"),
		"deadLetterPolicy": map[string]any{"deadLetterTopic": topicName("dlq"), "maxDeliveryAttempts": 5},
	}, 200)
	restCall(t, http.MethodPut, inst.GatewayURL()+"/pubsub/v1/projects/other-proj/topics/elsewhere", nil, 200)

	empty := sub(stats(project), "src-sub")
	if empty["backlog"] != float64(0) || empty["oldestUnackedPublishTime"] != nil {
		t.Fatalf("empty = %v", empty)
	}
	data := base64.StdEncoding.EncodeToString([]byte("poison"))
	restCall(t, http.MethodPost, v1+"/topics/src:publish", map[string]any{"messages": []map[string]any{{"data": data}}}, 200)
	restCall(t, http.MethodPost, v1+"/topics/src:publish", map[string]any{"messages": []map[string]any{{"data": data}}}, 200)

	out := stats(project)
	if n := len(out["subscriptions"].([]any)); n != 2 {
		t.Errorf("%d subscriptions, want only the Project's 2", n)
	}
	st := sub(out, "src-sub")
	oldest, _ := time.Parse(time.RFC3339Nano, st["oldestUnackedPublishTime"].(string))
	now, _ := time.Parse(time.RFC3339Nano, out["now"].(string))
	if st["backlog"] != float64(2) || st["backlogBytes"].(float64) <= 0 || oldest.IsZero() || oldest.After(now) || st["topic"] != topicName("src") {
		t.Fatalf("stats = %v", out)
	}

	// Nacking the messages 5 times dead-letters them.
	for range 5 {
		pulled := restCall(t, http.MethodPost, v1+"/subscriptions/src-sub:pull", map[string]any{"maxMessages": 10, "returnImmediately": true}, 200)
		msgs, _ := pulled["receivedMessages"].([]any)
		if len(msgs) != 2 {
			t.Fatalf("pulled %v", pulled)
		}
		if got := sub(stats(project), "src-sub")["outstanding"]; got != float64(2) {
			t.Errorf("outstanding = %v, want 2", got)
		}
		var ids []any
		for _, m := range msgs {
			ids = append(ids, m.(map[string]any)["ackId"])
		}
		restCall(t, http.MethodPost, v1+"/subscriptions/src-sub:modifyAckDeadline", map[string]any{"ackIds": ids, "ackDeadlineSeconds": 0}, 200)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		st = sub(stats(project), "src-sub")
		if st["deadLettered"] == float64(2) || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st["deadLettered"] != float64(2) || st["backlog"] != float64(0) || st["oldestUnackedPublishTime"] != nil {
		t.Fatalf("after dead-lettering: %v", st)
	}
	if dl := sub(stats(project), "dlq-sub"); dl["backlog"] != float64(2) {
		t.Errorf("dead-letter subscription = %v", dl)
	}

	// Seeking to now purges the backlog.
	restCall(t, http.MethodPost, v1+"/subscriptions/dlq-sub:seek", map[string]any{"time": stats(project)["now"]}, 200)
	if st := sub(stats(project), "dlq-sub"); st["backlog"] != float64(0) {
		t.Errorf("after purge: %v", st)
	}
}
