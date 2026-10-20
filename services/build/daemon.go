package build

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/linuxuser586/gcpemu/internal/agent"
	"github.com/linuxuser586/gcpemu/internal/frontend"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/services/ar"
)

// The Build daemon (ADR 0016) is the Managed container, one per Instance,
// whose Docker daemon every Build's steps use at /var/run/docker.sock: a
// privileged Docker-in-Docker container on the Build network, started on
// the first Build. Its image cache is a volume that outlives Builds,
// Reset and restarts of a Persistent Instance, and goes with the
// Instance's other volumes.
//
// The daemon resolves every LOCATION-docker.pkg.dev to its own loopback,
// where nothing serves HTTPS, and is told each is an insecure registry,
// so it falls back to plain HTTP on port 80. There a Sidecar agent relays
// it to the Build agent's registry proxy, which challenges for Basic
// credentials: the daemon answers with the ones a step's docker CLI sent
// it, which each Build's config.json holds for its service account
// (credentials.go).

const (
	daemonImage = "docker:29.9.0-dind"
	daemonAgent = "build-daemon"
	// daemonEnv passes the registry proxy's address to the daemon's
	// Sidecar agent.
	daemonEnv = "GCPEMU_BUILD_DAEMON"
	// daemonRun is where the daemon's container holds the volume with its
	// socket, which every step mounts at stepRun.
	daemonRun = "/gcpemu-run"
	stepRun   = "/var/run"
	// cloudbuildNetwork is the daemon's network named as on Cloud Build's
	// workers, for `docker run --network=cloudbuild`.
	cloudbuildNetwork = "cloudbuild"
	// labelRegistry records the registry proxy a daemon container relays
	// to: one relaying to an earlier Build agent's is replaced.
	labelRegistry = "gcpemu.build.registry"
	// daemonStart bounds how long a started daemon may take to serve.
	daemonStart = 2 * time.Minute
)

// errPrivileged is a container runtime refusing the Build daemon its
// privileged container.
var errPrivileged = errors.New("the Build daemon needs a privileged container, which the container runtime refused")

func init() { agent.Register(daemonAgent, runDaemonRelay) }

// runDaemonRelay relays the daemon's plain-HTTP registry connections on
// its loopback to the Build agent's registry proxy.
func runDaemonRelay(ctx context.Context, _ []string) error {
	l, err := net.Listen("tcp", "127.0.0.1:80")
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		l.Close()
	}()
	err = frontend.Relay(l, os.Getenv(daemonEnv))
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// daemonVolume names one of the Build daemon's volumes.
func daemonVolume(rt *runtime.Manager, kind string) string {
	return rt.Name("build", "daemon", kind)
}

// buildDaemon returns the Build daemon's container, starting it unless it
// runs and relays to the registry proxy of the Build agent on n.
func (s *Service) buildDaemon(ctx context.Context, rt *runtime.Manager, n buildNet) (string, error) {
	s.daemonMu.Lock()
	defer s.daemonMu.Unlock()
	registry := net.JoinHostPort(n.agent, strconv.Itoa(registryPort))
	if s.daemonID != "" {
		if c, err := rt.InspectContainer(ctx, s.daemonID); err == nil && c.Running && c.Labels[labelRegistry] == registry {
			return s.daemonID, nil
		}
	}
	s.daemonID = ""
	id, err := s.startDaemon(ctx, rt, n, registry)
	s.daemonErr = err
	if err != nil {
		return "", err
	}
	s.daemonID = id
	return id, nil
}

// startDaemon creates and starts the Build daemon's container, and waits
// until the daemon serves and has the cloudbuild network.
func (s *Service) startDaemon(ctx context.Context, rt *runtime.Manager, n buildNet, registry string) (string, error) {
	for _, kind := range []string{"cache", "run"} {
		if err := rt.CreateVolume(ctx, daemonVolume(rt, kind), rt.Labels("build", "", "daemon")); err != nil {
			return "", err
		}
	}
	if err := rt.EnsureImage(ctx, daemonImage, rt.Offline); err != nil {
		return "", fmt.Errorf("Build daemon image %s: %w", daemonImage, err)
	}
	bin, err := rt.AgentBinary(ctx)
	if err != nil {
		return "", fmt.Errorf("Build daemon: %w", err)
	}
	// The Sidecar agent runs beside dockerd, which the image's entrypoint
	// starts with only the flags given.
	cmd := []string{"-c", agent.ContainerPath + ` & exec dockerd-entrypoint.sh "$@"`, "sh",
		"dockerd", "--host=unix:///var/run/docker.sock", "--host=unix://" + daemonRun + "/docker.sock"}
	var hosts []string
	for _, loc := range ar.Locations() {
		h := loc + "-docker.pkg.dev"
		cmd = append(cmd, "--insecure-registry="+h)
		hosts = append(hosts, h+":127.0.0.1")
	}
	labels := rt.Labels("build", "", "daemon")
	labels[labelRegistry] = registry
	name := rt.Name("build", "daemon")
	_ = rt.RemoveContainer(ctx, name, true)
	id, err := rt.CreateContainer(ctx, runtime.ContainerSpec{
		Name:       name,
		Image:      daemonImage,
		Entrypoint: []string{"/bin/sh"},
		Cmd:        cmd,
		Env:        []string{agent.EnvVar + "=" + daemonAgent, daemonEnv + "=" + registry, "DOCKER_TLS_CERTDIR="},
		Labels:     labels,
		Hostname:   "build-daemon",
		Privileged: !s.unprivileged,
		Networks:   []runtime.Attachment{{Network: n.name}},
		Mounts: []runtime.Mount{
			{Type: "volume", Source: daemonVolume(rt, "cache"), Target: "/var/lib/docker"},
			{Type: "volume", Source: daemonVolume(rt, "run"), Target: daemonRun},
		},
		DNS:        []string{n.agent},
		ExtraHosts: hosts,
	})
	if err != nil {
		return "", refused(err, err.Error())
	}
	fail := func(err error) (string, error) {
		_ = rt.RemoveContainer(context.WithoutCancel(ctx), id, true)
		return "", err
	}
	if err := rt.CopyTo(ctx, id, "/", []runtime.File{{Name: strings.TrimPrefix(agent.ContainerPath, "/"), Mode: 0o755, Path: bin}}); err != nil {
		return fail(err)
	}
	if err := rt.StartContainer(ctx, id); err != nil {
		return fail(refused(err, err.Error()))
	}
	if err := waitDaemon(ctx, rt, id); err != nil {
		return fail(err)
	}
	if res, err := rt.Exec(ctx, id, []string{"docker", "network", "inspect", cloudbuildNetwork}, nil); err == nil && res.ExitCode != 0 {
		res, err = rt.Exec(ctx, id, []string{"docker", "network", "create", cloudbuildNetwork}, nil)
		if err == nil && res.ExitCode != 0 {
			err = errors.New(strings.TrimSpace(string(res.Stderr)))
		}
		if err != nil {
			return fail(fmt.Errorf("creating the %s network: %w", cloudbuildNetwork, err))
		}
	}
	return id, nil
}

// waitDaemon waits until the daemon in container id serves. A daemon whose
// container exits first is refused its privileges when its log says so.
func waitDaemon(ctx context.Context, rt *runtime.Manager, id string) error {
	ctx, cancel := context.WithTimeout(ctx, daemonStart)
	defer cancel()
	for {
		if res, err := rt.Exec(ctx, id, []string{"docker", "version"}, nil); err == nil && res.ExitCode == 0 {
			return nil
		}
		c, err := rt.InspectContainer(ctx, id)
		if err != nil {
			return err
		}
		if !c.Running && c.Status != "created" {
			logs, _ := rt.Logs(context.WithoutCancel(ctx), id, 20)
			return refused(fmt.Errorf("the Build daemon exited with code %d: %s", c.ExitCode, strings.TrimSpace(logs)), logs)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the Build daemon did not start: %w", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// refused returns errPrivileged with err's detail when detail shows the
// runtime refused the daemon its privileges, and err otherwise.
func refused(err error, detail string) error {
	for _, sign := range []string{"privileged", "are you root"} {
		if strings.Contains(detail, sign) {
			return fmt.Errorf("%w: %v", errPrivileged, err)
		}
	}
	return err
}

// clearDaemon removes every container in the Build daemon, which only
// Builds' steps start, once no Build runs: Cloud Build gives each Build
// a fresh worker, so none sees an earlier one's containers. Callers hold
// mu.
func (s *Service) clearDaemon(rt *runtime.Manager) {
	s.daemonMu.Lock()
	id := s.daemonID
	s.daemonMu.Unlock()
	if id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := rt.Exec(ctx, id, []string{"sh", "-c", `ids=$(docker ps -aq) && { [ -z "$ids" ] || docker rm -fv $ids >/dev/null; }`}, nil)
	if err == nil && res.ExitCode != 0 {
		err = errors.New(strings.TrimSpace(string(res.Stderr)))
	}
	if err != nil {
		s.env.Log.Warn("build: remove the Build daemon's containers", "err", err)
	}
}

// daemonStatus is the Build daemon as GET /_emu/v1/build/daemon reports
// it, for the Web console and gcpemu doctor.
type daemonStatus struct {
	Running bool `json:"running"`
	// CacheBytes is the size of the daemon's data, its image and layer
	// cache, while it runs.
	CacheBytes int64 `json:"cacheBytes"`
	// Error is why the daemon last failed to start.
	Error             string `json:"error,omitempty"`
	PrivilegedRefused bool   `json:"privilegedRefused"`
}

// serveDaemon is GET /_emu/v1/build/daemon.
func (s *Service) serveDaemon(w http.ResponseWriter, r *http.Request) {
	s.daemonMu.Lock()
	id, err := s.daemonID, s.daemonErr
	s.daemonMu.Unlock()
	st := daemonStatus{PrivilegedRefused: errors.Is(err, errPrivileged)}
	if err != nil {
		st.Error = err.Error()
	}
	if id != "" {
		if rt, rerr := s.env.Containers.Runtime(r.Context()); rerr == nil {
			if n, err := cacheBytes(r.Context(), rt, id); err == nil {
				st.Running, st.CacheBytes = true, n
			}
		}
	}
	writeJSON(w, http.StatusOK, st)
}

// servePrune is POST /_emu/v1/build/daemon/prune: it removes the Build
// daemon's stopped containers, unused images and volumes, and build
// cache, starting the daemon if needed to reach its cache.
func (s *Service) servePrune(w http.ResponseWriter, r *http.Request) {
	rt, err := s.env.Containers.Runtime(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	n, err := s.network(r.Context(), rt)
	if err != nil {
		http.Error(w, "the Build network: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	id, err := s.buildDaemon(r.Context(), rt, n)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	before, err := cacheBytes(r.Context(), rt, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	res, err := rt.Exec(r.Context(), id, []string{"sh", "-c",
		"docker container prune -f && docker image prune -af && docker volume prune -af && docker builder prune -af"}, nil)
	if err == nil && res.ExitCode != 0 {
		err = errors.New(strings.TrimSpace(string(res.Stderr)))
	}
	if err != nil {
		http.Error(w, "pruning the Build daemon: "+err.Error(), http.StatusInternalServerError)
		return
	}
	after, err := cacheBytes(r.Context(), rt, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"reclaimedBytes": max(before-after, 0), "cacheBytes": after})
}

// cacheBytes returns the size of the Build daemon's data in container id.
func cacheBytes(ctx context.Context, rt *runtime.Manager, id string) (int64, error) {
	res, err := rt.Exec(ctx, id, []string{"du", "-sk", "/var/lib/docker"}, nil)
	if err != nil {
		return 0, err
	}
	if res.ExitCode != 0 {
		return 0, errors.New(strings.TrimSpace(string(res.Stderr)))
	}
	kib, err := strconv.ParseInt(strings.Fields(string(res.Stdout) + " x")[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("du: %q", res.Stdout)
	}
	return kib * 1024, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
