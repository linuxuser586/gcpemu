package ar_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
)

// #1039: the Registry cleanup pass runs each standard repository's
// cleanup policies (ADR 0070).

const day = 24 * time.Hour

// policyEnv is a Deterministic Instance with a standard repository
// "images" holding policies; passes run only on advance.
func policyEnv(t *testing.T, dryRun bool, policies ...*artifactregistrypb.CleanupPolicy) *env {
	t.Helper()
	return policyEnvLogged(t, nil, dryRun, policies...)
}

// policyEnvLogged is policyEnv with its cleanup log lines collected in log
// (nil: not collected).
func policyEnvLogged(t *testing.T, log *cleanupLog, dryRun bool, policies ...*artifactregistrypb.CleanupPolicy) *env {
	t.Helper()
	opts := []emutest.Option{emutest.WithDeterministic()}
	if log != nil {
		opts = append(opts, log.options()...)
	}
	e := start(t, opts...)
	m := map[string]*artifactregistrypb.CleanupPolicy{}
	for _, p := range policies {
		m[p.Id] = p
	}
	if _, err := e.createModeRepo("images", &artifactregistrypb.Repository{CleanupPolicies: m, CleanupPolicyDryRun: dryRun}); err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	return e
}

func (e *env) advance(d time.Duration) {
	e.t.Helper()
	if err := e.inst.Internal().AdvanceClock(e.ctx, d); err != nil {
		e.t.Fatal(err)
	}
}

// versions returns the version IDs (digests) of PROJECT/images/<img>.
func (e *env) versions(img string) []string {
	e.t.Helper()
	it := e.c.ListVersions(e.ctx, &artifactregistrypb.ListVersionsRequest{
		Parent: "projects/" + testProject + "/locations/" + testLoc + "/repositories/images/packages/" + strings.ReplaceAll(img, "/", "%2F"),
	})
	var out []string
	for {
		v, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			e.t.Fatalf("ListVersions %s: %v", img, err)
		}
		out = append(out, v.Name[strings.LastIndex(v.Name, "/")+1:])
	}
	sort.Strings(out)
	return out
}

// pushed pushes a new one-layer image as PROJECT/images/<img><suffix>
// (":tag", or "" for untagged) and returns its digest.
func (e *env) pushed(img, suffix string) v1.Hash {
	e.t.Helper()
	i := imageOf(e.t, img+suffix, randomLayer(e.t))
	d := digestOf(e.t, i)
	if suffix == "" {
		suffix = "@" + d.String()
	}
	e.push(img+suffix, i)
	return d
}

func wantVersions(ds ...v1.Hash) []string {
	out := []string{}
	for _, d := range ds {
		out = append(out, d.String())
	}
	sort.Strings(out)
	return out
}

func (e *env) checkVersions(img string, ds ...v1.Hash) {
	e.t.Helper()
	got, w := e.versions(img), wantVersions(ds...)
	if strings.Join(got, ",") != strings.Join(w, ",") {
		e.t.Fatalf("versions of %s = %v, want %v", img, got, w)
	}
}

func deletePolicy(id string, c *artifactregistrypb.CleanupPolicyCondition) *artifactregistrypb.CleanupPolicy {
	return &artifactregistrypb.CleanupPolicy{Id: id, Action: artifactregistrypb.CleanupPolicy_DELETE,
		ConditionType: &artifactregistrypb.CleanupPolicy_Condition{Condition: c}}
}

func keepPolicy(id string, c *artifactregistrypb.CleanupPolicyCondition) *artifactregistrypb.CleanupPolicy {
	p := deletePolicy(id, c)
	p.Action = artifactregistrypb.CleanupPolicy_KEEP
	return p
}

func tagState(s artifactregistrypb.CleanupPolicyCondition_TagState) *artifactregistrypb.CleanupPolicyCondition_TagState {
	return &s
}

// DELETE UNTAGGED older than 7 days: after 8 days the untagged version is
// gone with its blobs; tagged and newer versions remain.
func TestPolicyDeletesOldUntagged(t *testing.T) {
	var log cleanupLog
	e := policyEnvLogged(t, &log, false, deletePolicy("old-untagged", &artifactregistrypb.CleanupPolicyCondition{
		TagState: tagState(artifactregistrypb.CleanupPolicyCondition_UNTAGGED), OlderThan: durationpb.New(7 * day),
	}))
	tagged := e.pushed("app", ":v1")
	untagged := e.pushed("app", "")
	e.advance(6 * day)
	if recs := log.take(); len(recs) != 0 {
		t.Fatalf("a pass before the version was old enough logged %v", recs)
	}
	newer := e.pushed("app", "")

	e.advance(2 * day)

	e.checkVersions("app", tagged, newer)
	if e.hasBlob(untagged) {
		t.Fatal("the deleted version's manifest is still on disk")
	}
	checkLine(t, log.take(), untagged, "[]", "[old-untagged]", false)
}

// A version a policy deletes frees its own layer and config blobs; a layer
// it shares with a remaining version stays (#1039: "by policy").
func TestPolicyDeleteFreesLayers(t *testing.T) {
	e := policyEnv(t, false, deletePolicy("old-untagged", &artifactregistrypb.CleanupPolicyCondition{
		TagState: tagState(artifactregistrypb.CleanupPolicyCondition_UNTAGGED), OlderThan: durationpb.New(7 * day),
	}))
	shared, own := randomLayer(t), randomLayer(t)
	tagged := imageOf(t, "app:v1", shared)
	e.push("app:v1", tagged)
	untagged := imageOf(t, "app-untagged", shared, own)
	ud := digestOf(t, untagged)
	e.push("app@"+ud.String(), untagged)
	cfg, err := untagged.ConfigName()
	if err != nil {
		t.Fatal(err)
	}
	sd, od := digestOf(t, shared), digestOf(t, own)

	e.advance(8 * day)

	e.checkVersions("app", digestOf(t, tagged))
	for what, d := range map[string]v1.Hash{"manifest": ud, "own layer": od, "config": cfg} {
		if e.hasBlob(d) {
			t.Errorf("the deleted version's %s %s is still on disk", what, d)
		}
	}
	if !e.hasBlob(sd) {
		t.Error("the layer shared with the remaining version was freed")
	}
}

// cleanupLog collects the Instance's cleanup policy log lines, written
// as JSON (emutest.WithLogOutput).
type cleanupLog struct {
	mu   sync.Mutex
	recs []map[string]any
}

func (l *cleanupLog) Write(p []byte) (int, error) {
	var m map[string]any
	if json.Unmarshal(p, &m) == nil && m["msg"] == "ar: cleanup policy matched a version" {
		l.mu.Lock()
		l.recs = append(l.recs, m)
		l.mu.Unlock()
	}
	return len(p), nil
}

func (l *cleanupLog) take() []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.recs
	l.recs = nil
	return out
}

// options returns the emutest options that feed l.
func (l *cleanupLog) options() []emutest.Option {
	return []emutest.Option{emutest.WithLogOutput(l), emutest.WithConfig(func(c *config.Config) { c.LogFormat = "json" })}
}

// checkLine fails unless recs is one Info line reporting version d of
// "app" in "images", with tags, the policies and dryRun.
func checkLine(t *testing.T, recs []map[string]any, d v1.Hash, tags, policies string, dryRun bool) {
	t.Helper()
	if len(recs) != 1 {
		t.Fatalf("%d log lines, want 1: %v", len(recs), recs)
	}
	r := recs[0]
	if r["level"] != "INFO" || r["repository"] != repoName(testLoc, "images") ||
		r["version"] != repoName(testLoc, "images")+"/packages/app/versions/"+d.String() ||
		fmt.Sprint(r["tags"]) != tags || fmt.Sprint(r["policies"]) != policies || r["dryRun"] != dryRun {
		t.Fatalf("log line = %v", r)
	}
}

// With cleanupPolicyDryRun nothing is deleted; each matching version is
// logged once, on the first pass that finds it.
func TestPolicyDryRun(t *testing.T) {
	var log cleanupLog
	e := policyEnvLogged(t, &log, true, deletePolicy("old-untagged", &artifactregistrypb.CleanupPolicyCondition{
		TagState: tagState(artifactregistrypb.CleanupPolicyCondition_UNTAGGED), OlderThan: durationpb.New(7 * day),
	}))
	tagged := e.pushed("app", ":v1")
	untagged := e.pushed("app", "")

	e.advance(8 * day)

	e.checkVersions("app", tagged, untagged)
	checkLine(t, log.take(), untagged, "[]", "[old-untagged]", true)

	e.advance(time.Hour)

	if recs := log.take(); len(recs) != 0 {
		t.Fatalf("a second pass logged %v", recs)
	}
	e.checkVersions("app", tagged, untagged)
}

// A KEEP mostRecentVersions{keepCount: 3} alongside a DELETE ANY leaves the
// 3 newest versions of each package; with packageNamePrefixes it protects
// only the packages that match.
func TestPolicyKeepMostRecent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		prefixes []string
		keepB    bool
	}{{"every package", nil, true}, {"matching packages", []string{"a"}, false}} {
		t.Run(tc.name, func(t *testing.T) {
			e := policyEnv(t, false,
				deletePolicy("all", &artifactregistrypb.CleanupPolicyCondition{TagState: tagState(artifactregistrypb.CleanupPolicyCondition_ANY)}),
				&artifactregistrypb.CleanupPolicy{Id: "recent", Action: artifactregistrypb.CleanupPolicy_KEEP,
					ConditionType: &artifactregistrypb.CleanupPolicy_MostRecentVersions{MostRecentVersions: &artifactregistrypb.CleanupPolicyMostRecentVersions{
						PackageNamePrefixes: tc.prefixes, KeepCount: proto.Int32(3),
					}}})
			var as, bs []v1.Hash
			for i := range 5 {
				as = append(as, e.pushed("app", fmt.Sprintf(":a%d", i)))
				bs = append(bs, e.pushed("bpp", fmt.Sprintf(":b%d", i)))
				e.advance(time.Minute)
			}
			e.advance(time.Minute)
			e.checkVersions("app", as[2:]...)
			if tc.keepB {
				e.checkVersions("bpp", bs[2:]...)
			} else {
				e.checkVersions("bpp")
			}
		})
	}
}

// A KEEP tagPrefixes [release] wins over a DELETE ANY: versions tagged
// release-1 stay; versions tagged only local-abc go, with their tags.
func TestPolicyKeepTagPrefixWinsOverDelete(t *testing.T) {
	e := policyEnv(t, false,
		deletePolicy("all", &artifactregistrypb.CleanupPolicyCondition{TagState: tagState(artifactregistrypb.CleanupPolicyCondition_ANY)}),
		keepPolicy("releases", &artifactregistrypb.CleanupPolicyCondition{
			TagState: tagState(artifactregistrypb.CleanupPolicyCondition_TAGGED), TagPrefixes: []string{"release"},
		}))
	release := e.pushed("app", ":release-1")
	e.pushed("app", ":local-abc")

	e.advance(time.Minute)

	e.checkVersions("app", release)
	if _, err := remote.Head(e.ref(testProject + "/images/app:local-abc")); err == nil {
		t.Fatal("the deleted version's tag still resolves")
	}
}

// tagPrefixes matches a version when any one of its tags has a prefix.
func TestPolicyTagPrefixMatchesAnyTag(t *testing.T) {
	e := policyEnv(t, false, deletePolicy("tmp", &artifactregistrypb.CleanupPolicyCondition{
		TagState: tagState(artifactregistrypb.CleanupPolicyCondition_TAGGED), TagPrefixes: []string{"tmp-"},
	}))
	e.pushed("app", ":v1")
	if err := remote.Tag(e.ref(testProject+"/images/app:tmp-1").(name.Tag), mustGet(t, e, "app:v1")); err != nil {
		t.Fatal(err)
	}
	other := e.pushed("app", ":v2")

	e.advance(time.Minute)

	e.checkVersions("app", other)
}

func mustGet(t *testing.T, e *env, ref string) remote.Taggable {
	t.Helper()
	d, err := remote.Get(e.ref(testProject + "/images/" + ref))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// versionNamePrefixes filters by version ID (the digest), and
// packageNamePrefixes by package ID (the image path, nested ones
// included); the fields of one condition are ANDed.
func TestPolicyNamePrefixesAreAnded(t *testing.T) {
	e := start(t, emutest.WithDeterministic())
	if _, err := e.createModeRepo("images", &artifactregistrypb.Repository{}); err != nil {
		t.Fatal(err)
	}
	team1, team2 := e.pushed("team/app", ""), e.pushed("team/app", "")
	other := e.pushed("other/app", "")
	victim := team1
	if team2.String() < team1.String() {
		victim = team2
	}
	keep := team1
	if victim == team1 {
		keep = team2
	}
	// The shortest prefix of victim's digest that keep's does not share.
	n := len("sha256:")
	for victim.String()[:n] == keep.String()[:n] {
		n++
	}
	n++
	e.setPolicies(deletePolicy("one", &artifactregistrypb.CleanupPolicyCondition{
		PackageNamePrefixes: []string{"team/"}, VersionNamePrefixes: []string{victim.String()[:n]},
	}))

	e.advance(time.Minute)

	e.checkVersions("team/app", keep)
	e.checkVersions("other/app", other)
}

// setPolicies replaces the cleanup policies of "images" through
// UpdateRepository.
func (e *env) setPolicies(ps ...*artifactregistrypb.CleanupPolicy) {
	e.t.Helper()
	m := map[string]*artifactregistrypb.CleanupPolicy{}
	for _, p := range ps {
		m[p.Id] = p
	}
	_, err := e.c.UpdateRepository(e.ctx, &artifactregistrypb.UpdateRepositoryRequest{
		Repository: &artifactregistrypb.Repository{Name: repoName(testLoc, "images"), CleanupPolicies: m},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"cleanup_policies"}},
	})
	if err != nil {
		e.t.Fatalf("UpdateRepository: %v", err)
	}
}

// newerThan matches only versions younger than the duration.
func TestPolicyNewerThan(t *testing.T) {
	e := policyEnv(t, false) // no policies yet
	old := e.pushed("app", "")
	e.advance(3 * day)
	e.pushed("app", "")
	e.setPolicies(deletePolicy("young", &artifactregistrypb.CleanupPolicyCondition{NewerThan: durationpb.New(day)}))

	e.advance(time.Hour)

	e.checkVersions("app", old)
}

// A tagged index's untagged children survive DELETE UNTAGGED; once the
// index is deleted, the next pass deletes them.
func TestPolicyKeepsChildrenOfARemainingIndex(t *testing.T) {
	e := policyEnv(t, false, deletePolicy("untagged", &artifactregistrypb.CleanupPolicyCondition{
		TagState: tagState(artifactregistrypb.CleanupPolicyCondition_UNTAGGED),
	}))
	idx, err := random.Index(256, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(e.ref(testProject+"/images/multi:v1"), idx); err != nil {
		t.Fatal(err)
	}
	im, err := idx.IndexManifest()
	if err != nil {
		t.Fatal(err)
	}
	e.advance(time.Minute)
	e.checkVersions("multi", digestOf(t, idx), im.Manifests[0].Digest, im.Manifests[1].Digest)

	e.deleteVersion("multi", digestOf(t, idx))
	e.advance(time.Minute)

	e.checkVersions("multi")
}

// An untagged referrer of a surviving tagged image survives DELETE
// UNTAGGED; once its subject is deleted, a later pass deletes it.
func TestPolicyKeepsReferrerOfARemainingSubject(t *testing.T) {
	e := policyEnv(t, false, deletePolicy("untagged", &artifactregistrypb.CleanupPolicyCondition{
		TagState: tagState(artifactregistrypb.CleanupPolicyCondition_UNTAGGED),
	}))
	subject := imageOf(t, "subject", randomLayer(t))
	e.push("app:v1", subject)
	desc, err := partial.Descriptor(subject)
	if err != nil {
		t.Fatal(err)
	}
	sig := mutate.Subject(imageOf(t, "signature", randomLayer(t)), *desc).(v1.Image)
	sd := digestOf(t, sig)
	e.push("app@"+sd.String(), sig)

	e.advance(time.Minute)
	e.checkVersions("app", digestOf(t, subject), sd)

	e.deleteVersion("app", digestOf(t, subject))
	e.advance(time.Minute)

	e.checkVersions("app")
}

// With immutableTags, a DELETE ANY policy deletes no tagged version.
func TestPolicyImmutableTagsKeepTagged(t *testing.T) {
	e := start(t, emutest.WithDeterministic())
	if _, err := e.createModeRepo("images", &artifactregistrypb.Repository{
		FormatConfig: &artifactregistrypb.Repository_DockerConfig{DockerConfig: &artifactregistrypb.Repository_DockerRepositoryConfig{ImmutableTags: true}},
		CleanupPolicies: map[string]*artifactregistrypb.CleanupPolicy{"all": deletePolicy("all", &artifactregistrypb.CleanupPolicyCondition{
			TagState: tagState(artifactregistrypb.CleanupPolicyCondition_ANY),
		})},
	}); err != nil {
		t.Fatal(err)
	}
	tagged := e.pushed("app", ":v1")
	e.pushed("app", "")

	e.advance(time.Minute)

	e.checkVersions("app", tagged)
}

// Policies on remote and virtual repositories are stored and never delete
// anything.
func TestPolicyStoredButIdleOnRemoteAndVirtual(t *testing.T) {
	e := start(t, emutest.WithDeterministic())
	up := newFakeUpstream(t)
	up.put("mirror/app:v1", 256, 1)
	all := map[string]*artifactregistrypb.CleanupPolicy{"all": deletePolicy("all", &artifactregistrypb.CleanupPolicyCondition{
		TagState: tagState(artifactregistrypb.CleanupPolicyCondition_ANY),
	})}
	rr := customRemote(up.pub.URL + "/mirror")
	rr.CleanupPolicies = all
	if _, err := e.createModeRepo("upstream", rr); err != nil {
		t.Fatal(err)
	}
	vr := virtualRepo(&artifactregistrypb.UpstreamPolicy{Id: "u", Repository: repoName(testLoc, "upstream"), Priority: 1})
	vr.CleanupPolicies = all
	if _, err := e.createModeRepo("virtual", vr); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Image(e.ref(testProject + "/upstream/app:v1")); err != nil {
		t.Fatalf("pull through the remote: %v", err)
	}
	if _, err := remote.Image(e.ref(testProject + "/virtual/app:v1")); err != nil {
		t.Fatalf("pull through the virtual: %v", err)
	}

	e.advance(30 * day)

	for _, id := range []string{"upstream", "virtual"} {
		got, err := e.c.GetRepository(e.ctx, &artifactregistrypb.GetRepositoryRequest{Name: repoName(testLoc, id)})
		if err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(got.GetCleanupPolicies()["all"], all["all"]) {
			t.Fatalf("%s policies = %v, want %v", id, got.GetCleanupPolicies(), all)
		}
	}
	vs, err := e.c.ListVersions(e.ctx, &artifactregistrypb.ListVersionsRequest{Parent: repoName(testLoc, "upstream") + "/packages/app"}).Next()
	if err != nil || vs == nil {
		t.Fatalf("the remote repository's cached version is gone: %v", err)
	}
}

// Invalid cleanup policies are refused with INVALID_ARGUMENT, on
// CreateRepository and on UpdateRepository.
func TestPolicyValidation(t *testing.T) {
	cond := func() *artifactregistrypb.CleanupPolicyCondition { return &artifactregistrypb.CleanupPolicyCondition{} }
	mrv := func(keep int32) *artifactregistrypb.CleanupPolicy_MostRecentVersions {
		return &artifactregistrypb.CleanupPolicy_MostRecentVersions{MostRecentVersions: &artifactregistrypb.CleanupPolicyMostRecentVersions{KeepCount: proto.Int32(keep)}}
	}
	eleven := map[string]*artifactregistrypb.CleanupPolicy{}
	for i := range 11 {
		id := fmt.Sprintf("p%d", i)
		eleven[id] = deletePolicy(id, cond())
	}
	long := strings.Repeat("x", 128)
	for _, tc := range []struct {
		name     string
		policies map[string]*artifactregistrypb.CleanupPolicy
	}{
		{"more than 10", eleven},
		{"ID of 128 characters", map[string]*artifactregistrypb.CleanupPolicy{long: deletePolicy(long, cond())}},
		{"key disagrees with id", map[string]*artifactregistrypb.CleanupPolicy{"a": deletePolicy("b", cond())}},
		{"neither condition nor mostRecentVersions", map[string]*artifactregistrypb.CleanupPolicy{"a": {Id: "a", Action: artifactregistrypb.CleanupPolicy_DELETE}}},
		{"mostRecentVersions with DELETE", map[string]*artifactregistrypb.CleanupPolicy{"a": {Id: "a", Action: artifactregistrypb.CleanupPolicy_DELETE, ConditionType: mrv(1)}}},
		{"empty ID", map[string]*artifactregistrypb.CleanupPolicy{"": deletePolicy("", cond())}},
		{"unknown action", map[string]*artifactregistrypb.CleanupPolicy{"a": {Id: "a", Action: 3, ConditionType: &artifactregistrypb.CleanupPolicy_Condition{Condition: cond()}}}},
		{"action unset", map[string]*artifactregistrypb.CleanupPolicy{"a": {Id: "a", ConditionType: &artifactregistrypb.CleanupPolicy_Condition{Condition: cond()}}}},
		{"tagPrefixes without TAGGED", map[string]*artifactregistrypb.CleanupPolicy{"a": deletePolicy("a", &artifactregistrypb.CleanupPolicyCondition{TagPrefixes: []string{"x"}})}},
		{"negative olderThan", map[string]*artifactregistrypb.CleanupPolicy{"a": deletePolicy("a", &artifactregistrypb.CleanupPolicyCondition{OlderThan: durationpb.New(-time.Hour)})}},
		{"negative newerThan", map[string]*artifactregistrypb.CleanupPolicy{"a": deletePolicy("a", &artifactregistrypb.CleanupPolicyCondition{NewerThan: durationpb.New(-time.Hour)})}},
		{"negative keepCount", map[string]*artifactregistrypb.CleanupPolicy{"a": {Id: "a", Action: artifactregistrypb.CleanupPolicy_KEEP, ConditionType: mrv(-1)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := start(t)
			if _, err := e.createModeRepo("bad", &artifactregistrypb.Repository{CleanupPolicies: tc.policies}); code(err) != codes.InvalidArgument {
				t.Errorf("CreateRepository = %v, want INVALID_ARGUMENT", err)
			}
			e.createRepo(testLoc, "images")
			_, err := e.c.UpdateRepository(e.ctx, &artifactregistrypb.UpdateRepositoryRequest{
				Repository: &artifactregistrypb.Repository{Name: repoName(testLoc, "images"), CleanupPolicies: tc.policies},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"cleanup_policies"}},
			})
			if code(err) != codes.InvalidArgument {
				t.Errorf("UpdateRepository = %v, want INVALID_ARGUMENT", err)
			}
		})
	}
}

// Valid policies round-trip through GetRepository unchanged, over gRPC and
// REST.
func TestPolicyRoundTrip(t *testing.T) {
	e := start(t)
	in := map[string]*artifactregistrypb.CleanupPolicy{
		"old": deletePolicy("old", &artifactregistrypb.CleanupPolicyCondition{
			TagState: tagState(artifactregistrypb.CleanupPolicyCondition_TAGGED), TagPrefixes: []string{"dev-"},
			VersionNamePrefixes: []string{"sha256:"}, PackageNamePrefixes: []string{"team/"},
			OlderThan: durationpb.New(7 * day), NewerThan: durationpb.New(30 * day),
		}),
		"recent": {Id: "recent", Action: artifactregistrypb.CleanupPolicy_KEEP, ConditionType: &artifactregistrypb.CleanupPolicy_MostRecentVersions{
			MostRecentVersions: &artifactregistrypb.CleanupPolicyMostRecentVersions{PackageNamePrefixes: []string{"app"}, KeepCount: proto.Int32(3)},
		}},
	}
	if _, err := e.createModeRepo("images", &artifactregistrypb.Repository{CleanupPolicies: in, CleanupPolicyDryRun: true}); err != nil {
		t.Fatal(err)
	}
	got, err := e.c.GetRepository(e.ctx, &artifactregistrypb.GetRepositoryRequest{Name: repoName(testLoc, "images")})
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(&artifactregistrypb.Repository{CleanupPolicies: got.CleanupPolicies}, &artifactregistrypb.Repository{CleanupPolicies: in}) || !got.CleanupPolicyDryRun {
		t.Fatalf("gRPC policies = %v, want %v", got.CleanupPolicies, in)
	}

	st, body := rest(t, e, "PATCH", "/v1/"+repoName(testLoc, "images")+"?updateMask=cleanupPolicies",
		`{"cleanupPolicies":{"keep":{"id":"keep","action":"KEEP","condition":{"tagState":"TAGGED","tagPrefixes":["release"]}}}}`)
	if st != http.StatusOK {
		t.Fatalf("REST PATCH = %d %v", st, body)
	}
	st, body = rest(t, e, "GET", "/v1/"+repoName(testLoc, "images"), "")
	b, _ := json.Marshal(body["cleanupPolicies"])
	if st != http.StatusOK || string(b) != `{"keep":{"action":"KEEP","condition":{"tagPrefixes":["release"],"tagState":"TAGGED"},"id":"keep"}}` {
		t.Fatalf("REST GET = %d %s", st, b)
	}
}
