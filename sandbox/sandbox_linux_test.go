package sandbox

import (
	"os"
	"slices"
	"syscall"
	"testing"
)

// A read path under /etc that etcRead does not bind is bound; one it binds
// is there already (review of 2026-11-12, round 6).
func TestBwrapArgsEtc(t *testing.T) {
	a := bwrapArgs("/c", false, "/h", []path{{real: "/etc/nginx", dir: true}, {real: "/etc/ssl", dir: true}})
	bound := func(p string) bool {
		for i := 0; i+2 < len(a); i++ {
			if a[i] == "--ro-bind" && a[i+1] == p && a[i+2] == p {
				return true
			}
		}
		return false
	}
	if !bound("/etc/nginx") {
		t.Errorf("/etc/nginx not bound: %v", a)
	}
	if slices.Contains(a, "/etc") {
		t.Errorf("all of /etc bound: %v", a)
	}
	if !inSystem("/etc/ssl/certs") || inSystem("/etc/nginx") || !inSystem("/usr/bin/sh") {
		t.Error("inSystem")
	}
}

// fakeInfo is a file's owner and mode, for closedToRoot.
type fakeInfo struct {
	os.FileInfo
	mode     os.FileMode
	uid, gid uint32
}

func (f fakeInfo) Mode() os.FileMode { return f.mode }
func (f fakeInfo) IsDir() bool       { return f.mode.IsDir() }
func (f fakeInfo) Sys() any          { return &syscall.Stat_t{Uid: f.uid, Gid: f.gid} }

// A runner run as root: what root owns, and what is open to everyone, is
// reached; a home of another user's at 0750 stops it, and so does a file of
// root's own that a group of root's cannot read (review of 2026-11-12,
// release prep: root's PATH named the user's claude).
func TestClosedToRoot(t *testing.T) {
	dir := os.ModeDir
	files := map[string]fakeInfo{
		"/":                 {mode: dir | 0o755},
		"/home":             {mode: dir | 0o755},
		"/home/u":           {mode: dir | 0o750, uid: 501, gid: 1000},
		"/home/u/claude":    {mode: 0o755, uid: 501, gid: 1000},
		"/home/open":        {mode: dir | 0o755, uid: 501, gid: 1000},
		"/home/open/codex":  {mode: 0o755, uid: 501, gid: 1000},
		"/home/open/secret": {mode: 0o600, uid: 501, gid: 1000},
		"/home/open/folder": {mode: dir | 0o744, uid: 501, gid: 1000},
		"/root":             {mode: dir | 0o700},
		"/root/claude":      {mode: 0o700},
		"/srv":              {mode: dir | 0o750, gid: 50},
		"/srv/data":         {mode: dir | 0o750, uid: 0, gid: 50},
		"/srv/groupread":    {mode: 0o040, uid: 501, gid: 0},
	}
	stat := func(p string) (os.FileInfo, error) {
		if f, ok := files[p]; ok {
			return f, nil
		}
		return nil, os.ErrNotExist
	}
	for _, c := range []struct {
		path, at string
	}{
		{"/root/claude", ""},
		{"/home/open/codex", ""},
		{"/srv/data", ""},
		{"/srv/groupread", ""},
		{"/home/u/claude", "/home/u"},
		{"/home/open/secret", "/home/open/secret"},
		{"/home/open/folder", "/home/open/folder"},
		{"/missing", "/missing"},
	} {
		p, at := closedToRoot([]string{c.path}, []int{0}, stat)
		if at != c.at || (at != "" && p != c.path) {
			t.Errorf("%s: stopped at %q, want %q", c.path, at, c.at)
		}
	}
}
