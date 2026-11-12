package netjail

// Ubuntu 23.10 and later (kernel.apparmor_restrict_unprivileged_userns=1)
// let a program that runs unconfined make a user namespace, but with no
// capabilities in it: the kernel moves it to the unprivileged_userns
// profile, which denies them. So loopback cannot be brought up and
// bubblewrap cannot mount. A program with a profile that allows userns is
// not moved. packaging/apparmor/agentbus is that profile for agentbus, the
// way Ubuntu ships one for chrome and flatpak. The programs agentbus
// starts, bubblewrap among them, keep its profile: a profile in unconfined
// mode keeps its label across an exec to a program that has no profile of
// its own.

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// installScript is the install script that writes the profile.
const installScript = "https://raw.githubusercontent.com/staysup/agentbus/main/install.sh"

// AppArmorAdvice is what is missing and the one command that adds it, when
// AppArmor's user namespace rule is what refuses this program; "" when it
// is not. It begins with "; " to follow a reason.
func AppArmorAdvice() string { return appArmorAdvice(readAppArmor()) }

// appArmor is what the advice is made from.
type appArmor struct {
	restricted bool   // kernel.apparmor_restrict_unprivileged_userns is 1
	label      string // this process's AppArmor label, such as "unconfined"
	bin        string // this program, its links resolved; "" when unknown
	open       string // bin, or a folder above it, that a user other than root may change; "" when none
}

func readAppArmor() appArmor {
	var a appArmor
	b, err := os.ReadFile("/proc/sys/kernel/apparmor_restrict_unprivileged_userns")
	a.restricted = err == nil && strings.TrimSpace(string(b)) == "1"
	if !a.restricted {
		return a
	}
	for _, p := range []string{"/proc/self/attr/apparmor/current", "/proc/self/attr/current"} {
		if b, err := os.ReadFile(p); err == nil {
			a.label = strings.TrimSpace(strings.TrimRight(string(b), "\x00"))
			break
		}
	}
	if p, err := os.Executable(); err == nil {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			a.bin = r
			a.open = openToOthers(r, os.Lstat)
		}
	}
	return a
}

// openToOthers is the first of p and the folders above it that a user other
// than root may change: one root does not own, or one its group or others
// may write. "" when there is none, so that only root can put another
// program at p. A profile names a path: at any other path, whoever can
// write there can give the profile's user namespaces to any program.
func openToOthers(p string, stat func(string) (os.FileInfo, error)) string {
	for d := p; ; d = filepath.Dir(d) {
		info, err := stat(d)
		if err != nil || !ownedByRoot(info) || info.Mode().Perm()&0o022 != 0 {
			return d
		}
		if d == filepath.Dir(d) {
			return ""
		}
	}
}

func ownedByRoot(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Uid == 0
}

func appArmorAdvice(a appArmor) string {
	if !a.restricted {
		return ""
	}
	// A profile of its own (agentbus's, or another an administrator gave
	// it): AppArmor's user namespace rule is not what refuses it.
	if a.label != "unconfined" && !strings.HasPrefix(a.label, "unprivileged_userns") && a.label != "" {
		return ""
	}
	why := "; AppArmor allows a user namespace with capabilities only to a program with a profile " +
		"(kernel.apparmor_restrict_unprivileged_userns is 1, as on Ubuntu 24.04), and agentbus runs without one"
	switch {
	case a.bin == "":
		return why + "; install agentbus with its profile, once: curl -fsSL " + installScript +
			" | sudo AGENTBUS_INSTALL_DIR=/usr/local/bin sh"
	case a.open != "":
		return why + ". A profile is given only to a binary that only root can change, and " + a.open +
			" can be changed by another user; install agentbus with its profile, once: curl -fsSL " + installScript +
			" | sudo AGENTBUS_INSTALL_DIR=/usr/local/bin sh, then run /usr/local/bin/agentbus"
	}
	return why + "; add it, once: curl -fsSL " + installScript + " | sudo sh -s apparmor " + shellQuote(a.bin)
}

// shellQuote is s as one word to a POSIX shell: as it is when it holds
// nothing the shell reads, otherwise in single quotes.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
