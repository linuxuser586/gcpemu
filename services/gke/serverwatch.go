package gke

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Watching the Cluster server (ADR 0045).

const (
	// serverExitLimit exits of a Cluster server within serverExitWindow
	// make the Service stop starting it again and mark its cluster
	// DEGRADED.
	serverExitLimit  = 3
	serverExitWindow = 10 * time.Minute
	// waitRetry is the pause before waiting again for a Cluster server
	// whose wait failed while it still ran.
	waitRetry = time.Second
)

// serverStoppedError reports that a watched Cluster server exited.
type serverStoppedError struct {
	code int
	logs string
}

func (e *serverStoppedError) Error() string {
	return fmt.Sprintf("Cluster server exited with code %d: %s", e.code, e.logs)
}

// serverSupervisor starts a cluster's Cluster server again each time it
// exits, until it exits serverExitLimit times within serverExitWindow. A
// restart that fails counts as another exit.
type serverSupervisor struct {
	now func() time.Time
	// wait blocks until container id exits and returns why, or nil once
	// ctx ends.
	wait func(ctx context.Context, id string) error
	// restart starts the server again on its volume, waits until it is
	// ready and returns its container.
	restart func(ctx context.Context) (string, error)
	// warn reports an exit the server is started again after, and
	// degrade the one it is not.
	warn    func(err error)
	degrade func(err error)
}

// run watches container id until ctx ends or the supervisor gives up. It
// gives up with the last failure, and the last k3s log tail seen if that
// failure has none.
func (v *serverSupervisor) run(ctx context.Context, id string) {
	var exits []time.Time
	tail := ""
	for {
		err := v.wait(ctx, id)
		for {
			if ctx.Err() != nil {
				return
			}
			if l := serverLogs(err); l != "" {
				tail = l
			} else if tail != "" {
				err = fmt.Errorf("%w; the Cluster server's last log lines: %s", err, tail)
			}
			now := v.now()
			exits = append(slices.DeleteFunc(exits, func(t time.Time) bool { return now.Sub(t) >= serverExitWindow }), now)
			if len(exits) >= serverExitLimit {
				v.degrade(err)
				return
			}
			v.warn(err)
			var nid string
			if nid, err = v.restart(ctx); err == nil {
				id = nid
				break
			}
		}
	}
}

// serverLogs returns the k3s log tail err carries, if any.
func serverLogs(err error) string {
	var stopped *serverStoppedError
	var exited *serverExitedError
	switch {
	case errors.As(err, &stopped):
		return stopped.logs
	case errors.As(err, &exited):
		return exited.logs
	}
	return ""
}

// serverWatch is a running watch of a cluster's Cluster server.
type serverWatch struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// watchServer watches the cluster's Cluster server on ctx's background
// generation, starting it again each time it exits (ADR 0045), unless it
// is already watched. Reset and Stop end the watch with the generation.
func (s *Service) watchServer(ctx context.Context, d *deps, key string) {
	g := genOf(ctx)
	if g == nil || !s.join(g, 1) {
		return
	}
	rt := s.rtFor(key)
	rt.mu.Lock()
	if rt.watch != nil || rt.server == "" {
		rt.mu.Unlock()
		g.wg.Done()
		return
	}
	wctx, cancel := context.WithCancel(g.ctx)
	w := &serverWatch{cancel: cancel, done: make(chan struct{})}
	rt.watch = w
	id := rt.server
	rt.mu.Unlock()
	v := s.supervisor(d, key)
	go func() {
		defer g.wg.Done()
		defer close(w.done)
		defer cancel()
		v.run(wctx, id)
		rt.mu.Lock()
		if rt.watch == w {
			rt.watch = nil
		}
		rt.mu.Unlock()
	}()
}

// unwatchServer ends the watch of the cluster's Cluster server and waits
// for it, before the Service stops or replaces the server on purpose.
func (s *Service) unwatchServer(key string) {
	rt := s.rtFor(key)
	rt.mu.Lock()
	w := rt.watch
	rt.watch = nil
	rt.mu.Unlock()
	if w != nil {
		w.cancel()
		<-w.done
	}
}

// supervisor returns the supervisor of the cluster's Cluster server.
func (s *Service) supervisor(d *deps, key string) *serverSupervisor {
	v := &serverSupervisor{
		now:     time.Now,
		wait:    s.serverWait,
		restart: func(ctx context.Context) (string, error) { return s.serverRestart(ctx, key) },
		warn: func(err error) {
			s.env.Log.Warn("gke: Cluster server exited; starting it again", "cluster", key, "err", err)
		},
		degrade: func(err error) { s.degradeServer(key, err) },
	}
	if v.wait == nil {
		v.wait = func(ctx context.Context, id string) error { return awaitServerExit(ctx, d, id) }
	}
	if s.serverRestart == nil {
		v.restart = func(ctx context.Context) (string, error) { return s.restartServer(ctx, d, key) }
	}
	return v
}

// awaitServerExit blocks until the Cluster server, container id, exits
// and returns why, or nil once ctx ends. A wait that fails while the
// container still runs, or while the runtime cannot say, is waited again
// after waitRetry: the runtime's API failed, not the server. The exit is
// forgotten by the runtime's start record: the Service starts the server
// again, so it is not Disrupted.
func awaitServerExit(ctx context.Context, d *deps, id string) error {
	for {
		code, err := d.rt.WaitContainer(ctx, id)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			ct, ierr := d.rt.InspectContainer(ctx, id)
			if ierr == nil && ct.Running || ierr != nil && !errors.Is(ierr, runtime.ErrNotFound) {
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(waitRetry):
				}
				continue
			}
		}
		d.rt.ForgetStart(id)
		if err != nil {
			return fmt.Errorf("waiting for the Cluster server: %w", err)
		}
		_, logs, _ := containerExit(ctx, d.rt, id)
		return &serverStoppedError{code: code, logs: logs}
	}
}

// restartServer starts the cluster's Cluster server again on its volume,
// as a bring-up does after an exit (#395), and returns its container.
func (s *Service) restartServer(ctx context.Context, d *deps, key string) (string, error) {
	rec, err := s.loadKey(key)
	if err != nil {
		return "", err
	}
	sn, err := d.vpc.SubnetNetwork(ctx, rec.Int.Subnetwork)
	if err != nil {
		return "", err
	}
	if err := s.bringUpServer(ctx, d, key, sn, rec.cluster().CurrentMasterVersion, true); err != nil {
		return "", err
	}
	// The endpoint keeps its port unless the server's address changed.
	s.writeKubeconfig()
	rt := s.rtFor(key)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.server, nil
}

// degradeServer marks the cluster DEGRADED once its Cluster server kept
// exiting, with err, the last exit. Stopping and starting the Instance
// brings it back.
func (s *Service) degradeServer(key string, err error) {
	s.env.Log.Warn("gke: Cluster server kept exiting; no longer starting it again", "cluster", key, "err", err)
	_ = s.updateCluster(key, func(rec *clusterRecord, c *containerpb.Cluster) error {
		if c.Status == containerpb.Cluster_STOPPING {
			return nil
		}
		c.Status = containerpb.Cluster_DEGRADED
		c.StatusMessage = fmt.Sprintf("The Cluster server exited %d times within %v and is no longer started again; stop and start the instance to bring it back. Last exit: %v",
			serverExitLimit, serverExitWindow, err)
		return nil
	})
}

// degraded returns the resource names of the DEGRADED clusters.
func (s *Service) degraded() []string {
	var out []string
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, r := range listClusters(tx, "") {
			if r.cluster().Status == containerpb.Cluster_DEGRADED {
				out = append(out, ref{Project: r.Int.Project, Location: r.Int.Location, Cluster: r.Int.Name}.clusterName())
			}
		}
		return nil
	})
	sort.Strings(out)
	return out
}
