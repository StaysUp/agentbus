package netjail

import (
	"io/fs"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeInfo is a file's owner and mode, as Lstat would give them.
type fakeInfo struct {
	mode fs.FileMode
	uid  uint32
}

func (f fakeInfo) Name() string       { return "" }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeInfo) Sys() any           { return &syscall.Stat_t{Uid: f.uid} }

// A path only root can change has nothing open; a binary or a folder above
// it that another user owns, or that its group or others may write, is
// named.
func TestOpenToOthers(t *testing.T) {
	root := map[string]fakeInfo{
		"/":                           {fs.ModeDir | 0o755, 0},
		"/usr":                        {fs.ModeDir | 0o755, 0},
		"/usr/local":                  {fs.ModeDir | 0o755, 0},
		"/usr/local/bin":              {fs.ModeDir | 0o755, 0},
		"/usr/local/bin/agentbus":     {0o755, 0},
		"/home":                       {fs.ModeDir | 0o755, 0},
		"/home/u":                     {fs.ModeDir | 0o750, 1000},
		"/home/u/.local/bin":          {fs.ModeDir | 0o755, 1000},
		"/home/u/.local":              {fs.ModeDir | 0o755, 1000},
		"/home/u/.local/bin/agentbus": {0o755, 1000},
		"/opt":                        {fs.ModeDir | 0o775, 0},
		"/opt/agentbus":               {0o755, 0},
		"/tmp":                        {fs.ModeDir | fs.ModeSticky | 0o777, 0},
		"/tmp/agentbus":               {0o755, 0},
		"/srv":                        {fs.ModeDir | 0o755, 0},
		"/srv/agentbus":               {0o757, 0},
	}
	stat := func(p string) (os.FileInfo, error) {
		if i, ok := root[p]; ok {
			return i, nil
		}
		return nil, os.ErrNotExist
	}
	for p, want := range map[string]string{
		"/usr/local/bin/agentbus":     "",
		"/home/u/.local/bin/agentbus": "/home/u/.local/bin/agentbus",
		"/opt/agentbus":               "/opt",
		"/tmp/agentbus":               "/tmp",
		"/srv/agentbus":               "/srv/agentbus",
		"/gone/agentbus":              "/gone/agentbus",
	} {
		if got := openToOthers(p, stat); got != want {
			t.Errorf("openToOthers(%s) = %q, want %q", p, got, want)
		}
	}
}

// The advice names what is missing and the one command that adds it, and
// says nothing where AppArmor's rule is not why.
func TestAppArmorAdvice(t *testing.T) {
	for _, c := range []struct {
		name string
		a    appArmor
		want []string // in the advice, in order; none: no advice
	}{
		{"not restricted", appArmor{label: "unconfined", bin: "/usr/local/bin/agentbus"}, nil},
		{"own profile", appArmor{restricted: true, label: "agentbus (unconfined)", bin: "/usr/local/bin/agentbus"}, nil},
		{"another profile", appArmor{restricted: true, label: "snap.x (enforce)", bin: "/usr/local/bin/agentbus"}, nil},
		{"no profile", appArmor{restricted: true, label: "unconfined", bin: "/usr/local/bin/agentbus"},
			[]string{"; AppArmor", "kernel.apparmor_restrict_unprivileged_userns is 1", "without one",
				"curl -fsSL " + installScript + " | sudo sh -s apparmor /usr/local/bin/agentbus"}},
		// The helper reads its label after the kernel moved it.
		{"in the helper", appArmor{restricted: true, label: "unprivileged_userns (enforce)", bin: "/usr/local/bin/agentbus"},
			[]string{"sudo sh -s apparmor /usr/local/bin/agentbus"}},
		{"label unread", appArmor{restricted: true, bin: "/usr/local/bin/agentbus"},
			[]string{"sudo sh -s apparmor /usr/local/bin/agentbus"}},
		{"a binary the person can change", appArmor{restricted: true, label: "unconfined",
			bin: "/home/u/.local/bin/agentbus", open: "/home/u/.local/bin/agentbus"},
			[]string{"only root can change", "/home/u/.local/bin/agentbus can be changed by another user",
				"| sudo AGENTBUS_INSTALL_DIR=/usr/local/bin sh", "then run /usr/local/bin/agentbus"}},
		{"a path with a space", appArmor{restricted: true, label: "unconfined", bin: "/opt/Agent Bus/it's"},
			[]string{`sudo sh -s apparmor '/opt/Agent Bus/it'\''s'`}},
		{"binary unknown", appArmor{restricted: true, label: "unconfined"},
			[]string{"| sudo AGENTBUS_INSTALL_DIR=/usr/local/bin sh"}},
	} {
		got := appArmorAdvice(c.a)
		if c.want == nil {
			if got != "" {
				t.Errorf("%s: %q, want none", c.name, got)
			}
			continue
		}
		rest := got
		for _, w := range c.want {
			i := strings.Index(rest, w)
			if i < 0 {
				t.Errorf("%s: %q lacks %q (in order)", c.name, got, w)
				break
			}
			rest = rest[i+len(w):]
		}
		// A command for a path that others can change is never offered.
		if c.a.open != "" && strings.Contains(got, "sh -s apparmor") {
			t.Errorf("%s: offers a profile for a path others can change: %q", c.name, got)
		}
	}
}
