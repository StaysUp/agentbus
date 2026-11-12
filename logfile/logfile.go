// Package logfile writes the service's own log files in the data folder:
// logs/service.log, one plain line per thing the service did, and
// logs/runs/<session>.log, the output of the agents the service started.
//
// What may go in each is decided by the callers, in cmd/agentbus and api. This
// package only keeps the files private (0600 in a 0700 folder) and bounded.
package logfile

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Dir makes dir with mode 0700, and makes it 0700 again if it already existed
// with another mode.
func Dir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// openPrivate opens path for appending, creating it 0600, and makes an
// existing file 0600 too.
func openPrivate(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// Log is a line log in one file that rotates by size: when a line would take
// the file past Max bytes, the file becomes <path>.1 (replacing any older one)
// and a new file starts. At most about 2×Max bytes are kept.
type Log struct {
	mu   sync.Mutex
	path string
	max  int64
	now  func() time.Time
	f    *os.File
	size int64
}

// Open opens or creates the log at path. max at or below zero means 10 MiB.
// now nil means time.Now.
func Open(path string, max int64, now func() time.Time) (*Log, error) {
	if max <= 0 {
		max = 10 << 20
	}
	if now == nil {
		now = time.Now
	}
	l := &Log{path: path, max: max, now: now}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *Log) open() error {
	f, err := openPrivate(l.path)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.size = f, fi.Size()
	return nil
}

// Printf writes one line: the UTC time, then the formatted text. Line breaks in
// the text become spaces, so a line is always one line.
func (l *Log) Printf(format string, args ...any) {
	if l == nil {
		return
	}
	msg := strings.NewReplacer("\r", " ", "\n", " ").Replace(fmt.Sprintf(format, args...))
	l.mu.Lock()
	defer l.mu.Unlock()
	line := l.now().UTC().Format("2006-01-02T15:04:05.000Z") + " " + msg + "\n"
	if l.f == nil {
		return
	}
	if l.size > 0 && l.size+int64(len(line)) > l.max {
		l.f.Close()
		l.f = nil
		_ = os.Rename(l.path, l.path+".1")
		if err := l.open(); err != nil {
			return
		}
	}
	n, _ := io.WriteString(l.f, line)
	l.size += int64(n)
}

// Close closes the file. Lines written after it are dropped.
func (l *Log) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// Tail keeps the last Max bytes written to it, and drops the rest. It is safe
// for concurrent writes, so one Tail can take a child's stdout and stderr.
type Tail struct {
	Max int
	mu  sync.Mutex
	buf []byte
	cut bool // bytes were dropped from the front
}

func (t *Tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(p)
	if len(p) >= t.Max {
		// Cut only when bytes were dropped: one write of exactly Max into
		// an empty buffer keeps it all.
		t.cut = t.cut || len(p) > t.Max || len(t.buf) > 0
		t.buf = append(t.buf[:0], p[len(p)-t.Max:]...)
		return n, nil
	}
	if over := len(t.buf) + len(p) - t.Max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
		t.cut = true
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

// Bytes returns what is kept, and whether anything was dropped before it.
func (t *Tail) Bytes() ([]byte, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]byte(nil), t.buf...), t.cut
}

// AppendTail appends data to the file at path, creating it 0600, then cuts the
// file to its last max bytes if it is longer. A cut starts at the first line
// break inside the kept part, so the file never begins mid-line unless one
// line is longer than max.
func AppendTail(path string, data []byte, max int64) error {
	f, err := openPrivate(path)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	fi, serr := f.Stat()
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	if serr != nil {
		return serr
	}
	if fi.Size() <= max {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	keep := b[int64(len(b))-max:]
	if i := bytes.IndexByte(keep, '\n'); i >= 0 && i+1 < len(keep) {
		keep = keep[i+1:]
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cut-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // nothing left behind if the rename fails
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(keep); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
