package instance

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// Managed emulators (ADR 0029): the External emulators that --external
// API=managed has the Instance run itself, one Managed container per
// Delegated API, from Google's own emulator images. Each starts with the
// Instance, which is not ready until it accepts connections, and is
// removed when the Instance stops. It sits on the services network, so
// clients reach it only through the API gateway. Its data is in memory
// only: Reset replaces its container, and no State snapshot holds it.

const (
	// cliEmulatorsImage is Google's Cloud CLI with its emulators, over 1 GB.
	cliEmulatorsImage = "gcr.io/google.com/cloudsdktool/google-cloud-cli:590.0.1-emulators"
	spannerImage      = "gcr.io/cloud-spanner-emulator/emulator:1.5.59"
	// managedStart bounds how long a started Managed emulator may take to
	// accept connections; the image pull is not counted.
	managedStart = 3 * time.Minute
)

// managedSpec is how one API's Managed emulator runs.
type managedSpec struct {
	image string
	// cmd is the container's command; nil keeps the image's.
	cmd []string
	// grpcPort and restPort are where it serves gRPC and REST.
	grpcPort, restPort int
}

// managedSpecs has an entry for each of config.ManagedAPIs. Datastore runs
// the Firestore emulator in Datastore mode, as Google deprecated the
// Datastore emulator.
var managedSpecs = map[string]managedSpec{
	"firestore": {image: cliEmulatorsImage, grpcPort: 8080, restPort: 8080,
		cmd: []string{"gcloud", "emulators", "firestore", "start", "--host-port=0.0.0.0:8080"}},
	"datastore": {image: cliEmulatorsImage, grpcPort: 8080, restPort: 8080,
		cmd: []string{"gcloud", "emulators", "firestore", "start", "--host-port=0.0.0.0:8080", "--database-mode=datastore-mode"}},
	"bigtable": {image: cliEmulatorsImage, grpcPort: 8086, restPort: 8086,
		cmd: []string{"gcloud", "beta", "emulators", "bigtable", "start", "--host-port=0.0.0.0:8086"}},
	// The image's gateway_main serves gRPC on 9010 and REST on 9020.
	"spanner": {image: spannerImage, grpcPort: 9010, restPort: 9020},
}

// managedEmulator is one Delegated API's Managed emulator.
type managedEmulator struct {
	api  string
	spec managedSpec
	c    *containers

	// runMu serialises starts: the Instance's, and Reset's.
	runMu sync.Mutex

	mu sync.Mutex
	// ip is its address on the services network once it accepts
	// connections, else "".
	ip string
	// why says why it is not running while ip is "".
	why error
	// noRuntime is whether why is that there is no usable container
	// runtime, which the Instance's readiness does not wait on.
	noRuntime bool
	// conns are the connections dialed to it, closed when it is replaced.
	conns map[net.Conn]struct{}
}

func newManagedEmulator(api string, c *containers) *managedEmulator {
	return &managedEmulator{api: api, spec: managedSpecs[api], c: c, why: errors.New("it is starting"), conns: map[net.Conn]struct{}{}}
}

// start (re)creates its container, replacing any it has, and waits until
// it accepts connections. Its error is also what readiness and dialers get.
func (m *managedEmulator) start(ctx context.Context) error {
	m.runMu.Lock()
	defer m.runMu.Unlock()
	m.mu.Lock()
	m.ip, m.why, m.noRuntime = "", errors.New("it is starting"), false
	conns := m.conns
	m.conns = map[net.Conn]struct{}{}
	m.mu.Unlock()
	for c := range conns {
		c.Close()
	}
	ip, err := m.run(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ip, m.why = ip, err
	if errors.Is(err, emu.ErrNoRuntime) {
		m.noRuntime = true
		return nil
	}
	return err
}

// status records why it is not running yet.
func (m *managedEmulator) status(format string, args ...any) {
	m.mu.Lock()
	m.why = fmt.Errorf(format, args...)
	m.mu.Unlock()
}

// run starts its container on the services network and returns its
// address there once it accepts connections.
func (m *managedEmulator) run(ctx context.Context) (string, error) {
	rt, err := m.c.Runtime(ctx)
	for err != nil && !errors.Is(err, emu.ErrNoRuntime) { // transient
		m.status("it is waiting for the container runtime: %v", err)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
		rt, err = m.c.Runtime(ctx)
	}
	if err != nil {
		return "", fmt.Errorf("it needs a container runtime, and none is usable: %w", err)
	}
	plane, err := m.c.Netplane(ctx)
	if err != nil {
		return "", err
	}
	svc, err := plane.Services(ctx)
	if err != nil {
		return "", err
	}
	m.status("it is starting: pulling %s", m.spec.image)
	if err := rt.EnsureImage(ctx, m.spec.image, rt.Offline); err != nil {
		return "", fmt.Errorf("image %s: %w", m.spec.image, err)
	}
	m.status("it is starting")
	name := rt.Name("external", m.api)
	_ = rt.RemoveContainer(ctx, name, true)
	id, err := rt.CreateContainer(ctx, runtime.ContainerSpec{
		Name:     name,
		Image:    m.spec.image,
		Cmd:      m.spec.cmd,
		Labels:   rt.Labels("external", m.api, "emulator"),
		Hostname: m.api + "-emulator",
		Networks: []runtime.Attachment{{Network: svc.Name}},
		Init:     true,
	})
	if err != nil {
		return "", err
	}
	ip, err := m.wait(ctx, rt, id, svc.Name)
	if err != nil {
		_ = rt.RemoveContainer(context.WithoutCancel(ctx), id, true)
		return "", err
	}
	return ip, nil
}

// wait starts container id and waits until it accepts connections on its
// ports, returning its address on network.
func (m *managedEmulator) wait(ctx context.Context, rt *runtime.Manager, id, network string) (string, error) {
	if err := rt.StartContainer(ctx, id); err != nil {
		return "", err
	}
	ct, err := rt.InspectContainer(ctx, id)
	if err != nil {
		return "", err
	}
	ip := ct.IPs[network]
	if ip == "" {
		return "", fmt.Errorf("container %s has no address on %s", ct.Name, network)
	}
	ctx, cancel := context.WithTimeout(ctx, managedStart)
	defer cancel()
	for _, port := range []int{m.spec.grpcPort, m.spec.restPort} {
		for {
			dctx, dcancel := context.WithTimeout(ctx, 2*time.Second)
			c, err := m.c.DialContext(dctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
			dcancel()
			if err == nil {
				c.Close()
				break
			}
			if ct, ierr := rt.InspectContainer(ctx, id); ierr == nil && !ct.Running {
				logs, _ := rt.Logs(context.WithoutCancel(ctx), id, 20)
				return "", fmt.Errorf("it exited with code %d: %s", ct.ExitCode, strings.TrimSpace(logs))
			}
			select {
			case <-ctx.Done():
				return "", fmt.Errorf("it did not accept connections on port %d: %w", port, ctx.Err())
			case <-time.After(250 * time.Millisecond):
			}
		}
	}
	return ip, nil
}

// addr returns its gRPC address, or "" and why it is not running.
func (m *managedEmulator) addr() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ip == "" {
		return "", m.why
	}
	return net.JoinHostPort(m.ip, strconv.Itoa(m.spec.grpcPort)), nil
}

// readiness is nil once it accepts connections, or when there is no usable
// container runtime, as Runtime Services are skipped then; else why not.
func (m *managedEmulator) readiness() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ip != "" || m.noRuntime {
		return nil
	}
	return fmt.Errorf("the Managed emulator for %s is not ready: %w", m.api, m.why)
}

// dial is its gateway.ManagedDialer.
func (m *managedEmulator) dial(ctx context.Context, grpc bool) (net.Conn, error) {
	m.mu.Lock()
	ip, why := m.ip, m.why
	m.mu.Unlock()
	if ip == "" {
		return nil, why
	}
	port := m.spec.restPort
	if grpc {
		port = m.spec.grpcPort
	}
	c, err := m.c.DialContext(ctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ip != ip { // replaced meanwhile
		c.Close()
		return nil, errors.New("it is starting")
	}
	t := &trackedConn{Conn: c, m: m}
	m.conns[t] = struct{}{}
	return t, nil
}

// trackedConn is a connection to a Managed emulator, which forgets it
// once closed.
type trackedConn struct {
	net.Conn
	m    *managedEmulator
	once sync.Once
}

func (t *trackedConn) Close() error {
	t.once.Do(func() {
		t.m.mu.Lock()
		delete(t.m.conns, t)
		t.m.mu.Unlock()
	})
	return t.Conn.Close()
}

// startManaged starts every Managed emulator in the background; Shutdown
// stops them.
func (in *Instance) startManaged() {
	for _, m := range in.managed {
		in.managedWG.Go(func() {
			if err := m.start(in.managedCtx); err != nil && in.managedCtx.Err() == nil {
				in.Env.Log.Error("Managed emulator failed to start", "api", m.api, "err", err)
			}
		})
	}
}

// resetManaged replaces every Managed emulator's container, clearing its
// data (Reset).
func (in *Instance) resetManaged() error {
	errs := make([]error, 0, len(in.managed))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, m := range in.managed {
		wg.Go(func() {
			if err := m.start(in.managedCtx); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("the Managed emulator for %s: %w", m.api, err))
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// stopManaged stops starting Managed emulators; their containers go with
// the Instance's others.
func (in *Instance) stopManaged() {
	in.managedCancel()
	in.managedWG.Wait()
}
