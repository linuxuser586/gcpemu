package lb

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linuxuser586/gcpemu/internal/agent"
)

// The "lb-edge" agent runs in the lb-edge container (edge.go). It
//
//   - owns one address per forwarding rule on the lb network and listens on
//     the rule's real port (80/443), relaying each connection to the
//     emulator's edge ingress with a PROXY v2 header (FR-LB-002);
//   - relays backend connections to addresses only reachable from container
//     networks (GKE pod IPs, via `ip route` through the node; FR-INT-001):
//     the emulator sends "TOKEN host:port\n" and gets "OK\n" before the
//     stream is spliced.
//
// The emulator configures it over a token-protected HTTP control port.

const (
	edgeAgentName   = "lb-edge"
	edgeControlPort = 15080
	edgeRelayPort   = 15081
	edgeTokenHeader = "X-Gcpemu-Lb-Token"
)

func init() { agent.Register(edgeAgentName, runEdgeAgent) }

// edgeConfig is the desired state the emulator posts to the agent.
type edgeConfig struct {
	Ingress   string         `json:"ingress"`
	Listeners []edgeListener `json:"listeners"`
	Routes    []edgeRoute    `json:"routes"`
}

type edgeListener struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

type edgeRoute struct {
	CIDR string `json:"cidr"`
	Via  string `json:"via"`
}

type edgeAgent struct {
	token  string
	subnet *net.IPNet

	mu        sync.Mutex
	ingress   string
	aliases   map[string]bool
	listeners map[string]net.Listener
	routes    map[string]string
}

func runEdgeAgent(ctx context.Context, _ []string) error {
	a := &edgeAgent{token: os.Getenv("GCPEMU_LB_TOKEN"), aliases: map[string]bool{},
		listeners: map[string]net.Listener{}, routes: map[string]string{}}
	if _, n, err := net.ParseCIDR(os.Getenv("GCPEMU_LB_SUBNET")); err == nil {
		a.subnet = n
	}
	relay, err := net.Listen("tcp", ":"+strconv.Itoa(edgeRelayPort))
	if err != nil {
		return err
	}
	go a.serveRelay(relay)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /config", a.handleConfig)
	srv := &http.Server{Addr: ":" + strconv.Itoa(edgeControlPort), Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); srv.Close(); relay.Close() }()
	if err := srv.ListenAndServe(); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

func (a *edgeAgent) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(edgeTokenHeader) != a.token {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var cfg edgeConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := a.apply(cfg); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func ipCmd(args ...string) error {
	out, err := exec.Command("ip", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// lbInterface finds the interface on the lb network.
func (a *edgeAgent) lbInterface() (string, int) {
	ones := 32
	if a.subnet != nil {
		ones, _ = a.subnet.Mask.Size()
	}
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		addrs, _ := ifc.Addrs()
		for _, ad := range addrs {
			if ipn, ok := ad.(*net.IPNet); ok && a.subnet != nil && a.subnet.Contains(ipn.IP) {
				return ifc.Name, ones
			}
		}
	}
	return "eth0", ones
}

func (a *edgeAgent) apply(cfg edgeConfig) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ingress = cfg.Ingress
	var errs []string
	iface, ones := a.lbInterface()
	want := map[string]bool{}
	wantL := map[string]edgeListener{}
	for _, l := range cfg.Listeners {
		want[l.IP] = true
		wantL[net.JoinHostPort(l.IP, strconv.Itoa(l.Port))] = l
	}
	for ip := range want {
		if !a.aliases[ip] {
			if err := ipCmd("addr", "add", ip+"/"+strconv.Itoa(ones), "dev", iface); err != nil && !strings.Contains(err.Error(), "File exists") {
				errs = append(errs, err.Error())
				continue
			}
			a.aliases[ip] = true
		}
	}
	for key, ln := range a.listeners {
		if _, ok := wantL[key]; !ok {
			ln.Close()
			delete(a.listeners, key)
		}
	}
	for ip := range a.aliases {
		if !want[ip] {
			_ = ipCmd("addr", "del", ip+"/"+strconv.Itoa(ones), "dev", iface)
			delete(a.aliases, ip)
		}
	}
	for key := range wantL {
		if _, ok := a.listeners[key]; ok {
			continue
		}
		ln, err := net.Listen("tcp", key)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		a.listeners[key] = ln
		go a.serveListener(ln)
	}
	wantR := map[string]string{}
	for _, rt := range cfg.Routes {
		wantR[rt.CIDR] = rt.Via
	}
	for cidr, via := range wantR {
		if a.routes[cidr] == via {
			continue
		}
		if err := ipCmd("route", "replace", cidr, "via", via); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		a.routes[cidr] = via
	}
	for cidr := range a.routes {
		if _, ok := wantR[cidr]; !ok {
			_ = ipCmd("route", "del", cidr)
			delete(a.routes, cidr)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func (a *edgeAgent) serveListener(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			a.mu.Lock()
			ingress := a.ingress
			a.mu.Unlock()
			u, err := net.DialTimeout("tcp", ingress, 5*time.Second)
			if err != nil {
				return
			}
			defer u.Close()
			src, _ := c.RemoteAddr().(*net.TCPAddr)
			dst, _ := c.LocalAddr().(*net.TCPAddr)
			if src == nil || dst == nil || writeProxyV2(u, src, dst) != nil {
				return
			}
			splice(c, u)
		}()
	}
}

func (a *edgeAgent) serveRelay(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
			br := bufio.NewReader(c)
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			_ = c.SetReadDeadline(time.Time{})
			tok, target, _ := strings.Cut(strings.TrimSpace(line), " ")
			if tok != a.token {
				io.WriteString(c, "ERR forbidden\n")
				return
			}
			u, err := net.DialTimeout("tcp", target, 10*time.Second)
			if err != nil {
				io.WriteString(c, "ERR "+err.Error()+"\n")
				return
			}
			defer u.Close()
			if _, err := io.WriteString(c, "OK\n"); err != nil {
				return
			}
			if n := br.Buffered(); n > 0 {
				b, _ := br.Peek(n)
				if _, err := u.Write(b); err != nil {
					return
				}
			}
			splice(c, u)
		}()
	}
}

// splice copies both directions until both are done.
func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
}
