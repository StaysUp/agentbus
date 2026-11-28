package isolation

import (
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"
)

// ctimeSec is p's change time, in whole seconds, as git's stat cache keeps it.
func ctimeSec(t *testing.T, p string) int64 {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no change time here")
	}
	return ctimeOf(st)
}

// A Copy checkpoint sees an edit of the same size whose time was put back,
// made in the same second as the file's last change before the Base: git's
// stat cache, which compares change times to the second, takes that file as
// unchanged (review evt_25f42e7c0c965a61, P1.2). The test makes that second
// happen, trying again when the clock moves on between the copy and the edit.
func TestCopySeesAnEditInTheSameSecond(t *testing.T) {
	for try := 0; try < 20; try++ {
		src := t.TempDir()
		fixture(t, src, map[string]string{"old.go": "package one\n"})
		tick := time.Now().Add(-time.Hour)
		os.Chtimes(filepath.Join(src, "old.go"), tick, tick)
		b, s := open(t, Copy, src)
		p := filepath.Join(s.Dir, "old.go")
		before := ctimeSec(t, p)
		os.WriteFile(p, []byte("package xyz\n"), 0o644)
		os.Chtimes(p, tick, tick)
		if ctimeSec(t, p) != before {
			b.Close(s)
			continue // the second moved on: git would see it anyway
		}
		if _, err := b.Checkpoint(s, "t1"); err != nil {
			t.Fatal(err)
		}
		if got := changesetNames(t, s, "t1"); !reflect.DeepEqual(got, []string{"old.go"}) {
			t.Fatalf("changeset: %v, want [old.go]", got)
		}
		return
	}
	t.Skip("never edited in the copy's own second")
}
