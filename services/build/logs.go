package build

import (
	"bytes"
	"net/http"
	"sync"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// maxLog caps the log kept for one Build; output past it is dropped.
const maxLog = 8 << 20

// logs keeps each Build's log in memory, by key, until a Reset. Where
// Build logs are stored is decided with Build sources (#385).
type logs struct {
	mu sync.Mutex
	m  map[string]*bytes.Buffer
}

// line appends a line to a Build's log.
func (l *logs) line(ref buildRef, s string) { l.write(ref, []byte(s+"\n")) }

func (l *logs) write(ref buildRef, p []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[string]*bytes.Buffer{}
	}
	b := l.m[ref.key()]
	if b == nil {
		b = &bytes.Buffer{}
		l.m[ref.key()] = b
	}
	switch {
	case b.Len() >= maxLog:
	case b.Len()+len(p) > maxLog:
		b.WriteString("(log truncated)\n")
	default:
		b.Write(p)
	}
}

func (l *logs) get(ref buildRef) []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b := l.m[ref.key()]; b != nil {
		return bytes.Clone(b.Bytes())
	}
	return nil
}

func (l *logs) clear() {
	l.mu.Lock()
	l.m = nil
	l.mu.Unlock()
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

// serveLog serves a Build's log as text, as Build.logUrl points to.
func (s *Service) serveLog(w http.ResponseWriter, r *http.Request) {
	ref := buildRef{r.PathValue("project"), r.PathValue("location"), r.PathValue("id")}
	if _, err := s.load(ref); err != nil {
		apierr.Write(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(s.logs.get(ref))
}
