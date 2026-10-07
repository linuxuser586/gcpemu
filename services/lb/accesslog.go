package lb

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/reqlog"
)

// Access logs (FR-LB-010): one Cloud Logging LogEntry per request in the
// http_load_balancer shape, honouring backendService.logConfig
// (enable, sampleRate). Entries go to the emulator log, the request log
// and, as JSON lines, to <data-dir>/lb/requests.log.

// LogEntry is the Cloud Logging entry of one load-balanced request.
type LogEntry struct {
	InsertID    string            `json:"insertId"`
	LogName     string            `json:"logName"`
	Resource    logResource       `json:"resource"`
	Timestamp   string            `json:"timestamp"`
	Severity    string            `json:"severity"`
	HTTPRequest httpRequest       `json:"httpRequest"`
	JSONPayload map[string]any    `json:"jsonPayload"`
	Trace       string            `json:"trace,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

type logResource struct {
	Type   string            `json:"type"`
	Labels map[string]string `json:"labels"`
}

type httpRequest struct {
	RequestMethod                  string `json:"requestMethod"`
	RequestURL                     string `json:"requestUrl"`
	RequestSize                    string `json:"requestSize,omitempty"`
	Status                         int    `json:"status"`
	ResponseSize                   string `json:"responseSize,omitempty"`
	UserAgent                      string `json:"userAgent,omitempty"`
	RemoteIP                       string `json:"remoteIp"`
	ServerIP                       string `json:"serverIp,omitempty"`
	Referer                        string `json:"referer,omitempty"`
	Latency                        string `json:"latency"`
	CacheLookup                    bool   `json:"cacheLookup,omitempty"`
	CacheHit                       bool   `json:"cacheHit,omitempty"`
	CacheValidatedWithOriginServer bool   `json:"cacheValidatedWithOriginServer,omitempty"`
	Protocol                       string `json:"protocol"`
}

type accessLogger struct {
	env  *emu.Env
	ch   chan *LogEntry
	done chan struct{}
	once sync.Once

	mu     sync.Mutex
	recent []*LogEntry // ring of the last entries (tests, admin)
	next   int
}

const recentLogs = 1000

func newAccessLogger(env *emu.Env) *accessLogger {
	l := &accessLogger{env: env, ch: make(chan *LogEntry, 4096), done: make(chan struct{})}
	go l.run()
	return l
}

func (l *accessLogger) run() {
	defer close(l.done)
	var w *bufio.Writer
	var f *os.File
	if dir, err := l.env.ServiceDir("lb"); err == nil {
		if f, err = os.OpenFile(filepath.Join(dir, "requests.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			w = bufio.NewWriterSize(f, 64<<10)
		}
	}
	flush := time.NewTicker(time.Second)
	defer flush.Stop()
	for {
		select {
		case e, ok := <-l.ch:
			if !ok {
				if w != nil {
					_ = w.Flush()
					_ = f.Close()
				}
				return
			}
			b, _ := json.Marshal(e)
			if w != nil {
				_, _ = w.Write(append(b, '\n'))
			}
			l.env.Log.Info("lb request", "logEntry", json.RawMessage(b))
		case <-flush.C:
			if w != nil {
				_ = w.Flush()
			}
		}
	}
}

func (l *accessLogger) close() {
	l.once.Do(func() { close(l.ch) })
	<-l.done
}

// Recent returns the most recent access log entries, oldest first.
func (l *accessLogger) Recent() []*LogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []*LogEntry
	n := len(l.recent)
	for i := 0; i < n; i++ {
		e := l.recent[(l.next+i)%n]
		if e != nil {
			out = append(out, e)
		}
	}
	return out
}

// log records one request.
func (l *accessLogger) log(rs *reqState, w *respWriter) {
	lat := time.Since(rs.start)
	r := rs.req
	fe := rs.fe
	if l.env.RequestLog != nil {
		l.env.RequestLog.Add(reqlog.Entry{Time: rs.start, Service: "lb", Protocol: "http", Method: r.Method + " " + r.Host + r.URL.RequestURI(),
			Resource: fe.frPath, Status: w.status, LatencyMS: float64(lat.Microseconds()) / 1000})
	}
	if rs.kind != "" && (!rs.logEnable || (rs.sample < 1 && rand.Float64() >= rs.sample)) {
		return
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	status := rs.cdnStatus
	e := &LogEntry{
		InsertID:  strconv.FormatUint(rand.Uint64(), 36),
		LogName:   "projects/" + fe.project + "/logs/requests",
		Timestamp: rs.start.UTC().Format(time.RFC3339Nano),
		Severity:  "INFO",
		HTTPRequest: httpRequest{
			RequestMethod: r.Method, RequestURL: scheme + "://" + r.Host + r.URL.RequestURI(),
			Status: w.status, UserAgent: r.UserAgent(), RemoteIP: rs.clientIP, ServerIP: fe.ip, Referer: r.Referer(),
			Latency: fmt.Sprintf("%.6fs", lat.Seconds()), Protocol: r.Proto,
			CacheLookup: rs.cdnOn, CacheHit: status == "hit", CacheValidatedWithOriginServer: status == "revalidated",
		},
		JSONPayload: map[string]any{
			"@type":                      "type.googleapis.com/google.cloud.loadbalancing.type.LoadBalancerLogEntry",
			"statusDetails":              rs.details,
			"backendTargetProjectNumber": "projects/" + fe.number,
			"remoteIp":                   rs.clientIP,
		},
	}
	if r.ContentLength > 0 {
		e.HTTPRequest.RequestSize = strconv.FormatInt(r.ContentLength, 10)
	}
	e.HTTPRequest.ResponseSize = strconv.FormatInt(w.bytes, 10)
	if rs.cdnOn {
		e.JSONPayload["cacheId"] = "gcpemu"
		if rs.cdnStatus == "hit" {
			e.JSONPayload["cacheDecision"] = []string{"RESPONSE_HAS_CONTENT_TYPE", "CACHE_MODE"}
		}
	}
	if rs.bLat > 0 {
		e.JSONPayload["backendLatency"] = fmt.Sprintf("%.6fs", rs.bLat.Seconds())
	}
	if t := r.Header.Get("X-Cloud-Trace-Context"); t != "" {
		if i := indexAny(t, "/;"); i > 0 {
			t = t[:i]
		}
		e.Trace = "projects/" + fe.project + "/traces/" + t
	}
	labels := map[string]string{
		"project_id":           fe.project,
		"forwarding_rule_name": fe.fr.Name,
		"target_proxy_name":    lastSeg(fe.proxyPath),
		"url_map_name":         lastSeg(fe.urlMap),
		"backend_service_name": "",
		"backend_target_name":  "",
		"backend_type":         "",
	}
	typ := "http_load_balancer"
	if fe.region != "" {
		labels["region"] = fe.region
		typ = "http_external_regional_lb_rule"
		if fe.fr.LoadBalancingScheme == "INTERNAL_MANAGED" {
			typ = "internal_http_lb_rule"
			labels["network_name"] = lastSeg(fe.fr.Network)
		}
	} else {
		labels["zone"] = "global"
	}
	switch rs.kind {
	case "backendService":
		labels["backend_service_name"] = lastSeg(rs.service)
		labels["backend_target_name"] = lastSeg(rs.service)
		labels["backend_type"] = "BACKEND_SERVICE"
	case "backendBucket":
		labels["backend_target_name"] = lastSeg(rs.service)
		labels["backend_type"] = "BACKEND_BUCKET"
	}
	if typ != "http_load_balancer" {
		delete(labels, "backend_target_name")
		delete(labels, "backend_type")
	}
	e.Resource = logResource{Type: typ, Labels: labels}
	l.mu.Lock()
	if l.recent == nil {
		l.recent = make([]*LogEntry, recentLogs)
	}
	l.recent[l.next] = e
	l.next = (l.next + 1) % recentLogs
	l.mu.Unlock()
	select {
	case l.ch <- e:
	default: // never block the data plane
	}
}

func indexAny(s, chars string) int {
	for i := 0; i < len(s); i++ {
		for j := 0; j < len(chars); j++ {
			if s[i] == chars[j] {
				return i
			}
		}
	}
	return -1
}
