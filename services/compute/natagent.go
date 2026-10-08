package compute

import (
	"bufio"
	"bytes"
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
	"time"

	"github.com/linuxuser586/gcpemu/internal/agent"
)

// The "nat-gateway" agent runs inside the NAT egress gateway container. It
// keeps the container alive, serves the --offline egress sink (connections
// REDIRECTed by iptables; the original destination comes from
// SO_ORIGINAL_DST), streams conntrack NEW events and reads the NFLOG copies
// of dropped connections (natflog.go), reporting all three to the emulator
// (see natevents.go).

func init() { agent.Register(natAgentName, runNatAgent) }

func runNatAgent(ctx context.Context, _ []string) error {
	r := &natReporter{
		url:     os.Getenv("GCPEMU_NAT_REPORT"),
		token:   os.Getenv("GCPEMU_NAT_TOKEN"),
		network: os.Getenv("GCPEMU_NAT_NETWORK"),
		ch:      make(chan natEvent, 1024),
	}
	go r.run(ctx)
	port, _ := strconv.Atoi(os.Getenv("GCPEMU_NAT_SINK_PORT"))
	if port == 0 {
		port = natSinkPort
	}
	status, body := parseSinkResponse(os.Getenv("GCPEMU_NAT_SINK_RESPONSE"))
	l, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); l.Close() }()
	go watchConntrack(ctx, r)
	go watchDrops(ctx, r)
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		go serveSink(c, r, status, body)
	}
}

// parseSinkResponse parses natSinkResponse ("STATUS" or "STATUS:BODY").
func parseSinkResponse(v string) (int, string) {
	status, body := 503, "gcpemu offline: egress blocked"
	if v == "" {
		return status, body
	}
	code, rest, hasBody := strings.Cut(v, ":")
	if n, err := strconv.Atoi(strings.TrimSpace(code)); err == nil && n >= 100 && n <= 599 {
		status = n
		if hasBody {
			body = rest
		} else {
			body = http.StatusText(n)
		}
	}
	return status, body
}

// serveSink records one redirected connection and answers HTTP requests.
func serveSink(c net.Conn, r *natReporter, status int, body string) {
	defer c.Close()
	ev := natEvent{Type: "sink", Time: time.Now().UTC().Format(time.RFC3339Nano), Proto: "tcp"}
	if a, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		ev.Src, ev.SrcPort = a.IP.String(), a.Port
	}
	if ip, port, err := originalDst(c); err == nil {
		ev.Dst, ev.DstPort = ip, port
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4096)
	n, _ := io.ReadAtLeast(c, buf, 1)
	req := buf[:n]
	line, _, _ := bytes.Cut(req, []byte("\r\n"))
	ev.Request = string(line)
	r.send(ev)
	if isHTTPRequest(line) {
		fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nX-Gcpemu-Nat-Sink: true\r\nConnection: close\r\n\r\n%s",
			status, http.StatusText(status), len(body), body)
	}
}

func isHTTPRequest(line []byte) bool {
	for _, m := range []string{"GET ", "POST ", "PUT ", "HEAD ", "DELETE ", "PATCH ", "OPTIONS ", "CONNECT "} {
		if bytes.HasPrefix(line, []byte(m)) {
			return true
		}
	}
	return false
}

// watchConntrack streams new connections ("conntrack -E -e NEW").
func watchConntrack(ctx context.Context, r *natReporter) {
	for ctx.Err() == nil {
		cmd := exec.CommandContext(ctx, "conntrack", "-E", "-e", "NEW")
		out, err := cmd.StdoutPipe()
		if err == nil {
			err = cmd.Start()
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "nat-gateway: conntrack: %v\n", err)
			return
		}
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if ev, ok := parseConntrack(sc.Text()); ok {
				r.send(ev)
			}
		}
		_ = cmd.Wait()
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

// natDropDedup suppresses repeats of one dropped flow (SYN retransmits).
const natDropDedup = 30 * time.Second

// watchDrops reports connections the gateway drops or rejects, once per
// flow within natDropDedup.
func watchDrops(ctx context.Context, r *natReporter) {
	seen := map[string]time.Time{}
	err := readNflog(ctx, natDropGroup, func(b []byte) {
		now := time.Now()
		for _, ev := range parseNflog(b) {
			k := fmt.Sprintf("%s %s:%d %s:%d", ev.Proto, ev.Src, ev.SrcPort, ev.Dst, ev.DstPort)
			if t, ok := seen[k]; ok && now.Sub(t) < natDropDedup {
				continue
			}
			seen[k] = now
			r.send(ev)
		}
		if len(seen) > 4096 {
			for k, t := range seen {
				if now.Sub(t) >= natDropDedup {
					delete(seen, k)
				}
			}
		}
	})
	if err != nil && ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "nat-gateway: nflog: %v (dropped connections are not logged)\n", err)
	}
}

// parseConntrack parses a conntrack event line into a translation event:
// one whose reply tuple's destination differs from the original source.
func parseConntrack(line string) (natEvent, bool) {
	f := strings.Fields(line)
	if len(f) < 3 {
		return natEvent{}, false
	}
	i := 0
	if strings.HasPrefix(f[0], "[") {
		i = 1
	}
	ev := natEvent{Type: "translation", Time: time.Now().UTC().Format(time.RFC3339Nano), Proto: f[i]}
	var orig, reply = map[string]string{}, map[string]string{}
	cur, inReply := orig, false
	for _, kv := range f[i+1:] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if _, dup := cur[k]; dup && !inReply {
			cur, inReply = reply, true
		}
		cur[k] = v
	}
	if orig["src"] == "" || reply["dst"] == "" || reply["dst"] == orig["src"] {
		return natEvent{}, false
	}
	ev.Src, ev.Dst, ev.NatIP = orig["src"], orig["dst"], reply["dst"]
	ev.SrcPort, _ = strconv.Atoi(orig["sport"])
	ev.DstPort, _ = strconv.Atoi(orig["dport"])
	ev.NatPort, _ = strconv.Atoi(reply["dport"])
	return ev, true
}

// natReporter batches events to the emulator.
type natReporter struct {
	url, token, network string
	ch                  chan natEvent
}

func (r *natReporter) send(ev natEvent) {
	select {
	case r.ch <- ev:
	default: // drop under pressure
	}
}

func (r *natReporter) run(ctx context.Context) {
	cl := &http.Client{Timeout: 10 * time.Second}
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	var batch []natEvent
	flush := func() {
		if len(batch) == 0 || r.url == "" {
			batch = batch[:0]
			return
		}
		b, _ := json.Marshal(natReport{Network: r.network, Events: batch})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url, bytes.NewReader(b))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(natTokenHdr, r.token)
			if resp, err := cl.Do(req); err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			} else {
				fmt.Fprintf(os.Stderr, "nat-gateway: report: %v\n", err)
			}
		}
		batch = batch[:0]
	}
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-r.ch:
			batch = append(batch, ev)
			if len(batch) >= 100 {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}
