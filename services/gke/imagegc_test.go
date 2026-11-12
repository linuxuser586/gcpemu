package gke

import (
	"context"
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
}

func (f *fakeImageStore) snapshotBytes(context.Context) (int64, error) { return f.snapshot, nil }
func (f *fakeImageStore) images(context.Context) ([]criImage, error) {
	return slices.Clone(f.imgs), nil
}
func (f *fakeImageStore) inUse(context.Context) (map[string]bool, error) { return f.used, nil }
func (f *fakeImageStore) remove(_ context.Context, id string) error {
	f.removed = append(f.removed, id)
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
