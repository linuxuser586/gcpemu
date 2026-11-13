//go:build prerelease

package prerelease

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// logs is the tee of the run's output to its steps' logs (#1139), or nil
// when there is none.
var logs *stepLogs

// stepLogs copies what the run writes to os.Stdout to the terminal and to
// the log of the step running: go test -v's lines, t.Log's and t.Fatal's
// among them, and the output of the commands a step streams. The steps
// run one at a time, so what is written between a step's start and end
// is its own. go test writes t.Log's lines as they come only with -v,
// which just prerelease passes; without it they come after the step ends
// and reach no log. The test binary's own stderr, such as the panic of
// go test's -timeout, is not copied.
type stepLogs struct {
	// w is the pipe's write end, os.Stdout while the tee runs.
	w *os.File
	// term is where the output goes besides the log: stdout, os.Stdout
	// before, unless a test says otherwise.
	term   io.Writer
	stdout *os.File
	// mu makes the switches from one log to another one at a time.
	mu   sync.Mutex
	acks chan struct{}
}

// logMark, a path and a NUL switch the log: it goes through the pipe
// with the output, so the reader switches after what was written before.
const logMark = "\x00gcpemu-prerelease-log:"

// teeStdout makes os.Stdout a pipe whose reader copies to the terminal
// and to the step's log.
func teeStdout() (*stepLogs, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	l := newStepLogs(w, os.Stdout)
	l.stdout, os.Stdout = os.Stdout, w
	go l.copy(r)
	return l, nil
}

// newStepLogs is the tee of what is written to w, the write end of a pipe
// whose reader is copy's, to term.
func newStepLogs(w *os.File, term io.Writer) *stepLogs {
	return &stepLogs{w: w, term: term, acks: make(chan struct{})}
}

// to sends what is written from now on to the log at path, which it
// creates, as well as the terminal, or to no log when path is "" (end).
// It returns once what was written before has gone to the previous log.
func (l *stepLogs) to(path string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := fmt.Fprintf(l.w, "%s%s\x00", logMark, path); err != nil {
		return
	}
	<-l.acks
}

// end sends what is written from now on to the terminal only.
func (l *stepLogs) end() { l.to("") }

// stop ends the last log and gives os.Stdout back. The pipe stays open:
// a process the run left, such as a detached Instance, may hold its write
// end, and its reader goes on copying it to the terminal.
func (l *stepLogs) stop() {
	if l == nil {
		return
	}
	l.end()
	if l.stdout != nil {
		os.Stdout = l.stdout
	}
}

// copy copies r to the terminal and the log, switching the log at each
// mark, until r ends.
func (l *stepLogs) copy(r io.Reader) {
	var log *os.File
	defer func() {
		if log != nil {
			log.Close()
		}
	}()
	out := func(b []byte) {
		if len(b) == 0 {
			return
		}
		l.term.Write(b)
		if log != nil {
			log.Write(b)
		}
	}
	var pending []byte
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		pending = append(pending, buf[:n]...)
		for len(pending) > 0 {
			i := bytes.IndexByte(pending, 0)
			if i < 0 {
				out(pending)
				pending = pending[:0]
				break
			}
			out(pending[:i])
			pending = pending[i:]
			if len(pending) < len(logMark) && logMark[:len(pending)] == string(pending) {
				break // perhaps a mark, not all read yet
			}
			if !bytes.HasPrefix(pending, []byte(logMark)) {
				out(pending[:1]) // a NUL of the output's own
				pending = pending[1:]
				continue
			}
			end := bytes.IndexByte(pending[len(logMark):], 0)
			if end < 0 {
				break
			}
			path := string(pending[len(logMark) : len(logMark)+end])
			pending = pending[len(logMark)+end+1:]
			if log != nil {
				log.Close()
				log = nil
			}
			if path != "" {
				var err error
				if log, err = createLog(path); err != nil {
					fmt.Fprintf(l.term, "\nthis step's output is not saved: %v\n", err)
				}
			}
			l.acks <- struct{}{}
		}
		if err != nil {
			out(pending)
			return
		}
	}
}

// createLog creates the log at path, with its dir.
func createLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return os.Create(path)
}
