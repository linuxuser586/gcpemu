package gke

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Autopilot node provisioning (FR-GKE-015, ADR 0031). Each Autopilot
// cluster runs a controller that, like GKE's cluster autoscaler, adds a
// node when a pod is Pending for lack of capacity and removes a node once
// it has been empty for scaleDownDelay, never the cluster's last. A new
// node takes the smallest shape (autopilotShapes) its first pod fits, and
// the compute class and Spot its pods select as labels. Nodes of every
// cluster share the host node limit (FR-GKE-003); a pod that needs a node
// beyond it stays Pending with FailedScaleUp, as on GKE when a scale-up
// fails for quota.

const (
	autoscaleInterval = 2 * time.Second
	// scaleDownDelay is how long a node stays empty before it is removed.
	scaleDownDelay = 30 * time.Second

	computeClassLabel = "cloud.google.com/compute-class"
	spotLabel         = "cloud.google.com/gke-spot"
)

// autoscaleLoop provisions an Autopilot cluster's nodes while ctx lives.
func (s *Service) autoscaleLoop(ctx context.Context, key string) {
	if rec, err := s.loadKey(key); err != nil || !autopilotOn(rec.cluster()) {
		return
	}
	a := &autoscaler{s: s, key: key, empty: map[string]time.Time{}, told: map[string]string{}}
	t := time.NewTicker(autoscaleInterval)
	defer t.Stop()
	for {
		if err := a.step(ctx); err != nil && ctx.Err() == nil {
			s.env.Log.Debug("gke: Autopilot node provisioning", "cluster", key, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// autoscaler is one Autopilot cluster's provisioning state.
type autoscaler struct {
	s   *Service
	key string
	// empty is when each node was first seen empty.
	empty map[string]time.Time
	// told is the last scale-up event reason given each Pending pod, by UID.
	told map[string]string
}

// res is an amount of node resources: CPU in millicores, memory in bytes,
// and pods.
type res struct{ cpu, mem, pods int64 }

func (r res) fits(in res) bool { return r.cpu <= in.cpu && r.mem <= in.mem && r.pods <= in.pods }
func (r res) minus(o res) res  { return res{r.cpu - o.cpu, r.mem - o.mem, r.pods - o.pods} }
func (r res) plus(o res) res   { return res{r.cpu + o.cpu, r.mem + o.mem, r.pods + o.pods} }

// resOf reads a node's capacity or allocatable.
func resOf(m map[string]string) res {
	return res{cpu: milli(m["cpu"]), mem: units(m["memory"]), pods: units(m["pods"])}
}

func milli(q string) int64 {
	v, _ := parseQuantity(q)
	return int64(math.Ceil(v * 1000))
}

func units(q string) int64 {
	v, _ := parseQuantity(q)
	return int64(math.Ceil(v))
}

type podContainer struct {
	Resources struct {
		Requests map[string]string `json:"requests"`
	} `json:"resources"`
	RestartPolicy string `json:"restartPolicy"`
}

type nodeSelectorTerm struct {
	MatchExpressions []struct {
		Key      string   `json:"key"`
		Operator string   `json:"operator"`
		Values   []string `json:"values"`
	} `json:"matchExpressions"`
}

// scalePod is a pod as the autoscaler reads it.
type scalePod struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		NodeName       string            `json:"nodeName"`
		NodeSelector   map[string]string `json:"nodeSelector"`
		Containers     []podContainer    `json:"containers"`
		InitContainers []podContainer    `json:"initContainers"`
		Overhead       map[string]string `json:"overhead"`
		Affinity       struct {
			NodeAffinity struct {
				Required struct {
					Terms []nodeSelectorTerm `json:"nodeSelectorTerms"`
				} `json:"requiredDuringSchedulingIgnoredDuringExecution"`
			} `json:"nodeAffinity"`
		} `json:"affinity"`
	} `json:"spec"`
	Status struct {
		Phase      string `json:"phase"`
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
			Reason string `json:"reason"`
		} `json:"conditions"`
	} `json:"status"`
}

// requests is what the scheduler counts for the pod: its containers and
// sidecars, or its largest init container if more, plus its overhead.
func (p *scalePod) requests() res {
	req := func(c podContainer) res {
		return res{cpu: milli(c.Resources.Requests["cpu"]), mem: units(c.Resources.Requests["memory"])}
	}
	var run, init res
	for _, c := range p.Spec.Containers {
		run = run.plus(req(c))
	}
	for _, c := range p.Spec.InitContainers {
		r := req(c)
		if c.RestartPolicy == "Always" {
			run = run.plus(r)
			continue
		}
		init.cpu, init.mem = max(init.cpu, r.cpu), max(init.mem, r.mem)
	}
	out := res{cpu: max(run.cpu, init.cpu), mem: max(run.mem, init.mem), pods: 1}
	return out.plus(res{cpu: milli(p.Spec.Overhead["cpu"]), mem: units(p.Spec.Overhead["memory"])})
}

// unschedulable reports whether the scheduler found no node for the pod.
func (p *scalePod) unschedulable() bool {
	if p.Spec.NodeName != "" || p.Status.Phase != "Pending" || p.Metadata.DeletionTimestamp != nil {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == "PodScheduled" {
			return c.Status == "False" && c.Reason == "Unschedulable"
		}
	}
	return false
}

// done reports whether the pod no longer holds its node's resources.
func (p *scalePod) done() bool {
	return p.Status.Phase == "Succeeded" || p.Status.Phase == "Failed"
}

func (p *scalePod) ownedBy(kind string) bool {
	for _, o := range p.Metadata.OwnerReferences {
		if o.Kind == kind {
			return true
		}
	}
	return false
}

// matches reports whether a node with labels satisfies the pod's node
// selector and required node affinity.
func (p *scalePod) matches(labels map[string]string) bool {
	for k, v := range p.Spec.NodeSelector {
		if labels[k] != v {
			return false
		}
	}
	terms := p.Spec.Affinity.NodeAffinity.Required.Terms
	if len(terms) == 0 {
		return true
	}
	for _, t := range terms {
		if t.matches(labels) {
			return true
		}
	}
	return false
}

func (t nodeSelectorTerm) matches(labels map[string]string) bool {
	for _, e := range t.MatchExpressions {
		v, ok := labels[e.Key]
		in := false
		for _, w := range e.Values {
			in = in || v == w
		}
		var m bool
		switch e.Operator {
		case "In":
			m = ok && in
		case "NotIn":
			m = !in
		case "Exists":
			m = ok
		case "DoesNotExist":
			m = !ok
		case "Gt", "Lt":
			a, err1 := strconv.ParseInt(v, 10, 64)
			var b int64
			var err2 error
			if len(e.Values) == 1 {
				b, err2 = strconv.ParseInt(e.Values[0], 10, 64)
			}
			m = ok && err1 == nil && err2 == nil && len(e.Values) == 1 && (e.Operator == "Gt" && a > b || e.Operator == "Lt" && a < b)
		}
		if !m {
			return false
		}
	}
	return true
}

// provisionLabels are the labels a node provisioned for the pod gets: the
// compute class and Spot its node selector asks for.
func (p *scalePod) provisionLabels() map[string]string {
	out := map[string]string{}
	if v := p.Spec.NodeSelector[computeClassLabel]; v != "" {
		out[computeClassLabel] = v
	}
	if p.Spec.NodeSelector[spotLabel] == "true" {
		out[spotLabel] = "true"
		out["cloud.google.com/gke-provisioning"] = "spot"
	}
	return out
}

// bin is a node pods can be placed on: a Ready one, one starting, or one
// to add.
type bin struct {
	node   nodeRecord
	labels map[string]string
	free   res
	// add marks a node to add, and starting one not yet Ready.
	add, starting bool
	pods          []*scalePod
}

// step runs one round of provisioning: scale up for unschedulable pods,
// or, when no pod waits for a node, remove a node that has stayed empty.
func (a *autoscaler) step(ctx context.Context) error {
	s := a.s
	rec, err := s.loadKey(a.key)
	if err != nil {
		return err
	}
	c := rec.cluster()
	if c.Status != containerpb.Cluster_RUNNING || s.opBusy(a.key) {
		return nil
	}
	np := findPool(c, defaultPoolName)
	if np == nil {
		return nil
	}
	kc, err := s.kube(a.key)
	if err != nil {
		return err
	}
	var nodes kubeList[kubeNode]
	if err := kc.get(ctx, "/api/v1/nodes", &nodes); err != nil {
		return err
	}
	var pods kubeList[scalePod]
	if err := kc.get(ctx, "/api/v1/pods", &pods); err != nil {
		return err
	}
	byName := map[string]*kubeNode{}
	for i := range nodes.Items {
		byName[nodes.Items[i].Metadata.Name] = &nodes.Items[i]
	}
	used := map[string]res{}
	busy := map[string]bool{} // nodes running pods that keep them
	ds := map[string]res{}
	var pending []*scalePod
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.unschedulable() {
			pending = append(pending, p)
			continue
		}
		if p.Spec.NodeName == "" || p.done() {
			continue
		}
		r := p.requests()
		used[p.Spec.NodeName] = used[p.Spec.NodeName].plus(r)
		switch {
		case p.ownedBy("DaemonSet"):
			ds[p.Spec.NodeName] = ds[p.Spec.NodeName].plus(r)
		case p.Metadata.Namespace == "kube-system", p.Metadata.Annotations["kubernetes.io/config.mirror"] != "", p.Metadata.DeletionTimestamp != nil:
		default:
			busy[p.Spec.NodeName] = true
		}
	}
	// A new node runs the DaemonSet pods the busiest node runs, and
	// cannot advertise more than the host has.
	var overhead res
	var host *res
	var arch string
	for _, n := range nodes.Items {
		d := ds[n.Metadata.Name]
		overhead = res{max(overhead.cpu, d.cpu), max(overhead.mem, d.mem), max(overhead.pods, d.pods)}
		if host == nil && n.Status.Capacity != nil {
			h := resOf(n.Status.Capacity)
			h.mem -= evictionKi << 10
			host = &h
		}
		if arch == "" {
			arch = n.Metadata.Labels["kubernetes.io/arch"]
		}
	}
	capOf := func(n nodeRecord) res {
		al := shapeOf(n).allocatable()
		r := res{cpu: al.CPUMilli, mem: al.MemoryKi << 10, pods: maxPods(c, np)}
		if host != nil {
			r.cpu, r.mem = min(r.cpu, host.cpu), min(r.mem, host.mem)
		}
		return r.minus(overhead)
	}
	labelsOf := func(n nodeRecord) map[string]string {
		m := map[string]string{"kubernetes.io/os": "linux", "kubernetes.io/arch": arch}
		for _, l := range nodeLabels(c, np, n) {
			k, v, _ := strings.Cut(l, "=")
			m[k] = v
		}
		return m
	}
	var bins []*bin
	starting := false
	for _, n := range rec.Int.Nodes {
		kn := byName[n.Name]
		switch {
		case kn == nil || !kn.ready():
			starting = true
			bins = append(bins, &bin{node: n, labels: labelsOf(n), free: capOf(n), starting: true})
		case !kn.Spec.Unschedulable:
			bins = append(bins, &bin{node: n, labels: kn.Metadata.Labels, free: resOf(kn.Status.Allocatable).minus(used[n.Name])})
		}
	}
	a.forget(pending)
	if len(pending) > 0 {
		waiting, err := a.scaleUp(ctx, kc, rec, c, np, pending, bins, capOf, labelsOf)
		if err != nil || waiting {
			return err
		}
	}
	if starting {
		return nil
	}
	return a.scaleDown(ctx, rec, byName, busy)
}

// scaleUp places the unschedulable pods on nodes, adding the nodes they
// need within the host node limit, and reports whether any pod waits for
// a node to start.
func (a *autoscaler) scaleUp(ctx context.Context, kc *kubeClient, rec *clusterRecord, c *containerpb.Cluster, np *containerpb.NodePool,
	pending []*scalePod, bins []*bin, capOf func(nodeRecord) res, labelsOf func(nodeRecord) map[string]string) (bool, error) {
	s := a.s
	sort.SliceStable(pending, func(i, j int) bool {
		ri, rj := pending[i].requests(), pending[j].requests()
		if ri.cpu != rj.cpu {
			return ri.cpu > rj.cpu
		}
		return ri.mem > rj.mem
	})
	limit := maxNodes()
	room := limit - s.nodesElsewhere(a.key) - len(rec.Int.Nodes)
	perZone := map[string]int{}
	for _, n := range rec.Int.Nodes {
		perZone[n.Zone]++
	}
	h := shortHash(c.Id + "/" + np.Name)
	taken := map[string]bool{}
	for _, n := range rec.Int.Nodes {
		taken[n.Name] = true
	}
	waiting := false
	var failed, unfit []*scalePod
	var added []*bin
pods:
	for _, p := range pending {
		r := p.requests()
		for _, b := range bins {
			if r.fits(b.free) && p.matches(b.labels) {
				b.free = b.free.minus(r)
				b.pods = append(b.pods, p)
				waiting = waiting || b.add || b.starting
				continue pods
			}
		}
		n, ok := a.newNode(c, np, p, r, perZone, capOf, labelsOf)
		if !ok {
			unfit = append(unfit, p)
			continue
		}
		if room <= 0 {
			failed = append(failed, p)
			waiting = true
			continue
		}
		for n.Name == "" || taken[n.Name] {
			n.Name = nodeName(c, np, h, s.env.IDs.Hex(2))
		}
		taken[n.Name] = true
		room--
		perZone[n.Zone]++
		b := &bin{node: n, labels: labelsOf(n), free: capOf(n).minus(r), add: true, pods: []*scalePod{p}}
		bins = append(bins, b)
		added = append(added, b)
		waiting = true
	}
	for _, p := range unfit {
		a.tell(ctx, kc, p, "Normal", "NotTriggerScaleUp", "pod didn't trigger scale-up: it wouldn't fit on any node Autopilot can add")
	}
	for _, p := range failed {
		zones := strings.Join(np.Locations, ",")
		if z := p.Spec.NodeSelector["topology.kubernetes.io/zone"]; z != "" {
			zones = z
		}
		a.tell(ctx, kc, p, "Warning", "FailedScaleUp", fmt.Sprintf(
			"Node scale up in zones %s associated with this pod failed: gcpemu node limit exceeded (%d nodes; set %s to raise it). Pod is at risk of not being scheduled.",
			zones, limit, maxNodesEnv))
	}
	if len(added) == 0 {
		return waiting, nil
	}
	if err := a.addNodes(added); err != nil {
		return true, err
	}
	before := len(rec.Int.Nodes)
	for _, b := range added {
		ig := strings.Replace(igURL(np, b.node.Zone), "/instanceGroupManagers/", "/instanceGroups/", 1)
		msg := fmt.Sprintf("pod triggered scale-up: [{%s %d->%d (max: %d)}]", ig, before, before+len(added), limit)
		for _, p := range b.pods {
			a.tell(ctx, kc, p, "Normal", "TriggeredScaleUp", msg)
		}
	}
	a.startNodes(ctx, added)
	return true, nil
}

// newNode plans a node for pod p, which requests r: in the zone it
// selects or the one with fewest nodes, of the smallest shape it fits,
// with its compute class and Spot labels. ok is false when no such node
// would take the pod.
func (a *autoscaler) newNode(c *containerpb.Cluster, np *containerpb.NodePool, p *scalePod, r res, perZone map[string]int,
	capOf func(nodeRecord) res, labelsOf func(nodeRecord) map[string]string) (nodeRecord, bool) {
	zone := ""
	if z := p.Spec.NodeSelector["topology.kubernetes.io/zone"]; z != "" {
		zone = z
	} else {
		for _, z := range np.Locations {
			if zone == "" || perZone[z] < perZone[zone] {
				zone = z
			}
		}
	}
	n := nodeRecord{Pool: np.Name, Zone: zone, Password: a.s.env.IDs.Hex(16), Labels: p.provisionLabels()}
	if len(n.Labels) == 0 {
		n.Labels = nil
	}
	for _, sh := range autopilotShapes {
		n.MachineType = sh.Name
		if r.fits(capOf(n)) && slices.Contains(np.Locations, zone) && p.matches(labelsOf(n)) {
			return n, true
		}
	}
	return nodeRecord{}, false
}

// igURL is the URL of pool np's instance group manager in zone.
func igURL(np *containerpb.NodePool, zone string) string {
	for _, u := range np.InstanceGroupUrls {
		if strings.Contains(u, "/zones/"+zone+"/") {
			return u
		}
	}
	return ""
}

// addNodes records the planned nodes, unless the cluster has left
// RUNNING or the host node limit would be exceeded.
func (a *autoscaler) addNodes(added []*bin) error {
	return a.s.env.Store.Update(func(tx store.Tx) error {
		rec, err := getCluster(tx, a.key)
		if err != nil {
			return err
		}
		c := rec.cluster()
		if c.Status != containerpb.Cluster_RUNNING {
			return apierr.FailedPrecondition("cluster %s is %s", a.key, c.Status)
		}
		if err := a.s.checkNodeLimit(tx, "", len(added)); err != nil {
			return err
		}
		for _, b := range added {
			rec.Int.Nodes = append(rec.Int.Nodes, b.node)
		}
		refreshCounts(rec, c)
		rec.setCluster(c)
		return putCluster(tx, rec)
	})
}

// startNodes starts the added nodes' containers; a node that fails to
// start is removed, so that a later round can try again.
func (a *autoscaler) startNodes(ctx context.Context, added []*bin) {
	s := a.s
	var gone []nodeRecord
	d, err := s.deps(ctx)
	if err == nil {
		var rec *clusterRecord
		if rec, err = s.loadKey(a.key); err == nil {
			sn, err2 := d.vpc.SubnetNetwork(ctx, rec.Int.Subnetwork)
			if err2 == nil {
				var mu sync.Mutex
				var wg sync.WaitGroup
				for _, b := range added {
					wg.Add(1)
					go func() {
						defer wg.Done()
						if err := s.startNode(ctx, d, a.key, sn, b.node, false); err != nil {
							s.env.Log.Warn("gke: starting an Autopilot node failed", "cluster", a.key, "node", b.node.Name, "err", err)
							mu.Lock()
							gone = append(gone, b.node)
							mu.Unlock()
						}
					}()
				}
				wg.Wait()
			}
			err = err2
		}
	}
	if err != nil {
		s.env.Log.Warn("gke: starting Autopilot nodes failed", "cluster", a.key, "err", err)
		for _, b := range added {
			gone = append(gone, b.node)
		}
	}
	if len(gone) > 0 && ctx.Err() == nil {
		a.remove(ctx, gone)
	}
}

// scaleDown removes one node that has stayed empty for scaleDownDelay,
// unless it is the cluster's last. busy names the nodes running pods
// other than DaemonSet, mirror and kube-system ones.
func (a *autoscaler) scaleDown(ctx context.Context, rec *clusterRecord, nodes map[string]*kubeNode, busy map[string]bool) error {
	now := a.s.env.Clock.Now()
	var victim *nodeRecord
	for i, n := range rec.Int.Nodes {
		if kn := nodes[n.Name]; kn == nil || !kn.ready() || busy[n.Name] {
			delete(a.empty, n.Name)
			continue
		}
		since, ok := a.empty[n.Name]
		if !ok {
			a.empty[n.Name] = now
			continue
		}
		if now.Sub(since) >= scaleDownDelay && victim == nil {
			victim = &rec.Int.Nodes[i]
		}
	}
	if victim == nil || len(rec.Int.Nodes) <= 1 {
		return nil
	}
	delete(a.empty, victim.Name)
	a.remove(ctx, []nodeRecord{*victim})
	return nil
}

// remove deletes nodes and their records.
func (a *autoscaler) remove(ctx context.Context, gone []nodeRecord) {
	s := a.s
	if err := s.removeNodes(ctx, a.key, gone); err != nil {
		s.env.Log.Warn("gke: removing Autopilot nodes", "cluster", a.key, "err", err)
	}
	drop := map[string]bool{}
	for _, n := range gone {
		drop[n.Name] = true
	}
	_ = s.updateCluster(a.key, func(rec *clusterRecord, c *containerpb.Cluster) error {
		var keep []nodeRecord
		for _, n := range rec.Int.Nodes {
			if !drop[n.Name] {
				keep = append(keep, n)
			}
		}
		rec.Int.Nodes = keep
		refreshCounts(rec, c)
		return nil
	})
}

// tell records a scale-up event on a pod, once per reason while it waits.
func (a *autoscaler) tell(ctx context.Context, kc *kubeClient, p *scalePod, typ, reason, msg string) {
	if a.told[p.Metadata.UID] == reason {
		return
	}
	now := a.s.env.Clock.Now().UTC().Format(time.RFC3339)
	ev := map[string]any{
		"apiVersion": "v1", "kind": "Event",
		"metadata": map[string]any{"generateName": p.Metadata.Name + ".", "namespace": p.Metadata.Namespace},
		"involvedObject": map[string]any{
			"apiVersion": "v1", "kind": "Pod", "namespace": p.Metadata.Namespace, "name": p.Metadata.Name,
			"uid": p.Metadata.UID, "resourceVersion": p.Metadata.ResourceVersion,
		},
		"reason": reason, "message": msg, "type": typ,
		"source":         map[string]any{"component": "cluster-autoscaler"},
		"firstTimestamp": now, "lastTimestamp": now, "count": 1,
	}
	if err := kc.create(ctx, "/api/v1/namespaces/"+url.PathEscape(p.Metadata.Namespace)+"/events", ev, nil); err != nil {
		a.s.env.Log.Debug("gke: recording a scale-up event", "pod", p.Metadata.Namespace+"/"+p.Metadata.Name, "err", err)
		return
	}
	a.told[p.Metadata.UID] = reason
}

// forget drops the event state of pods no longer Pending.
func (a *autoscaler) forget(pending []*scalePod) {
	keep := map[string]bool{}
	for _, p := range pending {
		keep[p.Metadata.UID] = true
	}
	for uid := range a.told {
		if !keep[uid] {
			delete(a.told, uid)
		}
	}
}

// opBusy reports whether an operation holds the cluster.
func (s *Service) opBusy(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, busy := s.busy[key]
	return busy
}
