package edgeagent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linuxuser586/gcpemu/internal/agent"
	"github.com/linuxuser586/gcpemu/internal/lanbind"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// LANRelay manages a LAN relay container: on the host network, with
// NET_BIND_SERVICE, it runs the agent in LAN mode, listens on host
// addresses on ports the emulator cannot bind (privileged ports) and
// relays each connection, with a PROXY v2 header, to an ingress on the
// host's loopback. It forwards plain TCP. The lb Service's lb-lan
// (--lb-bind) and the Google front end's gfe-lan (--gfe-bind) are two.
type LANRelay struct {
	// Runtime returns the container runtime.
	Runtime func(context.Context) (*runtime.Manager, error)
	// Name names the container (gcpemu-<id>-<Name>) and its log lines.
	Name string
	// Service, Resource and Role label the container.
	Service, Resource, Role string
	// Offline: the image is not pulled.
	Offline bool
	Log     *slog.Logger

	mu      sync.Mutex
	created bool // a container may exist under its name without id
	id      string
	ctrl    string // control address on the host's loopback
	token   string
	pushed  string
}

// Set makes the relay listen on exactly addrs (host:port) and relay to
// ingress, starting the container on demand and removing it when addrs is
// empty.
func (r *LANRelay) Set(ctx context.Context, ingress string, addrs []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(addrs) == 0 {
		r.removeLocked(ctx)
		return nil
	}
	if err := r.ensureLocked(ctx); err != nil {
		return err
	}
	cfg := Config{Ingress: ingress}
	addrs = append([]string(nil), addrs...)
	sort.Strings(addrs)
	for _, a := range addrs {
		h, p, _ := net.SplitHostPort(a)
		port, _ := strconv.Atoi(p)
		cfg.Listeners = append(cfg.Listeners, Listener{IP: h, Port: port})
	}
	body, _ := json.Marshal(cfg)
	if string(body) == r.pushed {
		return nil
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+r.ctrl+"/config", bytes.NewReader(body))
	req.Header.Set(TokenHeader, r.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s config: %s", r.Name, strings.TrimSpace(string(msg)))
	}
	r.pushed = string(body)
	return nil
}

// ensureLocked starts the container if it is not running.
func (r *LANRelay) ensureLocked(ctx context.Context) error {
	rt, err := r.Runtime(ctx)
	if err != nil {
		return err
	}
	if r.id != "" {
		if c, err := rt.InspectContainer(ctx, r.id); err == nil && c.Running {
			return nil
		}
		r.id, r.pushed = "", ""
	}
	if rt.Self() != "" {
		return errors.New("the Instance runs in a container on a bridge network, whose addresses reach only container networks; " + lanbind.Fix)
	}
	if err := rt.EnsureImage(ctx, Image, r.Offline); err != nil {
		return fmt.Errorf("%s image %s: %w", r.Name, Image, err)
	}
	bin, err := rt.AgentBinary(ctx)
	if err != nil {
		return err
	}
	var tb [16]byte
	_, _ = rand.Read(tb[:])
	r.token = hex.EncodeToString(tb[:])
	name := rt.Name(r.Name)
	// A start cut short (ctx done) cleans up regardless.
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_ = rt.RemoveContainer(ctx, name, true)
	r.created = true
	id, err := rt.CreateContainer(ctx, runtime.ContainerSpec{
		Name:       name,
		Image:      Image,
		Entrypoint: []string{agent.ContainerPath},
		Cmd:        []string{},
		Env: []string{
			agent.EnvVar + "=" + LANAgentName,
			"GCPEMU_LB_TOKEN=" + r.token,
		},
		Labels:   rt.Labels(r.Service, r.Resource, r.Role),
		CapAdd:   []string{"NET_BIND_SERVICE"},
		Networks: []runtime.Attachment{{Network: "host"}},
	})
	if err != nil {
		_ = rt.RemoveContainer(cleanup, name, true)
		return fmt.Errorf("%s: %w", r.Name, err)
	}
	if err := rt.CopyTo(ctx, id, "/", []runtime.File{{Name: strings.TrimPrefix(agent.ContainerPath, "/"), Mode: 0o755, Path: bin}}); err != nil {
		_ = rt.RemoveContainer(cleanup, id, true)
		return fmt.Errorf("%s: copy Sidecar agent: %w", r.Name, err)
	}
	if err := rt.StartContainer(ctx, id); err != nil {
		_ = rt.RemoveContainer(cleanup, id, true)
		return fmt.Errorf("%s: start: %w", r.Name, err)
	}
	ctrl, err := r.awaitControl(ctx, rt, id)
	if err != nil {
		logs, _ := rt.Logs(cleanup, id, 20)
		_ = rt.RemoveContainer(cleanup, id, true)
		return fmt.Errorf("%s: %w\n%s", r.Name, err, strings.TrimSpace(logs))
	}
	r.ctrl = ctrl
	r.id = id
	r.Log.Info(r.Name+" relay container started", "container", name)
	return nil
}

// awaitControl waits for the agent in container id to report the control
// address it listens on, on the host's loopback (writeControl).
func (r *LANRelay) awaitControl(ctx context.Context, rt *runtime.Manager, id string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		logs, err := rt.Logs(ctx, id, 100)
		if addr, ok := readControl(logs); ok {
			return addr, nil
		}
		if err == nil {
			if c, ierr := rt.InspectContainer(ctx, id); ierr == nil && (c.Status == "exited" || c.Status == "dead") {
				return "", fmt.Errorf("container exited with code %d before reporting its control port", c.ExitCode)
			}
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("control port not reported: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (r *LANRelay) removeLocked(ctx context.Context) {
	if !r.created {
		return
	}
	if rt, err := r.Runtime(ctx); err == nil {
		target := r.id
		if target == "" {
			target = rt.Name(r.Name)
		}
		_ = rt.RemoveContainer(ctx, target, true)
	}
	if r.id != "" {
		r.Log.Info(r.Name + " relay container removed")
	}
	r.created, r.id, r.pushed = false, "", ""
}

// Close removes the container.
func (r *LANRelay) Close(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removeLocked(ctx)
}

// runLANAgent is a LANRelay container's agent: edgeAgent listeners on host
// addresses, configured on a control port it reports on stdout.
func runLANAgent(ctx context.Context, _ []string) error {
	return serveLAN(ctx, os.Stdout, os.Getenv("GCPEMU_LB_TOKEN"))
}

// serveLAN listens for control on a free port on the host's loopback,
// writes its address to out (writeControl) and serves until ctx ends.
func serveLAN(ctx context.Context, out io.Writer, token string) error {
	a := &edgeAgent{token: token, lan: true, aliases: map[string]bool{},
		listeners: map[string]net.Listener{}, routes: map[string]string{}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	writeControl(out, ln.Addr().String())
	mux := http.NewServeMux()
	mux.HandleFunc("POST /config", a.handleConfig)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); srv.Close() }()
	if err := srv.Serve(ln); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// controlPrefix starts the line a LAN agent writes its control address on.
const controlPrefix = "gcpemu agent lan: control "

// writeControl writes the line that tells the emulator addr, the control
// address the LAN agent listens on.
func writeControl(w io.Writer, addr string) {
	fmt.Fprintf(w, "%s%s\n", controlPrefix, addr)
}

// readControl finds the control address writeControl wrote in logs.
func readControl(logs string) (string, bool) {
	for line := range strings.Lines(logs) {
		if addr, ok := strings.CutPrefix(strings.TrimSpace(line), controlPrefix); ok && addr != "" {
			return addr, true
		}
	}
	return "", false
}
