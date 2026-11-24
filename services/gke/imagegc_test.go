package gke

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/store"
)

const gib = int64(1) << 30

// fakeImageStore is a node's image storage: snapshot is the snapshotter's
// usage, and each image's size its content.
type fakeImageStore struct {
	snapshot int64
	imgs     []criImage
	used     map[string]bool // image ID → used by a container
	removed  []string
	// failRemove makes every removal fail.
	failRemove bool
	// disk is the Runtime disk the node's volume is on; nil means one
	// with ample room.
	disk *fakeDisk
}

// fakeDisk is a Runtime disk that nodes share: removing an image frees its
// size plus extra, standing for its snapshots.
type fakeDisk struct {
	free, extra int64
}

func (f *fakeImageStore) freeBytes(context.Context) (int64, error) {
	if f.disk == nil {
		return 1 << 50, nil
	}
	return f.disk.free, nil
}

func (f *fakeImageStore) snapshotBytes(context.Context) (int64, error) { return f.snapshot, nil }
func (f *fakeImageStore) images(context.Context) ([]criImage, error) {
	return slices.Clone(f.imgs), nil
}
func (f *fakeImageStore) inUse(context.Context) (map[string]bool, error) { return f.used, nil }
func (f *fakeImageStore) remove(_ context.Context, id string) error {
	if f.failRemove {
		return errors.New("crictl rmi failed")
	}
	f.removed = append(f.removed, id)
	if f.disk != nil {
		for _, i := range f.imgs {
			if i.ID == id {
				f.disk.free += i.Size + f.disk.extra
			}
		}
	}
	f.imgs = slices.DeleteFunc(f.imgs, func(i criImage) bool { return i.ID == id })
	return nil
}

var gcT0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// A 10 GiB boot disk with default thresholds: GC starts at 85% (8.5 GiB)
// and frees down to 80% (8 GiB).
var tenGiB = imagePolicy{capacity: 10 * gib, high: 85, low: 80, minAge: 2 * time.Minute}

// Once a node's image storage passes the high threshold of its boot disk,
// unused images go, the one used longest ago first, until it is under the
// low threshold; an image in use stays.
func TestImageGCFreesOldestUsedFirstToLowThreshold(t *testing.T) {
	st := &fakeImageStore{
		snapshot: 4 * gib,
		imgs: []criImage{
			{ID: "a", Size: gib}, {ID: "b", Size: gib}, {ID: "c", Size: gib}, {ID: "run", Size: gib / 2},
		},
		used: map[string]bool{"a": true, "b": true, "c": true, "run": true},
	}
	g := &imageGC{}
	ctx := context.Background()
	// b stops being used first, then a, then c.
	steps := []struct {
		at   time.Duration
		used []string
	}{{0, []string{"a", "b", "c", "run"}}, {time.Minute, []string{"a", "c", "run"}}, {2 * time.Minute, []string{"c", "run"}}, {3 * time.Minute, []string{"run"}}}
	for _, s := range steps {
		st.used = map[string]bool{}
		for _, u := range s.used {
			st.used[u] = true
		}
		if _, err := g.pass(ctx, st, tenGiB, gcT0.Add(s.at)); err != nil {
			t.Fatal(err)
		}
	}
	if len(st.removed) != 0 {
		t.Fatalf("removed %v under the high threshold (7.5 of 10 GiB used), want none", st.removed)
	}
	// 9 GiB of 10: 1 GiB over the low threshold.
	st.snapshot = 5*gib + gib/2
	if _, err := g.pass(ctx, st, tenGiB, gcT0.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if want := []string{"b"}; !slices.Equal(st.removed, want) {
		t.Errorf("removed %v, want %v", st.removed, want)
	}
	// 9.5 GiB: 1.5 GiB over. An image is credited its share of all image
	// storage, its content scaled by storage over content (9.5/2.5), so
	// a alone frees 3.8 GiB, and c, used later, stays.
	st.removed = nil
	st.snapshot = 7 * gib
	st.imgs = append(st.imgs, criImage{ID: "b2", Size: 0})
	if _, err := g.pass(ctx, st, tenGiB, gcT0.Add(20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if want := []string{"a"}; !slices.Equal(st.removed, want) {
		t.Errorf("removed %v, want %v", st.removed, want)
	}
}

// A pinned image, and a system image the node imported at start, which a
// private node could not pull again, are never removed.
func TestImageGCKeepsPinnedAndPreloadedImages(t *testing.T) {
	st := &fakeImageStore{
		snapshot: 9 * gib,
		imgs: []criImage{
			{ID: "pinned", Size: gib, Pinned: true},
			{ID: "dns", Tags: []string{"docker.io/rancher/mirrored-coredns-coredns:1.14.7"}, Size: gib},
			{ID: "app", Tags: []string{"us-docker.pkg.dev/p/r/app:local-1"}, Size: gib},
		},
	}
	g := &imageGC{}
	ctx := context.Background()
	if _, err := g.pass(ctx, st, tenGiB, gcT0); err != nil {
		t.Fatal(err)
	}
	if _, err := g.pass(ctx, st, tenGiB, gcT0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if want := []string{"app"}; !slices.Equal(st.removed, want) {
		t.Errorf("removed %v, want %v", st.removed, want)
	}
}

// crictlOutputs are a k3s node's crictl outputs, as of k3s v1.37.1.
var crictlOutputs = map[string]string{
	"crictl imagefsinfo -o json": `{"status":{"containerFilesystems":[],"imageFilesystems":[{"fsId":{"mountpoint":"/var/lib/rancher/k3s/agent/containerd/io.containerd.snapshotter.v1.overlayfs"},"inodesUsed":{"value":"3641"},"timestamp":"1794503298751040500","usedBytes":{"value":"216498176"}}]}}`,
	"crictl images -o json": `{"images":[
	  {"id":"sha256:8e0d","repoTags":["docker.io/rancher/klipper-helm:v0.13.3"],"repoDigests":["docker.io/rancher/klipper-helm@sha256:5425"],"size":"74172006","uid":{"value":"1000"},"username":"","pinned":false},
	  {"id":"sha256:4a83","repoTags":["docker.io/rancher/mirrored-pause:3.10.2"],"repoDigests":[],"size":"320805","pinned":true},
	  {"id":"sha256:d7b0","repoTags":[],"repoDigests":["us-docker.pkg.dev/p/r/app@sha256:aaaa"],"size":"1000","pinned":false}]}`,
	"crictl ps -a -o json": `{"containers":[
	  {"id":"6469","image":{"image":"sha256:8e0d"},"imageRef":"sha256:8e0d","state":"CONTAINER_EXITED"},
	  {"id":"2b9d","image":{"image":"us-docker.pkg.dev/p/r/app@sha256:aaaa"},"imageRef":"us-docker.pkg.dev/p/r/app@sha256:aaaa","state":"CONTAINER_RUNNING"}]}`,
}

// The node's image storage is read from crictl's JSON, an image a
// container names by digest counts as in use, and images are removed by ID.
func TestCrictlImageStore(t *testing.T) {
	var ran [][]string
	st := crictlStore{exec: func(_ context.Context, cmd []string) ([]byte, error) {
		ran = append(ran, cmd)
		return []byte(crictlOutputs[strings.Join(cmd, " ")]), nil
	}}
	ctx := context.Background()
	snap, err := st.snapshotBytes(ctx)
	if err != nil || snap != 216498176 {
		t.Errorf("snapshotBytes = %d, %v; want 216498176", snap, err)
	}
	imgs, err := st.images(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []criImage{
		{ID: "sha256:8e0d", Tags: []string{"docker.io/rancher/klipper-helm:v0.13.3"}, Digests: []string{"docker.io/rancher/klipper-helm@sha256:5425"}, Size: 74172006},
		{ID: "sha256:4a83", Tags: []string{"docker.io/rancher/mirrored-pause:3.10.2"}, Digests: []string{}, Size: 320805, Pinned: true},
		{ID: "sha256:d7b0", Tags: []string{}, Digests: []string{"us-docker.pkg.dev/p/r/app@sha256:aaaa"}, Size: 1000},
	}
	if !reflect.DeepEqual(imgs, want) {
		t.Errorf("images = %+v\nwant %+v", imgs, want)
	}
	used, err := st.inUse(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, img := range imgs {
		if got := imageUsed(img, used); got != (img.ID != "sha256:4a83") {
			t.Errorf("image %s in use = %v", img.ID, got)
		}
	}
	ran = nil
	if err := st.remove(ctx, "sha256:8e0d"); err != nil {
		t.Fatal(err)
	}
	if want := [][]string{{"crictl", "rmi", "sha256:8e0d"}}; !reflect.DeepEqual(ran, want) {
		t.Errorf("remove ran %v, want %v", ran, want)
	}
}

// In a Deterministic Instance, advancing the Emulator clock runs image GC
// on every node of a running cluster, against its pool's boot disk size
// and thresholds; a pool with neither set uses 100 GB and 85/80%.
func TestImageGCOnClockAdvance(t *testing.T) {
	s := newDeterministicService(t)
	rec := &clusterRecord{Int: clusterInternal{Project: "gc-project", Location: "us-central1-a", Name: "c",
		Nodes: []nodeRecord{{Name: "small-1", Pool: "small"}, {Name: "big-1", Pool: "big"}}}}
	rec.setCluster(&containerpb.Cluster{Name: "c", Id: "id1", Status: containerpb.Cluster_RUNNING,
		NodePools: []*containerpb.NodePool{
			{Name: "small", Config: &containerpb.NodeConfig{DiskSizeGb: 20,
				KubeletConfig: &containerpb.NodeKubeletConfig{ImageGcHighThresholdPercent: 50, ImageGcLowThresholdPercent: 40}}},
			{Name: "big", Config: &containerpb.NodeConfig{}},
		}})
	if err := s.env.Store.Update(func(tx store.Tx) error { return putCluster(tx, rec) }); err != nil {
		t.Fatal(err)
	}
	// Each node holds 11 GiB: two unused 1 GiB images over 9 GiB of
	// snapshots.
	stores := map[string]*fakeImageStore{}
	for _, n := range []string{"small-1", "big-1"} {
		stores[n] = &fakeImageStore{snapshot: 9 * gib, imgs: []criImage{{ID: "old", Size: gib}, {ID: "new", Size: gib}}}
	}
	s.testImageStore = func(key, node string) nodeImageStore {
		if key != rec.key() {
			t.Errorf("image store for cluster %s", key)
		}
		return stores[node]
	}
	ctx := context.Background()
	if err := s.ClockAdvanced(ctx); err != nil {
		t.Fatal(err)
	}
	if len(stores["small-1"].removed) != 0 {
		t.Fatalf("removed %v from images first seen just now, want none", stores["small-1"].removed)
	}
	s.env.Clock.(*clock.Offset).Advance(5 * time.Minute)
	if err := s.ClockAdvanced(ctx); err != nil {
		t.Fatal(err)
	}
	// 11 GiB passes 50% of 20 GiB. 40% is 8 GiB, so 3 GiB must go; each
	// image is credited 5.5 GiB (11/2 of its 1 GiB), so one image goes,
	// the first by ID of the two seen at the same time.
	if got, want := stores["small-1"].removed, []string{"new"}; !slices.Equal(got, want) {
		t.Errorf("small pool's node removed %v, want %v", got, want)
	}
	if got := stores["big-1"].removed; len(got) != 0 {
		t.Errorf("big pool's node (11 of 100 GiB) removed %v, want none", got)
	}
}

// The Runtime disk's free space is the available column of df over the
// node's k3s data directory, whose volume is on the Runtime disk (#1227);
// output that has no such column is an error, not zero.
func TestCrictlStoreFreeBytes(t *testing.T) {
	// Captured from a k3s node image over a named volume.
	const dfOut = "Filesystem                            1-blocks         Used    Available Capacity Mounted on\n" +
		"/dev/mapper/ubuntu--vg-ubuntu--lv 980105256960 789929299968 140313862144      85% /var/lib/rancher/k3s\n"
	var ran [][]string
	out := dfOut
	st := crictlStore{exec: func(_ context.Context, cmd []string) ([]byte, error) {
		ran = append(ran, cmd)
		return []byte(out), nil
	}}
	free, err := st.freeBytes(context.Background())
	if err != nil || free != 140313862144 {
		t.Errorf("freeBytes = %d, %v; want 140313862144", free, err)
	}
	if want := [][]string{{"df", "-B1", "-P", "/var/lib/rancher/k3s"}}; !reflect.DeepEqual(ran, want) {
		t.Errorf("ran %v, want %v", ran, want)
	}
	for _, bad := range []string{"", "Filesystem 1-blocks Used Available Capacity Mounted on\n", "Filesystem 1-blocks Used Available Capacity Mounted on\n/dev/x 10 5 lots 50% /var/lib/rancher/k3s\n"} {
		out = bad
		if free, err := st.freeBytes(context.Background()); err == nil {
			t.Errorf("df output %q: freeBytes = %d, want an error", bad, free)
		}
	}
}

// neverFull is a boot disk GC never fires on: a pass only keeps records.
var neverFull = imagePolicy{capacity: 1 << 60, high: 85, low: 80, minAge: 2 * time.Minute}

// observe runs a GC pass at now with used as the images containers use, so
// g records first sight and last use without removing anything.
func observe(t *testing.T, g *imageGC, st *fakeImageStore, used map[string]bool, now time.Time) {
	t.Helper()
	st.used = used
	if removed, err := g.pass(context.Background(), st, neverFull, now); err != nil || len(removed) > 0 {
		t.Fatalf("observing pass removed %v, %v", removed, err)
	}
}

// When the Runtime disk has less free than the floor, unused images go from
// every node of every cluster in one ranking, the one used longest ago
// first, each node's copy of an image ranked by that node's own use, until
// free space, measured again, is back to the floor plus a margin (#1227).
func TestImageGCFloorFreesOldestUsedAcrossNodes(t *testing.T) {
	disk := &fakeDisk{free: 8 * gib}
	img := func(id string) criImage { return criImage{ID: id, Size: 2 * gib} }
	stores := map[string]*fakeImageStore{
		"a1": {disk: disk, imgs: []criImage{img("x"), img("y")}},
		"a2": {disk: disk, imgs: []criImage{img("x")}},
		"b1": {disk: disk, imgs: []criImage{img("z"), img("w")}},
	}
	gcs := map[string]*imageGC{"a1": {}, "a2": {}, "b1": {}}
	t5 := gcT0.Add(5 * time.Minute)
	observe(t, gcs["a1"], stores["a1"], map[string]bool{"x": true}, gcT0)
	observe(t, gcs["a2"], stores["a2"], nil, gcT0)
	observe(t, gcs["b1"], stores["b1"], map[string]bool{"z": true}, gcT0)
	observe(t, gcs["a1"], stores["a1"], map[string]bool{"y": true}, t5)
	observe(t, gcs["a2"], stores["a2"], nil, t5)
	observe(t, gcs["b1"], stores["b1"], map[string]bool{"w": true}, t5)
	for _, st := range stores {
		st.used = nil
	}
	var nodes []floorNode
	for _, n := range []struct{ cluster, node string }{{"c1", "a1"}, {"c1", "a2"}, {"c2", "b1"}} {
		nodes = append(nodes, floorNode{cluster: n.cluster, node: n.node, st: stores[n.node], gc: gcs[n.node], minAge: 2 * time.Minute})
	}
	res, err := floorPass(context.Background(), nodes, 10*gib, gcT0.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	// From 8 GiB free, 2 GiB a removal: four removals reach 16 GiB, past
	// 10 + 5. a2's x was never used; a1's x and b1's z were last used at
	// T0, a1 before b1 by name; then a1's y and b1's w at T5.
	var got []string
	for _, r := range res.removed {
		got = append(got, r.node+"/"+r.img.ID)
	}
	if want := []string{"a2/x", "a1/x", "b1/z", "a1/y"}; !slices.Equal(got, want) {
		t.Errorf("removed %v, want %v", got, want)
	}
	if res.free != 16*gib || res.breached {
		t.Errorf("free %d GiB, breached %v; want 16 GiB, false", res.free/gib, res.breached)
	}
	if got := stores["b1"].imgs; len(got) != 1 || got[0].ID != "w" {
		t.Errorf("b1 kept %v, want w", got)
	}
}

// Above the floor, a floor pass removes nothing and lists no images.
func TestImageGCFloorAboveFloorRemovesNothing(t *testing.T) {
	st := &fakeImageStore{disk: &fakeDisk{free: 11 * gib}, imgs: []criImage{{ID: "old", Size: gib}}}
	res, err := floorPass(context.Background(), []floorNode{{cluster: "c", node: "n", st: st, gc: &imageGC{}}}, 10*gib, gcT0.Add(time.Hour))
	if err != nil || len(res.removed) > 0 || res.breached || res.free != 11*gib {
		t.Errorf("floorPass = %+v, %v; want nothing removed, 11 GiB free", res, err)
	}
}

// Below the floor, images in use (by ID, tag or digest), pinned and
// preloaded ones, and ones first seen within the minimum age still stay;
// with nothing else to remove, the floor stays breached.
func TestImageGCFloorKeepsProtectedImages(t *testing.T) {
	var preload string
	for ref := range preloadedRefs {
		preload = ref
		break
	}
	st := &fakeImageStore{disk: &fakeDisk{free: gib}, imgs: []criImage{
		{ID: "used", Tags: []string{"app:v1"}, Size: gib},
		{ID: "used-by-id", Size: gib},
		{ID: "used-by-digest", Digests: []string{"app@sha256:aa"}, Size: gib},
		{ID: "pinned", Size: gib, Pinned: true},
		{ID: "system", Tags: []string{preload}, Size: gib},
		{ID: "old", Size: gib},
	}}
	g := &imageGC{}
	st.used = map[string]bool{"app:v1": true, "used-by-id": true, "app@sha256:aa": true}
	observe(t, g, st, st.used, gcT0)
	// young is first seen by the floor pass itself, within the minimum age.
	st.imgs = append(st.imgs, criImage{ID: "young", Size: gib})
	res, err := floorPass(context.Background(), []floorNode{{cluster: "c", node: "n", st: st, gc: g, minAge: 2 * time.Minute}}, 10*gib, gcT0.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if got := st.removed; !slices.Equal(got, []string{"old"}) {
		t.Errorf("removed %v, want only old", got)
	}
	if !res.breached || res.free != 2*gib {
		t.Errorf("free %d GiB, breached %v; want 2 GiB, still breached", res.free/gib, res.breached)
	}
}

// floorCluster stores a RUNNING cluster name with one node of the default
// pool (100 GB boot disk), whose GC never fires on these tests' sizes.
func floorCluster(t *testing.T, s *Service, name, node string) string {
	t.Helper()
	rec := &clusterRecord{Int: clusterInternal{Project: "gc-project", Location: "us-central1-a", Name: name,
		Nodes: []nodeRecord{{Name: node, Pool: "default-pool"}}}}
	rec.setCluster(&containerpb.Cluster{Name: name, Id: "id-" + name, Status: containerpb.Cluster_RUNNING,
		NodePools: []*containerpb.NodePool{{Name: "default-pool", Config: &containerpb.NodeConfig{}}}})
	if err := s.env.Store.Update(func(tx store.Tx) error { return putCluster(tx, rec) }); err != nil {
		t.Fatal(err)
	}
	return rec.key()
}

// In a Deterministic Instance, advancing the Emulator clock runs the
// free-space floor across every running cluster's nodes (#1227): below
// 10 GiB free, unused images go from both clusters until 15 GiB is free.
// A pass that ends still below the floor logs one warning, naming the free
// space, the floor and each node's image storage, and logs it again only
// after free space has been back above the floor.
func TestImageGCFloorOnClockAdvance(t *testing.T) {
	s := newDeterministicService(t)
	var logs bytes.Buffer
	s.env.Log = slog.New(slog.NewTextHandler(&logs, nil))
	disk := &fakeDisk{free: 8 * gib}
	img := func(id string) criImage { return criImage{ID: id, Size: 2 * gib} }
	stores := map[string]*fakeImageStore{
		floorCluster(t, s, "c1", "n1"): {disk: disk, imgs: []criImage{img("a"), img("b")}},
		floorCluster(t, s, "c2", "n2"): {disk: disk, imgs: []criImage{img("c"), img("d")}},
	}
	s.testImageStore = func(key, node string) nodeImageStore { return stores[key] }
	warnings := func() int { return strings.Count(logs.String(), "level=WARN") }
	ctx := context.Background()
	advance := func(d time.Duration) {
		t.Helper()
		s.env.Clock.(*clock.Offset).Advance(d)
		if err := s.ClockAdvanced(ctx); err != nil {
			t.Fatal(err)
		}
	}

	advance(0) // every image first seen just now: none can go yet
	if n := warnings(); n != 1 {
		t.Fatalf("%d warnings with 8 GiB free and nothing removable, want 1:\n%s", n, &logs)
	}
	for _, want := range []string{"free=8589934592", "floor=10737418240", "gc-project/us-central1-a/c1/n1=4294967296"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("warning does not name %s:\n%s", want, &logs)
		}
	}
	advance(5 * time.Minute)
	var removed int
	for key, st := range stores {
		if len(st.removed) != 2 {
			t.Errorf("cluster %s removed %v, want both its images", key, st.removed)
		}
		removed += len(st.removed)
	}
	if disk.free != 16*gib || removed != 4 {
		t.Errorf("free %d GiB after %d removals, want 16 GiB after 4", disk.free/gib, removed)
	}
	if n := warnings(); n != 1 {
		t.Errorf("%d warnings after freeing to 16 GiB, want still 1", n)
	}

	disk.free = gib // something other than images filled the disk
	advance(time.Minute)
	advance(time.Minute)
	if n := warnings(); n != 2 {
		t.Errorf("%d warnings after crossing the floor again, want 2 (one per crossing)", n)
	}
}

// gkeImageGcFreeSpace 0 turns the floor off: below any free space, a clock
// advance removes nothing and warns of nothing.
func TestImageGCFloorOff(t *testing.T) {
	s := newDeterministicService(t)
	s.env.Config.GKEImageGCFreeSpace = "0"
	var logs bytes.Buffer
	s.env.Log = slog.New(slog.NewTextHandler(&logs, nil))
	st := &fakeImageStore{disk: &fakeDisk{free: 0}, imgs: []criImage{{ID: "old", Size: gib}}}
	floorCluster(t, s, "c", "n")
	s.testImageStore = func(string, string) nodeImageStore { return st }
	for range 2 {
		s.env.Clock.(*clock.Offset).Advance(10 * time.Minute)
		if err := s.ClockAdvanced(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(st.removed) > 0 || strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("floor off: removed %v, logs:\n%s", st.removed, &logs)
	}
}

// Each node's copy of an image is its own entry: when removing the copy
// used longest ago frees enough, the other node's copy stays.
func TestImageGCFloorRemovesOneCopyOfAnImage(t *testing.T) {
	disk := &fakeDisk{free: 9 * gib}
	stores := map[string]*fakeImageStore{
		"n1": {disk: disk, imgs: []criImage{{ID: "x", Size: 6 * gib}}},
		"n2": {disk: disk, imgs: []criImage{{ID: "x", Size: 6 * gib}}},
	}
	gcs := map[string]*imageGC{"n1": {}, "n2": {}}
	observe(t, gcs["n1"], stores["n1"], map[string]bool{"x": true}, gcT0)
	observe(t, gcs["n2"], stores["n2"], map[string]bool{"x": true}, gcT0)
	observe(t, gcs["n2"], stores["n2"], map[string]bool{"x": true}, gcT0.Add(5*time.Minute))
	stores["n1"].used, stores["n2"].used = nil, nil
	var nodes []floorNode
	for _, n := range []string{"n1", "n2"} {
		nodes = append(nodes, floorNode{cluster: "c", node: n, st: stores[n], gc: gcs[n], minAge: 2 * time.Minute})
	}
	res, err := floorPass(context.Background(), nodes, 10*gib, gcT0.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	// 9 + 6 = 15 GiB, the floor plus the margin: n1's copy, last used at
	// T0, goes; n2's, used at T5, stays.
	if !slices.Equal(stores["n1"].removed, []string{"x"}) || len(stores["n2"].removed) > 0 {
		t.Errorf("removed n1 %v, n2 %v; want only n1's x", stores["n1"].removed, stores["n2"].removed)
	}
	if res.free != 15*gib || res.breached {
		t.Errorf("free %d GiB, breached %v; want 15 GiB, false", res.free/gib, res.breached)
	}
}

// A pass whose removals fail does not know what is left, so it is not
// taken as having ended below the floor: no warning is logged on it.
func TestImageGCFloorFailedRemovalDoesNotWarn(t *testing.T) {
	s := newDeterministicService(t)
	var logs bytes.Buffer
	s.env.Log = slog.New(slog.NewTextHandler(&logs, nil))
	disk := &fakeDisk{free: 20 * gib}
	st := &fakeImageStore{disk: disk, imgs: []criImage{{ID: "old", Size: gib}}, failRemove: true}
	floorCluster(t, s, "c", "n")
	s.testImageStore = func(string, string) nodeImageStore { return st }
	for _, free := range []int64{20 * gib, gib, gib} { // above, then below
		disk.free = free
		if err := s.ClockAdvanced(context.Background()); err != nil {
			t.Fatal(err)
		}
		s.env.Clock.(*clock.Offset).Advance(10 * time.Minute)
	}
	if len(st.removed) != 0 {
		t.Fatalf("removed %v", st.removed)
	}
	if strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("warned after removals failed:\n%s", &logs)
	}
}
