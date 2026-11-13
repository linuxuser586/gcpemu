//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/hosttest"
	"github.com/linuxuser586/gcpemu/internal/instance"
)

// TestGKESizing measures what an Autopilot cluster's containers use on the
// Runtime machine, for the README's GKE sizing table (#1069). It is not an
// acceptance test: it asserts only that the cluster gets to each step,
// and logs a table. `just gke-sizing` runs it; GCPEMU_GKE_SIZING_NODES
// sets the larger node count (default 5, at least 4).
//
// Phases, each sampled with `docker stats` every sampleEvery for
// sampleFor after settleFor:
//
//   - 1 node, idle: the cluster as created, kube-system pods only.
//   - 1 node, load: the load below.
//   - N nodes, idle: one "hold" pause pod per node, each requesting 1 vCPU
//     so no two fit on an ek-standard-2 node, which makes Autopilot scale
//     to N.
//   - N nodes, load: the load below.
//
// Load is a client listing every pod, node and event in the cluster in turn,
// every 100 ms, and a "churn" Deployment of two pause pods per node (50m,
// 52Mi) whose pods are replaced every 20 s.
//
// Pause pods use almost no memory, so a node's figures are the node's own
// (k3s agent, containerd and the system pods it runs), not its
// pods'.
func TestGKESizing(t *testing.T) {
	emutest.RequireRuntime(t)
	if os.Getenv("GCPEMU_GKE_SIZING") != "1" {
		t.Skip("GCPEMU_GKE_SIZING != 1: `just gke-sizing` runs this measurement")
	}
	cli := os.Getenv("GCPEMU_GKE_SIZING_CLI")
	if cli == "" {
		cli = "docker"
	}
	if _, err := exec.LookPath(cli); err != nil {
		t.Skipf("%s is not on PATH", cli)
	}
	n := 5
	if v := os.Getenv("GCPEMU_GKE_SIZING_NODES"); v != "" {
		var err error
		if n, err = strconv.Atoi(v); err != nil || n < 4 {
			t.Fatalf("GCPEMU_GKE_SIZING_NODES=%q: want a number of at least 4", v)
		}
	}
	inst := emutest.Start(t, []string{"compute", "gke"}, emutest.WithConfig(func(c *config.Config) { c.GKEMaxNodes = n }))
	gw := inst.Endpoint("gateway")
	const project, region, name = "sizing", "us-central1", "sizing"
	cluster := "projects/" + project + "/locations/" + region + "/clusters/" + name
	post(t, gw, "/container/v1/projects/"+project+"/locations/"+region+"/clusters",
		map[string]any{"cluster": map[string]any{"name": name, "autopilot": map[string]any{"enabled": true}}})
	var c struct{ Status, CurrentMasterVersion string }
	hosttest.Eventually(t, 10*time.Minute, "cluster RUNNING", func() bool {
		return getJSON(gw, "/container/v1/"+cluster, &c) && c.Status == "RUNNING"
	}, nil)
	k := newKube(t, gw, cluster)
	sizingWaitNodes(t, k, 1)

	// docker info's JSON; podman's has another schema, so with podman the
	// host line leaves these empty.
	var info struct {
		ServerVersion, OperatingSystem, KernelVersion string
		NCPU                                          int
		MemTotal                                      int64
	}
	if cli == "docker" {
		out, err := exec.Command(cli, "info", "--format", "{{json .}}").Output()
		if err == nil {
			err = json.Unmarshal(out, &info)
		}
		if err != nil {
			t.Fatalf("docker info: %v", err)
		}
	}
	images := map[string]bool{}
	cts, err := inst.Internal().Containers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, ct := range cts {
		if ct.Service == "gke" {
			images[ct.Image] = true
		}
	}
	t.Logf("gcpemu %s, GKE %s (%s), %s %s on %s (kernel %s), %d CPUs, %.1f GiB",
		instance.Version, c.CurrentMasterVersion, strings.Join(slices.Sorted(maps.Keys(images)), ", "),
		cli, info.ServerVersion, info.OperatingSystem, info.KernelVersion, info.NCPU, float64(info.MemTotal)/(1<<30))

	measure := func(label string, nodes int) sizingPhase {
		time.Sleep(settleFor)
		p := sizingSample(t, inst, cli, label, nodes)
		t.Log(p)
		return p
	}
	idle1 := measure("1 node, idle", 1)
	stop := sizingLoad(t, k, 1)
	load1 := measure("1 node, load", 1)
	stop()

	k.apply(sizingPauseDeployment("hold", n, "1", "1Gi", ""))
	sizingWaitNodes(t, k, n)
	hosttest.Eventually(t, 10*time.Minute, "hold pods running", func() bool { return k.deploymentReady("default", "hold") },
		func() string { return k.diagnose("default", "app=hold") })
	idleN := measure(fmt.Sprintf("%d nodes, idle", n), n)
	stop = sizingLoad(t, k, n)
	loadN := measure(fmt.Sprintf("%d nodes, load", n), n)
	stop()

	t.Log("\n" + sizingTable(idle1, load1, idleN, loadN))
}

const (
	settleFor   = 3 * time.Minute
	sampleFor   = 3 * time.Minute
	sampleEvery = 10 * time.Second
)

// sizingWaitNodes waits for the cluster to have n Ready nodes.
func sizingWaitNodes(t *testing.T, k *kube, n int) {
	t.Helper()
	ready := 0
	hosttest.Eventually(t, 15*time.Minute, fmt.Sprintf("%d nodes Ready", n), func() bool {
		var l struct {
			Items []struct {
				Status struct {
					Conditions []struct{ Type, Status string }
				}
			}
		}
		if !k.get("/api/v1/nodes", &l) {
			return false
		}
		ready = 0
		for _, nd := range l.Items {
			for _, c := range nd.Status.Conditions {
				if c.Type == "Ready" && c.Status == "True" {
					ready++
				}
			}
		}
		return ready == n
	}, func() string { return fmt.Sprintf("%d Ready", ready) })
}

// sizingPauseDeployment is a Deployment of replicas pause pods requesting
// cpu and mem, whose pod template carries restartedAt (a change replaces
// every pod).
func sizingPauseDeployment(name string, replicas int, cpu, mem, restartedAt string) map[string]any {
	d := deployment("default", name, replicas, map[string]any{"name": "c", "image": pauseImage,
		"resources": map[string]any{"requests": map[string]string{"cpu": cpu, "memory": mem}}})
	tmpl := d["spec"].(map[string]any)["template"].(map[string]any)
	tmpl["metadata"].(map[string]any)["annotations"] = map[string]string{"gcpemu.test/restartedAt": restartedAt}
	return d
}

// sizingLoad starts the load on a cluster of nodes nodes, and returns a
// function that stops it and removes the churn Deployment.
func sizingLoad(t *testing.T, k *kube, nodes int) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		paths := []string{"/api/v1/pods", "/api/v1/nodes", "/api/v1/events"}
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for i := 0; ; i++ {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				k.do(http.MethodGet, paths[i%len(paths)], "", nil, nil)
			}
		}
	}()
	churn := func(restartedAt int) map[string]any {
		return sizingPauseDeployment("churn", 2*nodes, "50m", "52Mi", strconv.Itoa(restartedAt))
	}
	k.apply(churn(0))
	go func() {
		defer wg.Done()
		tick := time.NewTicker(20 * time.Second)
		defer tick.Stop()
		for i := 1; ; i++ {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				// k.apply's PATCH without its t.Fatalf, which must not
				// run off the test's goroutine; a missed roll only
				// lightens the load.
				b, _ := json.Marshal(churn(i))
				k.do(http.MethodPatch, objPath("Deployment", "default", "churn")+"?fieldManager=gcpemu-e2e&force=true",
					"application/apply-patch+yaml", b, nil)
			}
		}
	}()
	return func() {
		cancel()
		wg.Wait()
		k.delete("Deployment", "default", "churn")
	}
}

// roleUse is what a phase's containers of one role used: the sum over
// them of each sample's figures.
type roleUse struct {
	count            int
	meanMem, peakMem int64 // bytes, summed over the role's containers
	meanCPU, peakCPU int64 // millicores, summed over the role's containers
}

// sizingPhase is one phase's figures by role ("server", "node").
type sizingPhase struct {
	label string
	nodes int
	roles map[string]roleUse
}

func (p sizingPhase) String() string {
	var b strings.Builder
	b.WriteString(p.label + ":")
	for _, r := range []string{"server", "node"} {
		u := p.roles[r]
		fmt.Fprintf(&b, " %d %s: mem mean %s peak %s, CPU mean %dm peak %dm;", u.count, r,
			mib(u.meanMem), mib(u.peakMem), u.meanCPU, u.peakCPU)
	}
	return b.String()
}

func mib(b int64) string { return fmt.Sprintf("%d MiB", (b+1<<19)>>20) }

// sizingSample samples the Instance's GKE containers for sampleFor.
func sizingSample(t *testing.T, inst *emutest.Instance, cli, label string, nodes int) sizingPhase {
	t.Helper()
	cts, err := inst.Internal().Containers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	role := map[string]string{}
	args := []string{"stats", "--no-stream", "--format", "{{json .}}"}
	for _, ct := range cts {
		if ct.Service == "gke" && (ct.Role == "server" || ct.Role == "node") {
			role[ct.Name] = ct.Role
			args = append(args, ct.Name)
		}
	}
	type sums struct{ mem, cpu int64 }
	var samples []map[string]sums
	for end := time.Now().Add(sampleFor); time.Now().Before(end); time.Sleep(sampleEvery) {
		out, err := exec.Command(cli, args...).Output()
		if err != nil {
			t.Fatalf("%s stats: %v", cli, err)
		}
		s := map[string]sums{}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			name, mem, cpu, err := parseDockerStats([]byte(line))
			if err != nil {
				t.Fatal(err)
			}
			r, ok := role[strings.TrimPrefix(name, "/")]
			if !ok {
				t.Fatalf("%s stats: container %q was not asked for", cli, name)
			}
			s[r] = sums{s[r].mem + mem, s[r].cpu + cpu}
		}
		samples = append(samples, s)
	}
	p := sizingPhase{label: label, nodes: nodes, roles: map[string]roleUse{}}
	for _, r := range role {
		u := p.roles[r]
		u.count++
		p.roles[r] = u
	}
	for r, u := range p.roles {
		for _, s := range samples {
			u.meanMem += s[r].mem
			u.meanCPU += s[r].cpu
			u.peakMem = max(u.peakMem, s[r].mem)
			u.peakCPU = max(u.peakCPU, s[r].cpu)
		}
		u.meanMem /= int64(len(samples))
		u.meanCPU /= int64(len(samples))
		p.roles[r] = u
	}
	return p
}

// sizingTable is the README table: the Cluster server's figures, the
// first node's, and each further node's, the difference between the
// N-node and 1-node phases divided by N-1.
func sizingTable(idle1, load1, idleN, loadN sizingPhase) string {
	var b strings.Builder
	b.WriteString("| Container | Idle memory (mean / peak) | Idle CPU (mean) | Load memory (mean / peak) | Load CPU (mean / peak) |\n")
	b.WriteString("|---|---|---|---|---|\n")
	row := func(what string, idle, load roleUse) {
		fmt.Fprintf(&b, "| %s | %s / %s | %dm | %s / %s | %dm / %dm |\n", what,
			mib(idle.meanMem), mib(idle.peakMem), idle.meanCPU, mib(load.meanMem), mib(load.peakMem), load.meanCPU, load.peakCPU)
	}
	row("Cluster server, 1 node", idle1.roles["server"], load1.roles["server"])
	row(fmt.Sprintf("Cluster server, %d nodes", idleN.nodes), idleN.roles["server"], loadN.roles["server"])
	row("First node", idle1.roles["node"], load1.roles["node"])
	per := func(one, many roleUse) roleUse {
		d := int64(idleN.nodes - 1)
		return roleUse{meanMem: (many.meanMem - one.meanMem) / d, peakMem: (many.peakMem - one.peakMem) / d,
			meanCPU: (many.meanCPU - one.meanCPU) / d, peakCPU: (many.peakCPU - one.peakCPU) / d}
	}
	row("Each further node", per(idle1.roles["node"], idleN.roles["node"]), per(load1.roles["node"], loadN.roles["node"]))
	return b.String()
}

// parseDockerStats reads one line of `docker stats --no-stream --format
// '{{json .}}'` (or podman's): the container's name, its memory use in
// bytes, and its CPU use in millicores (100% is one CPU).
func parseDockerStats(line []byte) (name string, memBytes, cpuMilli int64, err error) {
	var s struct{ Name, MemUsage, CPUPerc string }
	if err := json.Unmarshal(line, &s); err != nil {
		return "", 0, 0, err
	}
	used, _, _ := strings.Cut(s.MemUsage, "/")
	memBytes, err = parseSize(strings.TrimSpace(used))
	if err != nil {
		return "", 0, 0, fmt.Errorf("MemUsage %q: %w", s.MemUsage, err)
	}
	// Podman prints "--" before it has two CPU readings.
	if pct, err := strconv.ParseFloat(strings.TrimSuffix(s.CPUPerc, "%"), 64); err == nil {
		cpuMilli = int64(math.Round(pct * 10))
	}
	return s.Name, memBytes, cpuMilli, nil
}

// sizeUnits are the suffixes docker (binary) and podman (decimal) print,
// longest first so that "MiB" is tried before "B".
var sizeUnits = []struct {
	suffix string
	mult   float64
}{
	{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40},
	{"kB", 1e3}, {"KB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"TB", 1e12},
	{"B", 1},
}

// parseSize parses a size such as "512.5MiB" or "64kB" into bytes.
func parseSize(s string) (int64, error) {
	for _, u := range sizeUnits {
		if num, ok := strings.CutSuffix(s, u.suffix); ok {
			v, err := strconv.ParseFloat(num, 64)
			if err != nil {
				return 0, err
			}
			return int64(math.Round(v * u.mult)), nil
		}
	}
	return 0, fmt.Errorf("no size unit in %q", s)
}
