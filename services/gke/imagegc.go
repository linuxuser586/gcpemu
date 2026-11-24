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

	"github.com/linuxuser586/gcpemu/internal/config"
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
	// freeBytes is the free space of the Runtime disk, which holds the
	// node's volume.
	freeBytes(ctx context.Context) (int64, error)
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

// observe records imgs as seen at now, and the ones in used as used at
// now, and forgets images no longer there. It returns the images' content
// size.
func (g *imageGC) observe(imgs []criImage, used map[string]bool, now time.Time) int64 {
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
	return content
}

// removable reports whether GC at now may remove img from a node whose
// containers use used: not in use, pinned or preloaded, and first seen
// at least minAge ago.
func (g *imageGC) removable(img criImage, used map[string]bool, minAge time.Duration, now time.Time) bool {
	return !imageUsed(img, used) && !img.Pinned && !preloaded(img) && now.Sub(g.records[img.ID].firstDetected) >= minAge
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
	content := g.observe(imgs, used, now)
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
		if !g.removable(img, used, p.minAge, now) {
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

// floorMargin is how far above the free-space floor a floor pass frees
// the Runtime disk to, so GC does not hover at the floor.
const floorMargin = 5 << 30

// floorNode is a node in a floor pass: where it is, its image storage, its
// GC records and its pool's imageMinimumGcAge.
type floorNode struct {
	cluster, node string
	st            nodeImageStore
	gc            *imageGC
	minAge        time.Duration
}

// floorRemoval is an image a floor pass removed from a node.
type floorRemoval struct {
	cluster, node string
	img           criImage
}

// floorResult is what a floor pass did: whether it knows the Runtime
// disk's free space at its end (it could read it, and every removal it
// tried worked), the images it removed, that free space, and whether that
// is still under the floor. When it is, storage is each
// node's image storage (content and snapshots), in nodes' order.
type floorResult struct {
	measured bool
	removed  []floorRemoval
	free     int64
	breached bool
	storage  []int64
}

// floorPass runs GC's free-space floor at now (#1227, ADR 0073). Every
// node's volume is on the one Runtime disk, so free space is read in any
// one of them. Below floor, the unused images of all nodes are ranked
// together, as each node's GC ranks its own, and removed until free space,
// read again after each removal, reaches floor plus floorMargin.
func floorPass(ctx context.Context, nodes []floorNode, floor int64, now time.Time) (floorResult, error) {
	if len(nodes) == 0 {
		return floorResult{}, nil
	}
	free, err := runtimeDiskFree(ctx, nodes)
	if err != nil {
		return floorResult{}, err
	}
	res := floorResult{measured: true, free: free}
	if free >= floor {
		return res, nil
	}
	type cand struct {
		n   *floorNode
		img criImage
		rec *imageRecord
	}
	var cands []cand
	var errs []error
	for i := range nodes {
		n := &nodes[i]
		imgs, err := n.st.images(ctx)
		if err == nil {
			var used map[string]bool
			if used, err = n.st.inUse(ctx); err == nil {
				n.gc.observe(imgs, used, now)
				for _, img := range imgs {
					if n.gc.removable(img, used, n.minAge, now) {
						cands = append(cands, cand{n, img, n.gc.records[img.ID]})
					}
				}
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("node %s/%s: %w", n.cluster, n.node, err))
		}
	}
	slices.SortFunc(cands, func(a, b cand) int {
		return cmp.Or(a.rec.lastUsed.Compare(b.rec.lastUsed), a.rec.firstDetected.Compare(b.rec.firstDetected),
			cmp.Compare(a.n.cluster, b.n.cluster), cmp.Compare(a.n.node, b.n.node), cmp.Compare(a.img.ID, b.img.ID))
	})
	for _, c := range cands {
		if res.free >= floor+floorMargin {
			break
		}
		if err := c.n.st.remove(ctx, c.img.ID); err != nil {
			errs = append(errs, fmt.Errorf("node %s/%s: removing image %s: %w", c.n.cluster, c.n.node, c.img.ID, err))
			res.measured = false // an image it could not remove may remain
			continue
		}
		delete(c.n.gc.records, c.img.ID)
		res.removed = append(res.removed, floorRemoval{c.n.cluster, c.n.node, c.img})
		free, err := runtimeDiskFree(ctx, nodes)
		if err != nil {
			errs = append(errs, err)
			res.measured = false
			break
		}
		res.free = free
	}
	res.breached = res.free < floor
	if res.breached {
		res.storage = make([]int64, len(nodes))
		for i, n := range nodes {
			imgs, err := n.st.images(ctx)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			snap, err := n.st.snapshotBytes(ctx)
			if err != nil {
				errs = append(errs, err)
			}
			for _, img := range imgs {
				snap += img.Size
			}
			res.storage[i] = snap
		}
	}
	return res, errors.Join(errs...)
}

// runtimeDiskFree reads the Runtime disk's free space in the first of
// nodes that answers: one that is starting or gone may not.
func runtimeDiskFree(ctx context.Context, nodes []floorNode) (int64, error) {
	var errs []error
	for _, n := range nodes {
		free, err := n.st.freeBytes(ctx)
		if err == nil {
			return free, nil
		}
		errs = append(errs, fmt.Errorf("node %s/%s: %w", n.cluster, n.node, err))
	}
	return 0, errors.Join(errs...)
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

func (c crictlStore) freeBytes(ctx context.Context) (int64, error) {
	out, err := c.exec(ctx, []string{"df", "-B1", "-P", k3sDataDir})
	if err != nil {
		return 0, err
	}
	// POSIX output: a header, then one line whose fourth field is the
	// bytes available.
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 {
		return 0, fmt.Errorf("df %s: unexpected output %q", k3sDataDir, out)
	}
	f := strings.Fields(lines[1])
	if len(f) < 4 {
		return 0, fmt.Errorf("df %s: unexpected output %q", k3sDataDir, out)
	}
	n, err := strconv.ParseInt(f[3], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("df %s: available %q: %w", k3sDataDir, f[3], err)
	}
	return n, nil
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
		s.imageGCFloor(ctx)
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
	s.imageGCFloor(ctx)
}

// imageGCFloor runs the free-space floor (floorPass) over every running
// cluster's nodes, unless gkeImageGcFreeSpace is 0. It warns once when a
// pass ends below the floor because every image left is in use, pinned,
// preloaded or too new, and again only after a pass has found free space
// back at or above the floor.
func (s *Service) imageGCFloor(ctx context.Context) {
	floor, err := s.env.Config.GKEImageGCFreeSpaceBytes()
	if err != nil || floor <= 0 {
		return // Validate refused an unusable value; 0 is off
	}
	s.floorMu.Lock()
	defer s.floorMu.Unlock()
	var keys []string
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, r := range listClusters(tx, "") {
			keys = append(keys, r.key())
		}
		return nil
	})
	// The pass ranks every cluster's records together, so it holds every
	// cluster's gcMu until it ends, taken in key order. A cluster's own pass
	// takes only its gcMu, so the two cannot deadlock.
	slices.Sort(keys)
	var nodes []floorNode
	for _, key := range keys {
		rt := s.rtFor(key)
		rt.gcMu.Lock()
		defer rt.gcMu.Unlock()
		for _, n := range s.runningGCNodes(ctx, key, rt) {
			nodes = append(nodes, floorNode{cluster: key, node: n.name, st: n.st, gc: n.gc, minAge: n.policy.minAge})
		}
	}
	res, err := floorPass(ctx, nodes, floor, s.env.Clock.Now())
	s.logFloor(ctx, nodes, floor, res, err)
}

// logFloor logs what a floor pass over nodes did: its removals by node, an
// error, and the warning when it ended below the floor (imageGCFloor).
func (s *Service) logFloor(ctx context.Context, nodes []floorNode, floor int64, res floorResult, err error) {
	type at struct{ cluster, node string }
	byNode := map[at][]string{}
	var order []at
	for _, r := range res.removed {
		k := at{r.cluster, r.node}
		if byNode[k] == nil {
			order = append(order, k)
		}
		byNode[k] = append(byNode[k], imageName(r.img))
	}
	for _, k := range order {
		s.env.Log.Info("gke: node image GC removed unused images below the free-space floor", "cluster", k.cluster, "node", k.node, "images", byNode[k], "floor", floor)
	}
	if err != nil && ctx.Err() == nil {
		s.env.Log.Debug("gke: node image GC free-space floor", "err", err)
	}
	switch {
	case !res.measured:
	case res.breached && !s.floorWarned:
		s.floorWarned = true
		args := []any{"free", res.free, "floor", floor}
		for i, n := range nodes {
			args = append(args, n.cluster+"/"+n.node, res.storage[i])
		}
		s.env.Log.Warn("gke: the Runtime disk is still below node image GC's free-space floor after removing every unused node image it could; "+
			"free space on it, or lower "+config.GKEImageGCFreeSpaceSetting+"; each node's image storage follows", args...)
	case !res.breached:
		s.floorWarned = false
	}
}

// imageGCPass runs image GC on each node of cluster key, if it is running.
// Failures are logged: a node that is starting or gone is tried again on
// the next pass.
func (s *Service) imageGCPass(ctx context.Context, key string) {
	rt := s.rtFor(key)
	rt.gcMu.Lock()
	defer rt.gcMu.Unlock()
	for _, n := range s.runningGCNodes(ctx, key, rt) {
		removed, err := n.gc.pass(ctx, n.st, n.policy, s.env.Clock.Now())
		if len(removed) > 0 {
			var names []string
			for _, img := range removed {
				names = append(names, imageName(img))
			}
			s.env.Log.Info("gke: node image GC removed unused images", "cluster", key, "node", n.name, "images", names)
		}
		if err != nil && ctx.Err() == nil {
			s.env.Log.Debug("gke: node image GC", "cluster", key, "node", n.name, "err", err)
		}
	}
}

// gcNode is a node of a running cluster as image GC sees it: its image
// storage, its GC state and its pool's image GC policy.
type gcNode struct {
	name   string
	st     nodeImageStore
	gc     *imageGC
	policy imagePolicy
}

// runningGCNodes returns the nodes of cluster key, or none if it is not
// running, and forgets the GC state of nodes it no longer has. Callers
// hold rt.gcMu.
func (s *Service) runningGCNodes(ctx context.Context, key string, rt *clusterRT) []gcNode {
	rec, err := s.loadKey(key)
	if err != nil {
		return nil
	}
	c := rec.cluster()
	if c.Status != containerpb.Cluster_RUNNING && c.Status != containerpb.Cluster_RECONCILING {
		return nil
	}
	storeOf, err := s.nodeImageStores(ctx, c, key)
	if err != nil {
		return nil
	}
	if rt.imageGC == nil {
		rt.imageGC = map[string]*imageGC{}
	}
	var nodes []gcNode
	have := map[string]bool{}
	for _, n := range rec.Int.Nodes {
		have[n.Name] = true
		g := rt.imageGC[n.Name]
		if g == nil {
			g = &imageGC{}
			rt.imageGC[n.Name] = g
		}
		nodes = append(nodes, gcNode{name: n.Name, st: storeOf(n.Name), gc: g, policy: poolImagePolicy(findPool(c, n.Pool))})
	}
	for n := range rt.imageGC {
		if !have[n] {
			delete(rt.imageGC, n)
		}
	}
	return nodes
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
