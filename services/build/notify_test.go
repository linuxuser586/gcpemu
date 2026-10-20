package build_test

import (
	"encoding/base64"
	"slices"
	"testing"
	"time"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/protobuf/encoding/protojson"
)

// buildEvent is a message pulled from the cloud-builds topic.
type buildEvent struct {
	attrs map[string]string
	build *cloudbuildpb.Build
}

// cloudBuilds creates the cloud-builds topic and a subscription on it.
func (e *env) cloudBuilds() {
	e.t.Helper()
	topic := "projects/" + testProject + "/topics/cloud-builds"
	if c := e.rest("PUT", "/pubsub/v1/"+topic, map[string]any{}, nil); c != 200 {
		e.t.Fatalf("create topic: %d", c)
	}
	if c := e.rest("PUT", "/pubsub/v1/projects/"+testProject+"/subscriptions/builds", map[string]any{"topic": topic}, nil); c != 200 {
		e.t.Fatalf("create subscription: %d", c)
	}
}

// pull drains the cloud-builds subscription.
func (e *env) pull() []buildEvent {
	e.t.Helper()
	var out []buildEvent
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
		if c := e.rest("POST", "/pubsub/v1/projects/"+testProject+"/subscriptions/builds:pull", map[string]any{"maxMessages": 100, "returnImmediately": true}, &resp); c != 200 {
			e.t.Fatalf("pull: %d", c)
		}
		if len(resp.ReceivedMessages) == 0 {
			return out
		}
		var acks []string
		for _, m := range resp.ReceivedMessages {
			data, err := base64.StdEncoding.DecodeString(m.Message.Data)
			if err != nil {
				e.t.Fatal(err)
			}
			b := &cloudbuildpb.Build{}
			if err := protojson.Unmarshal(data, b); err != nil {
				e.t.Fatalf("data %q: %v", data, err)
			}
			out = append(out, buildEvent{attrs: m.Message.Attributes, build: b})
			acks = append(acks, m.AckID)
		}
		if c := e.rest("POST", "/pubsub/v1/projects/"+testProject+"/subscriptions/builds:acknowledge", map[string]any{"ackIds": acks}, nil); c != 200 {
			e.t.Fatalf("acknowledge: %d", c)
		}
	}
}

// awaitEvents pulls until a message with a status in last arrives, and
// returns the statuses published for Build id, in order.
func (e *env) awaitEvents(id string, last ...cloudbuildpb.Build_Status) []string {
	e.t.Helper()
	var got []string
	deadline := time.Now().Add(2 * time.Minute)
	for {
		for _, ev := range e.pull() {
			if ev.attrs["buildId"] != id {
				continue
			}
			if ev.build.GetId() != id || ev.build.GetStatus().String() != ev.attrs["status"] {
				e.t.Fatalf("attributes %v, Build %s %v", ev.attrs, ev.build.GetId(), ev.build.GetStatus())
			}
			got = append(got, ev.attrs["status"])
			for _, l := range last {
				if ev.attrs["status"] == l.String() {
					return got
				}
			}
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("Build %s published %v, never %v", id, got, last)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Every status change of a Build is published to its Project's
// cloud-builds topic, in order, with the Build as data.
func TestCloudBuildsNotifications(t *testing.T) {
	e := startWith(t, []string{"pubsub"})
	e.cloudBuilds()

	op, b := e.create(&cloudbuildpb.Build{Steps: []*cloudbuildpb.BuildStep{step("", "true")}})
	if _, err := op.Wait(e.ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got, want := e.awaitEvents(b.GetId(), cloudbuildpb.Build_SUCCESS), []string{"QUEUED", "WORKING", "SUCCESS"}; !slices.Equal(got, want) {
		t.Errorf("successful Build published %v, want %v", got, want)
	}

	_, b = e.create(&cloudbuildpb.Build{Steps: []*cloudbuildpb.BuildStep{step("", "exit 1")}})
	if got, want := e.awaitEvents(b.GetId(), cloudbuildpb.Build_FAILURE), []string{"QUEUED", "WORKING", "FAILURE"}; !slices.Equal(got, want) {
		t.Errorf("failed Build published %v, want %v", got, want)
	}

	_, b = e.create(&cloudbuildpb.Build{Steps: []*cloudbuildpb.BuildStep{step("", "sleep 600")}})
	e.awaitStep(b.GetId(), 0)
	if _, err := e.cb.CancelBuild(e.ctx, &cloudbuildpb.CancelBuildRequest{ProjectId: testProject, Id: b.GetId()}); err != nil {
		t.Fatalf("CancelBuild: %v", err)
	}
	if got, want := e.awaitEvents(b.GetId(), cloudbuildpb.Build_CANCELLED), []string{"QUEUED", "WORKING", "CANCELLED"}; !slices.Equal(got, want) {
		t.Errorf("cancelled Build published %v, want %v", got, want)
	}
}

// Without a cloud-builds topic, Builds run, nothing is published and the
// topic is not created.
func TestNoCloudBuildsTopic(t *testing.T) {
	e := startWith(t, []string{"pubsub"})
	op, _ := e.create(&cloudbuildpb.Build{Steps: []*cloudbuildpb.BuildStep{step("", "true")}})
	b, err := op.Wait(e.ctx)
	if err != nil || b.GetStatus() != cloudbuildpb.Build_SUCCESS {
		t.Fatalf("Build = %v, %v", b.GetStatus(), err)
	}
	if c := e.rest("GET", "/pubsub/v1/projects/"+testProject+"/topics/cloud-builds", nil, nil); c != 404 {
		t.Errorf("GET cloud-builds topic = %d, want 404", c)
	}
}
