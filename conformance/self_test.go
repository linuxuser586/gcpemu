//go:build conformance

package conformance

import (
	"bytes"
	"context"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/escapeguard"
)

// scenarios are the suite's tests by name, for the tests below.
var scenarios = map[string]scenario{
	"TestStorageBuckets":        storageBuckets,
	"TestStorageObjects":        storageObjects,
	"TestSecretManagerSecrets":  secretManagerSecrets,
	"TestSecretManagerVersions": secretManagerVersions,
	"TestDNSManagedZones":       dnsManagedZones,
	"TestDNSRecordSetChanges":   dnsRecordSetChanges,
	"TestArtifactRegistryRepos": artifactRegistryRepos,
	"TestIAMServiceAccounts":    iamServiceAccounts,
	"TestIAMResourcePolicies":   iamResourcePolicies,
	"TestPubSubTopics":          pubsubTopics,
	"TestPubSubSubscriptions":   pubsubSubscriptions,
	"TestPubSubPublishPull":     pubsubPublishPull,
}

// selfModes record against the emulator into dir, standing in for GCP,
// and compare with what they recorded.
func selfModes(dir string) (record, compare mode) {
	record = mode{record: true, dir: dir, target: emulatorTarget, report: reportMismatches}
	compare = record
	compare.record = false
	return record, compare
}

// TestSelfRecording runs every scenario in record mode against the
// emulator, twice, and then in compare mode: recording must be
// byte-identical when nothing changed, and comparing must pass.
func TestSelfRecording(t *testing.T) {
	dir := t.TempDir()
	rec, cmp := selfModes(dir)
	for _, name := range slices.Sorted(maps.Keys(scenarios)) {
		s := scenarios[name]
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(dir, s.product, name+".json")
			t.Run("record", func(t *testing.T) { s.run(t, rec, name) })
			first, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			t.Run("rerecord", func(t *testing.T) { s.run(t, rec, name) })
			if second, err := os.ReadFile(file); err != nil || !bytes.Equal(first, second) {
				t.Errorf("recording again changed %s (%v):\n%s\n----\n%s", file, err, first, second)
			}
			t.Run("compare", func(t *testing.T) { s.run(t, cmp, name) })
		})
	}
}

// TestChangedRecordingNamesTheField: a Recording with one field changed
// fails compare mode with that field's path.
func TestChangedRecordingNamesTheField(t *testing.T) {
	dir := t.TempDir()
	rec, cmp := selfModes(dir)
	const name = "TestPubSubTopics"
	t.Run("record", func(t *testing.T) { pubsubTopics.run(t, rec, name) })
	file := filepath.Join(dir, "pubsub", name+".json")
	r, err := readRecording(file)
	if err != nil {
		t.Fatal(err)
	}
	r.Exchanges[1].Response.Body.(map[string]any)["labels"].(map[string]any)[labelKey] = "changed"
	if err := writeRecording(file, r); err != nil {
		t.Fatal(err)
	}
	var got []Mismatch
	cmp.report = func(_ testing.TB, _ string, ms []Mismatch) { got = ms }
	t.Run("compare", func(t *testing.T) { pubsubTopics.run(t, cmp, name) })
	assertMismatches(t, got, "exchanges[1].response.body.labels."+labelKey)
}

// TestMissingRecordingSkips: compare mode skips a test with no Recording.
func TestMissingRecordingSkips(t *testing.T) {
	_, cmp := selfModes(t.TempDir())
	var skippedTest bool
	t.Run("compare", func(t *testing.T) {
		defer func() { skippedTest = t.Skipped() }()
		pubsubTopics.run(t, cmp, "TestPubSubTopics")
	})
	if !skippedTest || !strings.Contains(recordHint, "task conformance:record") {
		t.Errorf("skipped = %v, hint %q", skippedTest, recordHint)
	}
}

// TestNothingLeavesTheMachine: in compare mode the clients' transport and
// gRPC dialer send any non-loopback request to the Escape guard, which
// refuses it and names the host, while loopback is reached directly.
func TestNothingLeavesTheMachine(t *testing.T) {
	g := escapeguard.New(t)
	client := &http.Client{Transport: guardedTransport(g)}
	if resp, err := client.Get("https://storage.googleapis.com/storage/v1/b"); err == nil {
		resp.Body.Close()
		t.Error("request to storage.googleapis.com succeeded")
	}
	if _, err := guardedDialer(g)(context.Background(), "pubsub.googleapis.com:443"); err == nil {
		t.Error("gRPC dial of pubsub.googleapis.com succeeded")
	}
	if got := g.Take(); !slices.Equal(got, []string{"storage.googleapis.com", "pubsub.googleapis.com"}) {
		t.Errorf("guard saw %q", got)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	resp, err := client.Get("http://" + ln.Addr().String())
	if err != nil {
		t.Fatalf("loopback: %v", err)
	}
	resp.Body.Close()
	conn, err := guardedDialer(g)(context.Background(), ln.Addr().String())
	if err != nil {
		t.Fatalf("loopback dial: %v", err)
	}
	conn.Close()
	if got := g.Take(); len(got) != 0 {
		t.Errorf("loopback went through the guard: %q", got)
	}
}
