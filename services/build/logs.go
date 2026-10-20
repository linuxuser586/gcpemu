package build

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/project"
)

// maxLog caps the log kept for one Build; output past it is dropped.
const maxLog = 8 << 20

// logFlushInterval is how often a WORKING Build's new output is appended
// to its log object. Each append copies the object (emu.ObjectStore), and
// gcloud polls the log about once a second.
var logFlushInterval = 500 * time.Millisecond

// logContentType is the log object's content type.
const logContentType = "text/plain; charset=utf-8"

// logs keeps each Build's log in memory, by key, until a Reset, and
// appends what it has not sent yet to the Build's log object in Cloud
// Storage (flushLog).
type logs struct {
	mu sync.Mutex
	m  map[string]*buildLog
}

// buildLog is one Build's log. logs.mu guards buf and sent; flushing
// guards warned.
type buildLog struct {
	buf bytes.Buffer
	// sent counts the bytes of buf already in the log object.
	sent int
	// flushing serializes appends to the log object.
	flushing sync.Mutex
	// warned is set once a failed append has been logged.
	warned bool
	// masked are the Build's secret payloads, which logs.mu guards and
	// write replaces with secretMask.
	masked []string
}

// line appends a line to a Build's log.
func (l *logs) line(ref buildRef, s string) { l.write(ref, []byte(s+"\n")) }

func (l *logs) write(ref buildRef, p []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entry(ref)
	for _, m := range e.masked {
		p = bytes.ReplaceAll(p, []byte(m), []byte(secretMask))
	}
	switch {
	case e.buf.Len() >= maxLog:
		return
	case e.buf.Len()+len(p) > maxLog:
		p = []byte("(log truncated)\n")
	}
	e.buf.Write(p)
}

// mask has every later line of a Build's log carry secretMask in place of
// each of values.
func (l *logs) mask(ref buildRef, values []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entry(ref)
	for _, v := range values {
		if v != "" {
			e.masked = append(e.masked, v)
		}
	}
}

// entry returns a Build's log, adding it. Callers hold mu.
func (l *logs) entry(ref buildRef) *buildLog {
	if l.m == nil {
		l.m = map[string]*buildLog{}
	}
	e := l.m[ref.key()]
	if e == nil {
		e = &buildLog{}
		l.m[ref.key()] = e
	}
	return e
}

func (l *logs) get(ref buildRef) []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.m[ref.key()]; e != nil {
		return bytes.Clone(e.buf.Bytes())
	}
	return nil
}

func (l *logs) clear() {
	l.mu.Lock()
	l.m = nil
	l.mu.Unlock()
}

// flush hands the part of a Build's log not sent yet to send, and counts
// it sent once send succeeds. warn reports that send failed for the
// first time for the Build.
func (l *logs) flush(ref buildRef, send func([]byte) error) (warn bool, err error) {
	l.mu.Lock()
	e := l.entry(ref)
	l.mu.Unlock()
	e.flushing.Lock()
	defer e.flushing.Unlock()
	l.mu.Lock()
	p := bytes.Clone(e.buf.Bytes()[e.sent:])
	l.mu.Unlock()
	if len(p) == 0 {
		return false, nil
	}
	if err := send(p); err != nil {
		warn, e.warned = !e.warned, true
		return warn, err
	}
	l.mu.Lock()
	e.sent += len(p)
	l.mu.Unlock()
	return false, nil
}

// writer returns a writer that adds each line written to it to a Build's
// log after prefix.
func (l *logs) writer(ref buildRef, prefix string) *lineWriter {
	return &lineWriter{l: l, ref: ref, prefix: prefix}
}

// lineWriter prefixes whole lines; flush writes a last partial one.
type lineWriter struct {
	l       *logs
	ref     buildRef
	prefix  string
	partial []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.partial = append(w.partial, p...)
	for {
		i := bytes.IndexByte(w.partial, '\n')
		if i < 0 {
			return len(p), nil
		}
		w.l.write(w.ref, append([]byte(w.prefix), w.partial[:i+1]...))
		w.partial = w.partial[i+1:]
	}
}

func (w *lineWriter) flush() {
	if len(w.partial) > 0 {
		w.l.write(w.ref, append(append([]byte(w.prefix), w.partial...), '\n'))
		w.partial = nil
	}
}

// defaultLogsBucket is the logsBucket of a Build that names none, as GCP
// documents it.
func defaultLogsBucket(projectID string) string {
	return project.NumberString(projectID) + ".cloudbuild-logs.googleusercontent.com"
}

// logObject splits a Build's logsBucket ("gs://B" or "gs://B/some/dir") into
// the bucket and the log object's name, as gcloud's log tailer does.
func logObject(b *cloudbuildpb.Build) (bucket, object string) {
	bucket, dir, _ := strings.Cut(strings.TrimPrefix(b.GetLogsBucket(), "gs://"), "/")
	object = "log-" + b.GetId() + ".txt"
	if dir = strings.Trim(dir, "/"); dir != "" {
		object = dir + "/" + object
	}
	return bucket, object
}

// objects returns the Cloud Storage Service, or nil when it is not running.
func (s *Service) objects() emu.ObjectStore {
	svc, ok := s.env.Lookup("gcs")
	if !ok {
		return nil
	}
	objs, _ := svc.(emu.ObjectStore)
	return objs
}

// flushLog appends what the Build's log has not sent yet to its log
// object. Without Cloud Storage the log stays in memory only.
func (s *Service) flushLog(b *cloudbuildpb.Build) {
	objs := s.objects()
	if objs == nil {
		return
	}
	bucket, object := logObject(b)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	warn, err := s.logs.flush(refOf(b), func(p []byte) error {
		return objs.AppendObject(ctx, bucket, object, logContentType, p)
	})
	if warn {
		s.env.Log.Warn("build: writing the Build log to Cloud Storage", "build", b.GetName(), "bucket", bucket, "err", err)
	}
}

// flushLogs flushes a WORKING Build's log every logFlushInterval until
// stop is closed or the Build is halted.
func (s *Service) flushLogs(r *liveBuild, b *cloudbuildpb.Build, stop <-chan struct{}) {
	t := time.NewTicker(logFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		if r.halted() {
			return
		}
		s.flushLog(b)
	}
}

// serveLog serves a Build's log as text.
func (s *Service) serveLog(w http.ResponseWriter, r *http.Request) {
	ref := buildRef{r.PathValue("project"), r.PathValue("location"), r.PathValue("id")}
	if _, err := s.load(ref); err != nil {
		apierr.Write(w, err)
		return
	}
	w.Header().Set("Content-Type", logContentType)
	_, _ = w.Write(s.logs.get(ref))
}
