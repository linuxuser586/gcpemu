package emutest_test

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
)

func status(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// pulled returns the data of the messages a pull from sub returns.
func pulled(t *testing.T, gw, sub string) []string {
	t.Helper()
	var r struct {
		ReceivedMessages []struct {
			Message struct{ Data string }
		}
	}
	if err := json.Unmarshal(call(t, "POST", gw+"/pubsub/v1/projects/snap-proj/subscriptions/"+sub+":pull", `{"maxMessages":10}`), &r); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range r.ReceivedMessages {
		b, _ := base64.StdEncoding.DecodeString(m.Message.Data)
		out = append(out, string(b))
	}
	return out
}

// TestStateSnapshots is FR-CORE-012: a State snapshot keeps GCS objects
// and Pub/Sub messages, restoring it undoes every later change, and a
// later Instance of the same instance dir restores it too.
func TestStateSnapshots(t *testing.T) {
	dir := t.TempDir()
	sameDir := func(c *config.Config) { c.DataDir = dir }
	inst := emutest.Start(t, []string{"gcs", "pubsub"}, sameDir)
	gw := inst.GatewayURL()
	publish := func(data string) {
		call(t, "POST", gw+"/pubsub/v1/projects/snap-proj/topics/topic-a:publish",
			`{"messages":[{"data":"`+base64.StdEncoding.EncodeToString([]byte(data))+`"}]}`)
	}
	call(t, "POST", gw+"/storage/v1/b?project=snap-proj", `{"name":"snap-bucket"}`)
	call(t, "POST", gw+"/upload/storage/v1/b/snap-bucket/o?uploadType=media&name=kept", "kept data")
	call(t, "PUT", gw+"/pubsub/v1/projects/snap-proj/topics/topic-a", `{}`)
	call(t, "PUT", gw+"/pubsub/v1/projects/snap-proj/subscriptions/sub-a", `{"topic":"projects/snap-proj/topics/topic-a"}`)
	publish("before")

	var info struct {
		Name      string
		Services  []string
		SizeBytes int64
	}
	if err := json.Unmarshal(call(t, "POST", gw+"/_emu/v1/snapshots", `{"name":"fixture"}`), &info); err != nil {
		t.Fatal(err)
	}
	if info.Name != "fixture" || info.SizeBytes <= int64(len("kept data")) || !strings.Contains(strings.Join(info.Services, ","), "gcs") {
		t.Errorf("saved %+v", info)
	}

	call(t, "DELETE", gw+"/storage/v1/b/snap-bucket/o/kept", "")
	call(t, "POST", gw+"/upload/storage/v1/b/snap-bucket/o?uploadType=media&name=later", "later data")
	call(t, "PUT", gw+"/pubsub/v1/projects/snap-proj/topics/later", `{}`)
	publish("after")

	call(t, "POST", gw+"/_emu/v1/snapshots/fixture/restore", "")
	if got := string(call(t, "GET", gw+"/storage/v1/b/snap-bucket/o/kept?alt=media", "")); got != "kept data" {
		t.Errorf("restored object = %q", got)
	}
	if code, _ := status(t, "GET", gw+"/storage/v1/b/snap-bucket/o/later", ""); code != 404 {
		t.Errorf("object made after the snapshot: %d", code)
	}
	if code, _ := status(t, "GET", gw+"/pubsub/v1/projects/snap-proj/topics/later", ""); code != 404 {
		t.Errorf("topic made after the snapshot: %d", code)
	}
	if got := pulled(t, gw, "sub-a"); strings.Join(got, ",") != "before" {
		t.Errorf("pulled after restore = %q", got)
	}

	// Saving under a taken name replaces the snapshot.
	call(t, "POST", gw+"/_emu/v1/snapshots", `{"name":"fixture"}`)
	var list struct{ Snapshots []struct{ Name string } }
	if err := json.Unmarshal(call(t, "GET", gw+"/_emu/v1/snapshots", ""), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Snapshots) != 1 || list.Snapshots[0].Name != "fixture" {
		t.Errorf("snapshots = %+v", list.Snapshots)
	}

	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{"POST", "/_emu/v1/snapshots", `{"name":"../escape"}`, 400},
		{"POST", "/_emu/v1/snapshots", `{"name":""}`, 400},
		{"POST", "/_emu/v1/snapshots/missing/restore", "", 404},
		{"DELETE", "/_emu/v1/snapshots/missing", "", 404},
	} {
		if code, body := status(t, c.method, gw+c.path, c.body); code != c.want {
			t.Errorf("%s %s %s = %d %s, want %d", c.method, c.path, c.body, code, body, c.want)
		}
	}

	// A new Instance of the same instance dir, with none of the state.
	if err := inst.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	inst2 := emutest.Start(t, []string{"gcs", "pubsub"}, sameDir)
	gw2 := inst2.GatewayURL()
	if code, _ := status(t, "GET", gw2+"/storage/v1/b/snap-bucket", ""); code != 404 {
		t.Fatalf("Ephemeral Instance kept its bucket: %d", code)
	}
	call(t, "POST", gw2+"/_emu/v1/snapshots/fixture/restore", "")
	if got := string(call(t, "GET", gw2+"/storage/v1/b/snap-bucket/o/kept?alt=media", "")); got != "kept data" {
		t.Errorf("object restored into a new Instance = %q", got)
	}
	call(t, "DELETE", gw2+"/_emu/v1/snapshots/fixture", "")
	if err := json.Unmarshal(call(t, "GET", gw2+"/_emu/v1/snapshots", ""), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Snapshots) != 0 {
		t.Errorf("snapshots after delete = %+v", list.Snapshots)
	}
}
