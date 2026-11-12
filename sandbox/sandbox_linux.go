package sandbox

// Linux: bubblewrap. The agent sees an empty root (a tmpfs) with only these
// bound in: the system's programs and libraries read only, a few files of
// /etc, its own /proc (a pid namespace of its own, so no other process's
// files), its own /dev and /tmp, the paths it may read, and its copy and
// home read and write, each at its own path. bwrap runs inside netjail's
// network namespace (netjail.Jail.Start), so it does not make a network
// namespace of its own: the agent reaches only the forwarded addresses and
// the proxy there.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/staysup/agentbus/netjail"
)

// Supported: Linux has bubblewrap.
const Supported = true

// bwrap is bubblewrap, the system's own: never looked up on PATH.
const bwrap = "/usr/bin/bwrap"

type platform struct {
	args []string
}

// etcRead are the files and folders of /etc a session reads: users and
// groups (getpwuid), names, certificates, the dynamic linker's cache, the
// time zone, and Debian's alternatives, which programs in /usr/bin link
// through.
var etcRead = []string{
	"/etc/passwd", "/etc/group", "/etc/hosts", "/etc/resolv.conf", "/etc/nsswitch.conf", "/etc/host.conf",
	"/etc/ssl", "/etc/ca-certificates", "/etc/pki", "/etc/ld.so.cache", "/etc/ld.so.conf", "/etc/ld.so.conf.d",
	"/etc/localtime", "/etc/alternatives", "/etc/gitconfig",
}

// build makes bwrap's arguments for b and readies the network namespace.
func (b *Box) build(spec netjail.Spec) error {
	if info, err := os.Stat(bwrap); err != nil || info.IsDir() {
		return &Error{Why: bwrap + " is not on this runner; install bubblewrap (apt install bubblewrap)"}
	}
	if err := netjail.Check(); err != nil {
		return &Error{Why: err.Error()}
	}
	b.plat.args = bwrapArgs(b.copy, b.readOnly, b.home, b.read)
	// The check makes its own network namespace: it runs outside netjail.
	check := exec.Command(bwrap, append(append(append([]string{}, b.plat.args...), "--unshare-net"), "--", "/usr/bin/true")...)
	if !runCheck(check) {
		return &Error{Why: "bubblewrap could not start a program in it: a path it shows the session is missing, or its user namespace was refused (on Ubuntu 24.04 and later AppArmor refuses one to a program without a profile)"}
	}
	b.jail = netjail.New(spec)
	return nil
}

// bwrapArgs are bwrap's options for the wall, before "--".
func bwrapArgs(copy string, readOnly bool, home string, read []path) []string {
	a := []string{"--die-with-parent", "--unshare-user", "--unshare-pid", "--unshare-ipc", "--unshare-uts", "--unshare-cgroup-try",
		"--tmpfs", "/", "--ro-bind", "/usr", "/usr"}
	// /bin, /lib and the rest: links into /usr on a merged system, folders
	// on an older one.
	for _, d := range []string{"/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32"} {
		info, err := os.Lstat(d)
		switch {
		case err != nil:
		case info.Mode()&os.ModeSymlink != 0:
			if to, err := os.Readlink(d); err == nil {
				a = append(a, "--symlink", to, d)
			}
		case info.IsDir():
			a = append(a, "--ro-bind", d, d)
		}
	}
	for _, e := range etcRead {
		a = append(a, "--ro-bind-try", e, e)
	}
	a = append(a, "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp")
	for _, r := range read {
		// What the system folders hold is there already, read only.
		if !inSystem(r.real) {
			a = append(a, "--ro-bind", r.real, r.real)
		}
		// The link stays a link, so a program finds its files beside
		// where it really is (node resolves a script's folder this way).
		// A link in a system folder, or under a folder that is itself a
		// link (/bin/sh on a merged /usr), is there already.
		if r.link != "" && !inSystem(r.link) {
			if dir, err := filepath.EvalSymlinks(filepath.Dir(r.link)); err == nil && dir == filepath.Dir(r.link) {
				a = append(a, "--symlink", r.real, r.link)
			}
		}
	}
	bind := "--bind"
	if readOnly {
		bind = "--ro-bind"
	}
	a = append(a, bind, copy, copy, "--bind", home, home)
	// The empty root read only: nothing written outside the binds and /tmp.
	return append(a, "--remount-ro", "/", "--chdir", copy, "--cap-drop", "ALL")
}

// systemDirs are the folders bwrapArgs binds read only, or links into
// /usr. Of /etc only etcRead is bound.
var systemDirs = []string{"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32"}

// inSystem says p is in one of systemDirs or etcRead: there already.
func inSystem(p string) bool {
	for _, d := range append(append([]string{}, systemDirs...), etcRead...) {
		if p == d || strings.HasPrefix(p, d+"/") {
			return true
		}
	}
	return false
}

// Start starts cmd in the wall: bwrap, started in netjail's namespace.
// cmd's Path, Args, Env, Dir and SysProcAttr are as for a plain start;
// Start puts bwrap in front.
func (b *Box) Start(cmd *exec.Cmd) error {
	var rest []string
	if len(cmd.Args) > 1 {
		rest = cmd.Args[1:]
	}
	path := cmd.Path
	if !filepath.IsAbs(path) {
		if p, err := filepath.Abs(path); err == nil {
			path = p
		}
	}
	cmd.Args = append(append(append([]string{bwrap}, b.plat.args...), "--", path), rest...)
	cmd.Path = bwrap
	cmd.Err = nil
	return b.jail.Start(cmd)
}

// systemHint is the system setting that refuses the wall, when one does, as
// a sentence to follow the reason: Linux's switches for user namespaces,
// which bubblewrap needs. None when bubblewrap is not there: that is the
// reason then.
func systemHint() (string, string) {
	if _, err := os.Stat(bwrap); err != nil {
		return "", ""
	}
	return userNSHint(os.ReadFile)
}

func userNSHint(read func(string) ([]byte, error)) (word, text string) {
	val := func(p string) string {
		b, err := read(p)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	switch {
	case val("/proc/sys/kernel/unprivileged_userns_clone") == "0":
		return "unprivileged_userns_clone", "; this machine turns unprivileged user namespaces off (kernel.unprivileged_userns_clone is 0)"
	case val("/proc/sys/user/max_user_namespaces") == "0":
		return "max_user_namespaces", "; this machine allows no user namespaces (user.max_user_namespaces is 0)"
	case val("/proc/sys/kernel/apparmor_restrict_unprivileged_userns") == "1":
		return "AppArmor", "; AppArmor allows a user namespace only to a program with a profile " +
			"(kernel.apparmor_restrict_unprivileged_userns is 1, as on Ubuntu 24.04 and later)"
	}
	return "", ""
}
