package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func wantMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Errorf("%s: mode %v, want %v", path, got, want)
	}
}

// waitLog waits for the service log to hold every one of want. A request's
// line is written once its handler returns, which can be just after the
// client has its reply.
func waitLog(t *testing.T, home string, want ...string) string {
	t.Helper()
	var s string
	waitFor(t, "service.log lines", func() bool {
		b, err := os.ReadFile(filepath.Join(home, "logs", "service.log"))
		if err != nil {
			return false
		}
		s = string(b)
		for _, w := range want {
			if !strings.Contains(s, w) {
				return false
			}
		}
		return true
	})
	return s
}

func TestServiceLogRotatesBySize(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("OPENAI_BASE_URL", "")
	svc, err := start(config{listen: "127.0.0.1:0", home: home, upstream: mustURL("http://127.0.0.1:1"),
		chatgpt: mustURL("http://127.0.0.1:1"), logf: t.Logf, logMax: 2000})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.stop)
	for i := 0; i < 100; i++ {
		rawGet(t, svc.addr, "/nowhere", "")
	}
	waitFor(t, "a rotated log", func() bool {
		_, err := os.Stat(filepath.Join(home, "logs", "service.log.1"))
		return err == nil
	})
	for _, name := range []string{"service.log", "service.log.1"} {
		p := filepath.Join(home, "logs", name)
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() > 2000 {
			t.Errorf("%s is %d bytes, over the 2000 bound", name, fi.Size())
		}
		wantMode(t, p, 0o600)
	}
}
