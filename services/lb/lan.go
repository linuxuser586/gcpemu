package lb

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/linuxuser586/gcpemu/internal/agent"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// --lb-bind (Config.LBBind): every forwarding rule listener also serves on
// <address>:<the rule's real port> for each configured address, so that
// another device (a phone on the LAN) can reach the load balancer while
// the admin API, the gateway and every other data plane stay on --bind.
//
// The emulator listens there itself when it may bind the port. When the
// port is privileged (ip_unprivileged_port_start above it), the lb-lan
// container, on the host network, listens instead and relays each
// connection to the edge ingress with a PROXY v2 header, exactly like
// lb-edge (edgeagent.go runs in both).
//
// Two forwarding rules on one port cannot share an address: the one with
// the lowest "ip:port" key gets it. Each frontend's addresses are recorded
// as endpoint "lb-lan:<name>", which `gcpemu status` and `doctor` print.

const lanAgentName = "lb-lan"

func init() { agent.Register(lanAgentName, runLANAgent) }

// lanBinding is one claimed --lb-bind address:port.
type lanBinding struct {
	key   string    // frontend key
	l     *listener // the frontend listener it serves
	ln    net.Listener
	relay bool // served by the lb-lan container
}

// syncLAN binds the --lb-bind addresses of every listener. Callers hold d.mu.
func (d *dataplane) syncLAN() {
	binds := d.s.env.Config.LBBind
	if len(binds) == 0 {
		return
	}
	if d.lan == nil {
		d.lan = map[string]*lanBinding{}
		d.lanErr = map[string]string{}
	}
	for addr, b := range d.lan {
		if d.listeners[b.key] != b.l {
			if b.ln != nil {
				b.ln.Close()
			}
			delete(d.lan, addr)
		}
	}
	keys := make([]string, 0, len(d.listeners))
	for k := range d.listeners {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var fresh []string
	for _, key := range keys {
		l := d.listeners[key]
		_, port, _ := net.SplitHostPort(key)
		for _, ip := range binds {
			addr := net.JoinHostPort(ip, port)
			if b := d.lan[addr]; b != nil {
				if b.key != key {
					d.lanFailed(addr, key, fmt.Errorf("already serves forwarding rule %s", b.l.name))
				}
				continue
			}
			b, err := d.bindLAN(l, addr)
			if err != nil {
				d.lanFailed(addr, key, err)
				continue
			}
			d.lan[addr] = b
			fresh = append(fresh, addr)
		}
	}
	var relayed []string
	for addr, b := range d.lan {
		if b.relay {
			relayed = append(relayed, addr)
		}
	}
	if err := d.relay.set(d.ctx, relayed); err != nil {
		// Not exposed after all: retried on the next change.
		for _, addr := range relayed {
			d.lanFailed(addr, d.lan[addr].key, fmt.Errorf("lb-lan relay: %w", err))
			delete(d.lan, addr)
		}
	}
	for _, addr := range fresh {
		if b := d.lan[addr]; b != nil {
			delete(d.lanErr, addr+" "+b.key)
			mode := "direct"
			if b.relay {
				mode = "relay"
			}
			d.s.env.Log.Info("lb: forwarding rule exposed", "forwardingRule", b.l.name, "address", addr, "mode", mode)
		}
	}
	byName := map[string][]string{}
	for addr, b := range d.lan {
		byName[b.l.name] = append(byName[b.l.name], addr)
	}
	for _, n := range d.s.env.Endpoints.Names() {
		if name, ok := strings.CutPrefix(n, "lb-lan:"); ok && byName[name] == nil {
			d.s.env.Endpoints.Delete(n)
		}
	}
	for name, addrs := range byName {
		sort.Strings(addrs)
		d.s.env.Endpoints.Set("lb-lan:"+name, strings.Join(addrs, ","))
	}
}

// bindLAN serves l on addr, directly or through the lb-lan relay.
func (d *dataplane) bindLAN(l *listener, addr string) (*lanBinding, error) {
	b := &lanBinding{key: l.key, l: l}
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		b.ln = ln
		go l.serve(ln)
		return b, nil
	}
	if !errors.Is(err, syscall.EACCES) {
		return nil, err
	}
	if !d.edge.available(d.ctx) {
		return nil, fmt.Errorf("%w (privileged port and no container runtime to relay it; lower net.ipv4.ip_unprivileged_port_start)", err)
	}
	if l.virt == nil {
		l.virt = newChanListener(l.addr)
		go l.serve(l.virt)
	}
	b.relay = true
	return b, nil
}

// lanFailed logs a --lb-bind failure once per address and frontend.
func (d *dataplane) lanFailed(addr, key string, err error) {
	k := addr + " " + key
	if d.lanErr[k] == err.Error() {
		return
	}
	d.lanErr[k] = err.Error()
	name := key
	if l := d.listeners[key]; l != nil {
		name = l.name
	}
	d.s.env.Log.Warn("lb: cannot expose forwarding rule on --lb-bind address", "forwardingRule", name, "address", addr, "err", err)
}

// closeLAN closes every --lb-bind listener and the relay. Callers hold d.mu.
func (d *dataplane) closeLAN(ctx context.Context) {
	for addr, b := range d.lan {
		if b.ln != nil {
			b.ln.Close()
		}
		delete(d.lan, addr)
	}
	d.relay.close(ctx)
}

// lanRelay manages the lb-lan container: on the host network, it listens
// on --lb-bind addresses for privileged ports and relays to the ingress.
type lanRelay struct {
	d *dataplane

	mu     sync.Mutex
	id     string
	ctrl   string // control address on the host's loopback
	token  string
	pushed string
}

// set makes the relay listen on exactly addrs, starting the container on
// demand and removing it when addrs is empty.
func (r *lanRelay) set(ctx context.Context, addrs []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(addrs) == 0 {
		r.removeLocked(ctx)
		return nil
	}
	ingress, err := r.d.edge.ingressAddr()
	if err != nil {
		return err
	}
	if err := r.ensureLocked(ctx); err != nil {
		return err
	}
	cfg := edgeConfig{Ingress: ingress}
	sort.Strings(addrs)
	for _, a := range addrs {
		h, p, _ := net.SplitHostPort(a)
		port, _ := strconv.Atoi(p)
		cfg.Listeners = append(cfg.Listeners, edgeListener{IP: h, Port: port})
	}
	body, _ := json.Marshal(cfg)
	if string(body) == r.pushed {
		return nil
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+r.ctrl+"/config", bytes.NewReader(body))
	req.Header.Set(edgeTokenHeader, r.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("lb-lan config: %s", strings.TrimSpace(string(msg)))
	}
	r.pushed = string(body)
	return nil
}

// ensureLocked starts the lb-lan container if it is not running.
func (r *lanRelay) ensureLocked(ctx context.Context) error {
	env := r.d.s.env
	rt, err := env.Containers.Runtime(ctx)
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
		return errors.New("the emulator runs in a container: publish the port on its container instead")
	}
	if err := rt.EnsureImage(ctx, edgeImage, env.Config.Offline); err != nil {
		return fmt.Errorf("lb-lan image %s: %w", edgeImage, err)
	}
	bin, err := agent.Binary()
	if err != nil {
		return err
	}
	// The relay shares the host's loopback: pick a free control port there.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	r.ctrl = ln.Addr().String()
	ln.Close()
	var tb [16]byte
	_, _ = rand.Read(tb[:])
	r.token = hex.EncodeToString(tb[:])
	name := rt.Name("lb-lan")
	_ = rt.RemoveContainer(ctx, name, true)
	id, err := rt.CreateContainer(ctx, runtime.ContainerSpec{
		Name:       name,
		Image:      edgeImage,
		Entrypoint: []string{agent.ContainerPath},
		Cmd:        []string{},
		Env: []string{
			agent.EnvVar + "=" + lanAgentName,
			"GCPEMU_LB_TOKEN=" + r.token,
			"GCPEMU_LB_CONTROL=" + r.ctrl,
		},
		Labels:   rt.Labels("lb", "lan", "lan"),
		CapAdd:   []string{"NET_BIND_SERVICE"},
		Networks: []runtime.Attachment{{Network: "host"}},
	})
	if err != nil {
		return fmt.Errorf("lb-lan: %w", err)
	}
	if err := rt.CopyTo(ctx, id, "/", []runtime.File{{Name: strings.TrimPrefix(agent.ContainerPath, "/"), Mode: 0o755, Path: bin}}); err != nil {
		_ = rt.RemoveContainer(ctx, id, true)
		return fmt.Errorf("lb-lan: copy agent: %w", err)
	}
	if err := rt.StartContainer(ctx, id); err != nil {
		_ = rt.RemoveContainer(ctx, id, true)
		return fmt.Errorf("lb-lan: start: %w", err)
	}
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		req, _ := http.NewRequestWithContext(wctx, http.MethodGet, "http://"+r.ctrl+"/healthz", nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			break
		}
		select {
		case <-wctx.Done():
			_ = rt.RemoveContainer(ctx, id, true)
			return fmt.Errorf("lb-lan: control port not ready: %w", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	r.id = id
	env.Log.Info("lb: lb-lan relay container started", "container", name)
	return nil
}

func (r *lanRelay) removeLocked(ctx context.Context) {
	if r.id == "" {
		return
	}
	if rt, err := r.d.s.env.Containers.Runtime(ctx); err == nil {
		_ = rt.RemoveContainer(ctx, r.id, true)
	}
	r.id, r.pushed = "", ""
	r.d.s.env.Log.Info("lb: lb-lan relay container removed")
}

func (r *lanRelay) close(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removeLocked(ctx)
}

// runLANAgent is the lb-lan container's agent: edgeAgent listeners on host
// addresses, configured on GCPEMU_LB_CONTROL.
func runLANAgent(ctx context.Context, _ []string) error {
	a := &edgeAgent{token: os.Getenv("GCPEMU_LB_TOKEN"), lan: true, aliases: map[string]bool{},
		listeners: map[string]net.Listener{}, routes: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /config", a.handleConfig)
	srv := &http.Server{Addr: os.Getenv("GCPEMU_LB_CONTROL"), Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); srv.Close() }()
	if err := srv.ListenAndServe(); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}
