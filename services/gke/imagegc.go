package gke

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/internal/store"
)

// Node image GC (#1052, ADR 0073). The kubelet's own image GC measures the
// image filesystem it sees, which on a node is the container runtime's
// whole disk, so it fires only once the host is full. The emulator runs
// GC instead against the node pool's boot disk size, as GKE's kubelet does
// against the node's boot disk.

const (
	// imageGCInterval is how often GC runs outside a Deterministic
	// Instance; in one, it runs only when the Emulator clock is advanced.
	imageGCInterval = time.Minute
	// Defaults of a node pool's kubeletConfig, as on GKE.
	defaultImageGCHigh   = 85
	defaultImageGCLow    = 80
	defaultImageGCMinAge = 2 * time.Minute
	// crictlTimeout bounds each crictl command in a node.
	crictlTimeout = 30 * time.Second
)

// criImage is an image in a node's container runtime.
type criImage struct {
	ID      string
	Tags    []string
	Digests []string
	// Size is the image's content, as the CRI reports it.
	Size   int64
	Pinned bool
}

// nodeImageStore is a node's image storage.
type nodeImageStore interface {
	// snapshotBytes is what the image filesystem's unpacked snapshots use.
	snapshotBytes(ctx context.Context) (int64, error)
	images(ctx context.Context) ([]criImage, error)
	// inUse returns the images any container on the node, running or
	// not, was created from, by ID or reference (imageUsed).
	inUse(ctx context.Context) (map[string]bool, error)
	remove(ctx context.Context, id string) error
}

// imagePolicy is a node's image GC configuration.
type imagePolicy struct {
	// capacity is the boot disk's size in bytes.
	capacity int64
	// high and low are the thresholds, in percent of capacity: GC starts
	// at high and frees down to low.
	high, low int64
	// minAge is how long an image is kept after it was first seen.
	minAge time.Duration
}

// imageRecord is when GC first saw an image, and last saw it in use, on
// the Emulator clock.
type imageRecord struct {
	firstDetected, lastUsed time.Time
}

// imageGC is one node's image GC state. Like the kubelet's, it lives in
// memory: after a restart every image counts as just seen.
type imageGC struct {
	records map[string]*imageRecord // image ID → record
}

// pass runs one GC pass at now. It returns the images it removed.
func (g *imageGC) pass(ctx context.Context, st nodeImageStore, p imagePolicy, now time.Time) ([]criImage, error) {
	imgs, err := st.images(ctx)
	if err != nil {
		return nil, err
	}
	used, err := st.inUse(ctx)
	if err != nil {
		return nil, err
	}
	if g.records == nil {
		g.records = map[string]*imageRecord{}
	}
	seen := map[string]bool{}
	content := int64(0)
	for _, img := range imgs {
		seen[img.ID] = true
		content += img.Size
		r := g.records[img.ID]
		if r == nil {
			r = &imageRecord{firstDetected: now}
			g.records[img.ID] = r
		}
		if imageUsed(img, used) {
			r.lastUsed = now
		}
	}
	for id := range g.records {
		if !seen[id] {
			delete(g.records, id)
		}
	}
	snap, err := st.snapshotBytes(ctx)
	if err != nil {
		return nil, err
	}
	usage := content + snap
	if usage*100 < p.high*p.capacity {
		return nil, nil
	}
	toFree := usage - p.low*p.capacity/100
	// A removed image frees its content and its snapshots, which the CRI
	// does not report per image: it is credited its share of all image
	// storage, its content scaled by usage over content.
	scale := 1.0
	if content > 0 {
		scale = float64(usage) / float64(content)
	}
	var cands []criImage
	for _, img := range imgs {
		r := g.records[img.ID]
		if imageUsed(img, used) || img.Pinned || preloaded(img) || now.Sub(r.firstDetected) < p.minAge {
			continue
		}
		cands = append(cands, img)
	}
	slices.SortFunc(cands, func(a, b criImage) int {
		ra, rb := g.records[a.ID], g.records[b.ID]
		return cmp.Or(ra.lastUsed.Compare(rb.lastUsed), ra.firstDetected.Compare(rb.firstDetected), cmp.Compare(a.ID, b.ID))
	})
	var removed []criImage
	var errs []error
	freed := int64(0)
	for _, img := range cands {
		if freed >= toFree {
			break
		}
		if err := st.remove(ctx, img.ID); err != nil {
			errs = append(errs, fmt.Errorf("removing image %s: %w", img.ID, err))
			continue
		}
		delete(g.records, img.ID)
		freed += int64(float64(img.Size) * scale)
		removed = append(removed, img)
	}
	return removed, errors.Join(errs...)
}

// preloadedRefs are the system images nodes import at start (versions'
// Preload), as the CRI names them.
var preloadedRefs = func() map[string]bool {
	m := map[string]bool{}
	for _, v := range versions {
		for _, ref := range v.Preload {
			m["docker.io/"+ref] = true
		}
	}
	return m
}()

// preloaded reports whether img is a system image nodes import at start:
// a private node could not pull it again.
func preloaded(img criImage) bool {
	return slices.ContainsFunc(img.Tags, func(t string) bool { return preloadedRefs[t] })
}

// imageUsed reports whether a container named img in used, by ID, tag or
// digest.
func imageUsed(img criImage, used map[string]bool) bool {
	return used[img.ID] || slices.ContainsFunc(img.Tags, func(r string) bool { return used[r] }) ||
		slices.ContainsFunc(img.Digests, func(r string) bool { return used[r] })
}

// crictlStore is a node's image storage, read and changed by running
// crictl in the node container: exec runs a command there and returns its
// output.
type crictlStore struct {
	exec func(ctx context.Context, cmd []string) ([]byte, error)
}

// crictlJSON runs crictl with args and decodes its JSON output into v.
func (c crictlStore) crictlJSON(ctx context.Context, v any, args ...string) error {
	out, err := c.exec(ctx, append([]string{"crictl"}, args...))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(out, v); err != nil {
		return fmt.Errorf("crictl %v: %w", args, err)
	}
	return nil
}

func (c crictlStore) snapshotBytes(ctx context.Context) (int64, error) {
	var out struct {
		Status struct {
			ImageFilesystems []struct {
				UsedBytes struct {
					Value string `json:"value"`
				} `json:"usedBytes"`
			} `json:"imageFilesystems"`
		} `json:"status"`
	}
	if err := c.crictlJSON(ctx, &out, "imagefsinfo", "-o", "json"); err != nil {
		return 0, err
	}
	var n int64
	for _, fs := range out.Status.ImageFilesystems {
		v, err := strconv.ParseInt(fs.UsedBytes.Value, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("crictl imagefsinfo: usedBytes %q: %w", fs.UsedBytes.Value, err)
		}
		n += v
	}
	return n, nil
}

func (c crictlStore) images(ctx context.Context) ([]criImage, error) {
	var out struct {
		Images []struct {
			ID          string   `json:"id"`
			RepoTags    []string `json:"repoTags"`
			RepoDigests []string `json:"repoDigests"`
			Size        string   `json:"size"`
			Pinned      bool     `json:"pinned"`
		} `json:"images"`
	}
	if err := c.crictlJSON(ctx, &out, "images", "-o", "json"); err != nil {
		return nil, err
	}
	imgs := make([]criImage, 0, len(out.Images))
	for _, i := range out.Images {
		size, err := strconv.ParseInt(i.Size, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("crictl images: %s size %q: %w", i.ID, i.Size, err)
		}
		imgs = append(imgs, criImage{ID: i.ID, Tags: i.RepoTags, Digests: i.RepoDigests, Size: size, Pinned: i.Pinned})
	}
	return imgs, nil
}

func (c crictlStore) inUse(ctx context.Context) (map[string]bool, error) {
	var out struct {
		Containers []struct {
			Image struct {
				Image string `json:"image"`
			} `json:"image"`
			ImageRef string `json:"imageRef"`
		} `json:"containers"`
	}
	if err := c.crictlJSON(ctx, &out, "ps", "-a", "-o", "json"); err != nil {
		return nil, err
	}
	used := map[string]bool{}
	for _, ct := range out.Containers {
		for _, r := range []string{ct.ImageRef, ct.Image.Image} {
			if r != "" {
				used[r] = true
			}
		}
	}
	return used, nil
}

func (c crictlStore) remove(ctx context.Context, id string) error {
	_, err := c.exec(ctx, []string{"crictl", "rmi", id})
	return err
}

// poolImagePolicy is the image GC configuration of np's nodes: its boot
// disk size and its kubeletConfig's thresholds and minimum age, or GKE's
// defaults.
func poolImagePolicy(np *containerpb.NodePool) imagePolicy {
	cfg := np.GetConfig()
	disk := int64(cfg.GetDiskSizeGb())
	if disk <= 0 {
		disk = defaultDiskSizeGB
	}
	kc := cfg.GetKubeletConfig()
	p := imagePolicy{capacity: disk << 30, high: defaultImageGCHigh, low: defaultImageGCLow, minAge: defaultImageGCMinAge}
	if v := int64(kc.GetImageGcHighThresholdPercent()); v > 0 && v <= 100 {
		p.high = v
	}
	if v := int64(kc.GetImageGcLowThresholdPercent()); v > 0 && v <= 100 {
		p.low = v
	}
	p.low = min(p.low, p.high)
	if d, err := time.ParseDuration(kc.GetImageMinimumGcAge()); err == nil && d >= 0 {
		p.minAge = d
	}
	return p
}

// imageGCLoop runs image GC on the cluster's nodes every imageGCInterval,
// outside a Deterministic Instance (ClockAdvanced runs it there).
func (s *Service) imageGCLoop(ctx context.Context, key string) {
	if s.env.Config.Deterministic {
		return
	}
	t := time.NewTicker(imageGCInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.imageGCPass(ctx, key)
	}
}

// imageGCAll runs image GC on every running cluster's nodes.
func (s *Service) imageGCAll(ctx context.Context) {
	var keys []string
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, r := range listClusters(tx, "") {
			keys = append(keys, r.key())
		}
		return nil
	})
	for _, key := range keys {
		s.imageGCPass(ctx, key)
	}
}

// imageGCPass runs image GC on each node of cluster key, if it is running.
// Failures are logged: a node that is starting or gone is tried again on
// the next pass.
func (s *Service) imageGCPass(ctx context.Context, key string) {
	rt := s.rtFor(key)
	rt.gcMu.Lock()
	defer rt.gcMu.Unlock()
	rec, err := s.loadKey(key)
	if err != nil {
		return
	}
	c := rec.cluster()
	if c.Status != containerpb.Cluster_RUNNING && c.Status != containerpb.Cluster_RECONCILING {
		return
	}
	storeOf, err := s.nodeImageStores(ctx, c, key)
	if err != nil {
		return
	}
	if rt.imageGC == nil {
		rt.imageGC = map[string]*imageGC{}
	}
	nodes := map[string]bool{}
	for _, n := range rec.Int.Nodes {
		nodes[n.Name] = true
		st := storeOf(n.Name)
		g := rt.imageGC[n.Name]
		if g == nil {
			g = &imageGC{}
			rt.imageGC[n.Name] = g
		}
		removed, err := g.pass(ctx, st, poolImagePolicy(findPool(c, n.Pool)), s.env.Clock.Now())
		if len(removed) > 0 {
			var names []string
			for _, img := range removed {
				names = append(names, imageName(img))
			}
			s.env.Log.Info("gke: node image GC removed unused images", "cluster", key, "node", n.Name, "images", names)
		}
		if err != nil && ctx.Err() == nil {
			s.env.Log.Debug("gke: node image GC", "cluster", key, "node", n.Name, "err", err)
		}
	}
	for n := range rt.imageGC {
		if !nodes[n] {
			delete(rt.imageGC, n)
		}
	}
}

// imageName is how logs name img: a tag, else a digest, else its ID.
func imageName(img criImage) string {
	if len(img.Tags) > 0 {
		return img.Tags[0]
	}
	if len(img.Digests) > 0 {
		return img.Digests[0]
	}
	return img.ID
}

// nodeImageStores returns how to reach the image storage of cluster key's
// nodes, by node name.
func (s *Service) nodeImageStores(ctx context.Context, c *containerpb.Cluster, key string) (func(node string) nodeImageStore, error) {
	if s.testImageStore != nil {
		return func(node string) nodeImageStore { return s.testImageStore(key, node) }, nil
	}
	d, err := s.deps(ctx)
	if err != nil {
		return nil, err
	}
	return func(node string) nodeImageStore { return nodeCrictl(d, nodeContainer(d, c, node)) }, nil
}

// nodeCrictl is the image storage of node container name.
func nodeCrictl(d *deps, name string) crictlStore {
	return crictlStore{exec: func(ctx context.Context, cmd []string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, crictlTimeout)
		defer cancel()
		res, err := d.rt.Exec(ctx, name, cmd, nil)
		if err != nil {
			return nil, err
		}
		if res.ExitCode != 0 {
			return nil, fmt.Errorf("%s: exit %d: %s", strings.Join(cmd, " "), res.ExitCode, strings.TrimSpace(string(res.Stderr)))
		}
		return res.Stdout, nil
	}}
}
