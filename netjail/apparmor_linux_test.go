package netjail

import (
	"io/fs"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/staysup/agentbus/queue"
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
			[]string{"; AppArmor", "Ubuntu 24.04", "agentbus has none",
				"curl -fsSL " + installScript + " | sudo sh -s apparmor /usr/local/bin/agentbus"}},
		// The helper reads its label after the kernel moved it.
		{"in the helper", appArmor{restricted: true, label: "unprivileged_userns (enforce)", bin: "/usr/local/bin/agentbus"},
			[]string{"sudo sh -s apparmor /usr/local/bin/agentbus"}},
		{"label unread", appArmor{restricted: true, bin: "/usr/local/bin/agentbus"},
			[]string{"sudo sh -s apparmor /usr/local/bin/agentbus"}},
		{"a binary the person can change", appArmor{restricted: true, label: "unconfined",
			bin: "/home/u/.local/bin/agentbus", open: "/home/u/.local/bin/agentbus"},
			[]string{"only root can change", "| sudo AGENTBUS_INSTALL_DIR=/usr/local/bin sh"}},
		// Review rounds 1 and 2: a path with a space was offered a
		// profile command that the shell split, then one install.sh
		// refuses. Such a path gets the install instead.
		{"a path with a space", appArmor{restricted: true, label: "unconfined", bin: "/opt/Agent Bus/agentbus"},
			[]string{"cannot name its path", "| sudo AGENTBUS_INSTALL_DIR=/usr/local/bin sh"}},
		{"a path with a quote", appArmor{restricted: true, label: "unconfined", bin: "/opt/it's/agentbus"},
			[]string{"| sudo AGENTBUS_INSTALL_DIR=/usr/local/bin sh"}},
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
		// A profile command is offered only for a path install.sh accepts.
		if (c.a.open != "" || !ProfilePathPlain(c.a.bin)) && strings.Contains(got, "sh -s apparmor") {
			t.Errorf("%s: offers a profile for a path others can change: %q", c.name, got)
		}
	}
}

// Every advice fits whole in the runner's report, after the longest reason
// it follows, for a binary at the usual path: a cut would lose the command.
func TestAppArmorAdviceFitsTheReport(t *testing.T) {
	reason := (&Error{Why: "loopback could not be brought up in the agent's namespace (operation not permitted)"}).Error()
	bin := "/usr/local/bin/agentbus"
	for _, a := range []appArmor{
		{restricted: true, label: "unconfined", bin: bin},
		{restricted: true, label: "unconfined", bin: bin, open: bin},
		{restricted: true, label: "unconfined", bin: "/opt/a b/agentbus"},
		{restricted: true, label: "unconfined"},
	} {
		if why := reason + appArmorAdvice(a); len(why) > queue.MaxSandboxWhy {
			t.Errorf("%d bytes, over %d: %s", len(why), queue.MaxSandboxWhy, why)
		}
	}
}
