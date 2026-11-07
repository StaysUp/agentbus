package logfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLogRotatesAndStaysPrivate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "service.log")
	// An existing file with a loose mode is made private.
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := Open(p, 500, func() time.Time { return time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		l.Printf("request path=/x n=%d\nsecond", i)
	}
	l.Close()
	for _, name := range []string{"service.log", "service.log.1"} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() > 500 {
			t.Errorf("%s: %d bytes, over 500", name, fi.Size())
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: mode %v", name, fi.Mode().Perm())
		}
	}
	b, _ := os.ReadFile(p)
	for _, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if !strings.HasPrefix(line, "2026-11-05T09:00:00.000Z request path=/x n=") || !strings.HasSuffix(line, " second") {
			t.Errorf("line %q", line)
		}
	}
	if !strings.Contains(string(b), "n=99 ") {
		t.Error("the newest line is not in the current file")
	}
}

func TestTailKeepsTheLastBytes(t *testing.T) {
	tl := &Tail{Max: 5}
	tl.Write([]byte("abc"))
	if b, cut := tl.Bytes(); string(b) != "abc" || cut {
		t.Errorf("%q %v", b, cut)
	}
	tl.Write([]byte("defg"))
	if b, cut := tl.Bytes(); string(b) != "cdefg" || !cut {
		t.Errorf("%q %v", b, cut)
	}
	tl.Write([]byte("0123456789"))
	if b, _ := tl.Bytes(); string(b) != "56789" {
		t.Errorf("%q", b)
	}
}

func TestAppendTailBoundsTheFileAtALineBreak(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.log")
	for i := 0; i < 20; i++ {
		if err := AppendTail(p, []byte("=== header\nline line line\n"), 100); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(p)
	if len(b) > 100 || len(b) < 50 {
		t.Errorf("%d bytes", len(b))
	}
	if !strings.HasPrefix(string(b), "=== header\n") && !strings.HasPrefix(string(b), "line line line\n") {
		t.Errorf("cut mid-line: %q", b)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	if m, _ := filepath.Glob(filepath.Join(filepath.Dir(p), ".cut-*")); len(m) != 0 {
		t.Errorf("left behind: %v", m)
	}
}

func TestDirIsPrivate(t *testing.T) {
	d := filepath.Join(t.TempDir(), "logs")
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Dir(d); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(d)
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
}
