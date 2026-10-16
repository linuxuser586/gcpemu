package gke

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/internal/clock"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// TestAwaitNodesFailsFast (#230): when a node's container exits before
// the node is Ready, waiting fails at once with the exit code and the
// tail of the container's logs, instead of after nodeReadyWait.
func TestAwaitNodesFailsFast(t *testing.T) {
	for _, tt := range []struct {
		status string
		died   bool
	}{{"exited", true}, {"running", false}} {
		t.Run(tt.status, func(t *testing.T) {
			cfg := config.Defaults()
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			s := New(&emu.Env{Config: &cfg, Store: store.NewMemory(), Clock: clock.Real{}, IDs: emu.NewIDs(true), Log: log,
				Auth: emu.NewPolicyAuthorizer(config.IAMOff, log), Endpoints: emu.NewEndpoints()}).(*Service)
			c := &containerpb.Cluster{Name: "c0", Id: "0123456789abcdef"}
			rec := &clusterRecord{Int: clusterInternal{Project: "p", Location: "us-central1-a", Name: "c0",
				Nodes: []nodeRecord{{Name: "n1", Pool: "default-pool"}, {Name: "n2", Pool: "default-pool"}}}}
			rec.setCluster(c)
			if err := s.env.Store.Update(func(tx store.Tx) error { return putCluster(tx, rec) }); err != nil {
				t.Fatal(err)
			}
			// The API server knows n1 (Ready) but not n2.
			kube := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `{"items":[{"metadata":{"name":"n1"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}`)
			}))
			defer kube.Close()
			s.rtFor(rec.key()).kube = &kubeClient{base: kube.URL, hc: kube.Client()}

			rt := &runtime.Manager{InstanceID: "i1"}
			n2 := nodeContainerName(rt, c, "n2")
			docker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/containers/"+n2+"/json"):
					_ = json.NewEncoder(w).Encode(map[string]any{"Name": "/" + n2,
						"State": map[string]any{"Status": tt.status, "Running": !tt.died, "ExitCode": map[bool]int{true: 1}[tt.died]}})
				case strings.HasSuffix(r.URL.Path, "/containers/"+n2+"/logs"):
					msg := "level=fatal msg=\"k3s agent failed\"\n"
					hdr := make([]byte, 8)
					hdr[0] = 2
					binary.BigEndian.PutUint32(hdr[4:], uint32(len(msg)))
					_, _ = w.Write(append(hdr, msg...))
				default:
					t.Errorf("unexpected runtime request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer docker.Close()
			cl, err := runtime.New("tcp://" + strings.TrimPrefix(docker.URL, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			rt.Client = cl

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			start := time.Now()
			err = s.awaitNodes(ctx, rec.key(), "", rt)
			if !tt.died {
				if err == nil || !strings.Contains(err.Error(), "nodes [n2] did not become ready") {
					t.Fatalf("awaitNodes = %v; want n2 not ready", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "node n2 exited with code 1") || !strings.Contains(err.Error(), "k3s agent failed") {
				t.Fatalf("awaitNodes = %v; want n2's exit code and logs", err)
			}
			if d := time.Since(start); d > time.Second {
				t.Errorf("awaitNodes took %v; want it to fail fast", d)
			}
		})
	}
}

// TestAwaitNodesDeadlineMidPoll (#355): when the deadline cuts off a
// nodes request, the error names the nodes not Ready at the last poll
// that succeeded, not every node.
func TestAwaitNodesDeadlineMidPoll(t *testing.T) {
	cfg := config.Defaults()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(&emu.Env{Config: &cfg, Store: store.NewMemory(), Clock: clock.Real{}, IDs: emu.NewIDs(true), Log: log,
		Auth: emu.NewPolicyAuthorizer(config.IAMOff, log), Endpoints: emu.NewEndpoints()}).(*Service)
	rec := &clusterRecord{Int: clusterInternal{Project: "p", Location: "us-central1-a", Name: "c0",
		Nodes: []nodeRecord{{Name: "n1", Pool: "default-pool"}, {Name: "n2", Pool: "default-pool"}}}}
	rec.setCluster(&containerpb.Cluster{Name: "c0", Id: "0123456789abcdef"})
	if err := s.env.Store.Update(func(tx store.Tx) error { return putCluster(tx, rec) }); err != nil {
		t.Fatal(err)
	}
	// The first poll sees n1 Ready; later polls hang until cancelled.
	var polls atomic.Int32
	kube := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if polls.Add(1) > 1 {
			<-r.Context().Done()
			return
		}
		_, _ = io.WriteString(w, `{"items":[{"metadata":{"name":"n1"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}`)
	}))
	defer kube.Close()
	s.rtFor(rec.key()).kube = &kubeClient{base: kube.URL, hc: kube.Client()}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := s.awaitNodes(ctx, rec.key(), "", nil)
	if err == nil || !strings.Contains(err.Error(), "nodes [n2] did not become ready") {
		t.Fatalf("awaitNodes = %v; want n2 not ready", err)
	}
	if polls.Load() < 2 {
		t.Fatalf("polls = %d; want the deadline to cut off a later poll", polls.Load())
	}

	// With no poll that succeeded, every node is named.
	polls.Store(1)
	ctx, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err = s.awaitNodes(ctx, rec.key(), "", nil)
	if err == nil || !strings.Contains(err.Error(), "nodes [n1 n2] did not become ready") {
		t.Fatalf("awaitNodes = %v; want n1 and n2 not ready", err)
	}
}

// TestEvacuateRetriesBusy (#230): a process that joins the root cgroup
// while the agent delegates controllers (the emulator's exec into a node
// that just started) is moved too, instead of killing the node.
func TestEvacuateRetriesBusy(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpu memory pids\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	procs := filepath.Join(root, "cgroup.procs")
	if err := os.WriteFile(procs, []byte("1\n7\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var moved []string
	var subtree string
	busy := 0
	write := func(name string, data []byte, _ os.FileMode) error {
		switch filepath.Base(name) {
		case "cgroup.procs":
			moved = append(moved, string(data))
			return nil
		case "cgroup.subtree_control":
			if busy == 0 {
				// An exec'd process joined root after it was read.
				busy++
				_ = os.WriteFile(procs, []byte("42\n"), 0o644)
				return &os.PathError{Op: "write", Path: name, Err: syscall.EBUSY}
			}
			subtree = string(data)
			return nil
		}
		t.Fatalf("unexpected write %s", name)
		return nil
	}
	if err := evacuate(root, write); err != nil {
		t.Fatalf("evacuate = %v", err)
	}
	if strings.Join(moved, ",") != "1,7,42" {
		t.Errorf("moved %v; want 1,7,42", moved)
	}
	if subtree != "+cpu +memory +pids" {
		t.Errorf("subtree_control = %q", subtree)
	}

	// Other errors still fail.
	err := evacuate(root, func(name string, _ []byte, _ os.FileMode) error {
		if filepath.Base(name) == "cgroup.subtree_control" {
			return &os.PathError{Op: "write", Path: name, Err: syscall.EINVAL}
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "delegating cgroup controllers") {
		t.Errorf("evacuate = %v; want the delegation error", err)
	}
}
