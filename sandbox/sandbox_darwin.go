package sandbox

// macOS: Seatbelt. The profile denies everything, then allows what a
// session needs, measured on macOS 13.0.1 with claude 2.1.321 and codex
// 0.164.0 (research/2026-11-11-session-sandbox.md in the notes). Paths go
// in as parameters (-D), never into the profile's text, so no path can
// change a rule. sandbox-exec is marked deprecated by Apple and still works
// on macOS 13; its path is fixed, never looked up on PATH.

import (
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/staysup/agentbus/netjail"
)

// Supported: macOS has Seatbelt.
const Supported = true

// sandboxExec is Seatbelt's front end.
const sandboxExec = "/usr/bin/sandbox-exec"

type platform struct {
	profile string
	params  []string // -D KEY=VALUE, two arguments each
}

// systemRead are the folders every session reads: the system, the
// developer tools git needs (the Xcode shim), Homebrew's programs, and the
// few files of /private/var the system libraries read. Not /Users, not
// /Volumes, not /private/tmp, not /private/var/folders.
var systemRead = []string{
	"/usr", "/bin", "/sbin", "/System", "/Library/Apple", "/Library/Preferences",
	"/Library/Developer/CommandLineTools", "/Applications/Xcode.app/Contents", "/opt/homebrew",
	"/private/etc", "/private/var/db/timezone", "/private/var/db/dyld", "/private/var/select", "/dev",
}

// machServices are the system services a session may look up:
// notifications, the system log, certificate trust, network configuration,
// and the user lookup codex needs to start (getpwuid). Not the keychain.
var machServices = []string{
	"com.apple.system.notification_center", "com.apple.system.logger", "com.apple.trustd.agent",
	"com.apple.SystemConfiguration.configd", "com.apple.system.opendirectoryd.libinfo",
}

// build writes the profile for b and starts its proxy.
func (b *Box) build(net netjail.Spec) error {
	if info, err := os.Stat(sandboxExec); err != nil || info.IsDir() {
		return &Error{Why: sandboxExec + " is not on this Mac"}
	}
	jail, err := netjail.NewHost(net)
	if err != nil {
		return &Error{Why: "its network proxy could not be started"}
	}
	ports, ok := loopbackPorts(append(append([]string{}, net.Forward...), jail.Proxy()))
	if !ok {
		jail.Close()
		return &Error{Why: "an address it should reach is not on this machine's loopback"}
	}
	b.jail = jail
	b.plat = profile(b.copy, b.readOnly, b.home, b.read, ports)
	check := exec.Command(sandboxExec, b.args("/usr/bin/true")...)
	check.Dir = b.copy
	if !runCheck(check) {
		jail.Close()
		return &Error{Why: "sandbox-exec refused its profile"}
	}
	return nil
}

// profile is the Seatbelt profile and its parameters.
func profile(copy string, readOnly bool, home string, read []path, ports []int) platform {
	var p platform
	param := func(v string) string {
		k := "P" + strconv.Itoa(len(p.params)/2)
		p.params = append(p.params, "-D", k+"="+v)
		return `(param "` + k + `")`
	}
	var s strings.Builder
	s.WriteString("(version 1)\n(deny default)\n")
	// Its own processes only: it cannot signal or inspect the runner or
	// another session.
	s.WriteString("(allow process-exec process-fork)\n")
	s.WriteString("(allow signal (target same-sandbox))\n(allow process-info* (target same-sandbox))\n")
	s.WriteString("(allow sysctl-read)\n")
	// Metadata everywhere: a session can learn that a file exists, not
	// read it. Only the ancestors of what it may read would close that.
	s.WriteString("(allow file-read-metadata)\n")
	s.WriteString(`(allow file-read* (literal "/") (literal "/var") (literal "/tmp") (literal "/etc")`)
	for _, d := range systemRead {
		s.WriteString(" (subpath " + param(d) + ")")
	}
	s.WriteString(")\n")
	s.WriteString(`(allow file-write-data file-ioctl (literal "/dev/null") (literal "/dev/zero") (literal "/dev/dtracehelper")` +
		` (regex #"^/dev/tty") (regex #"^/dev/fd/"))` + "\n")
	if readOnly {
		s.WriteString("(allow file-read* (subpath " + param(copy) + "))\n")
		s.WriteString("(allow file-read* file-write* (subpath " + param(home) + "))\n")
	} else {
		s.WriteString("(allow file-read* file-write* (subpath " + param(copy) + ") (subpath " + param(home) + "))\n")
	}
	if len(read) > 0 {
		s.WriteString("(allow file-read*")
		for _, r := range read {
			if r.dir {
				s.WriteString(" (subpath " + param(r.real) + ")")
			} else {
				s.WriteString(" (literal " + param(r.real) + ")")
			}
			if r.link != "" {
				s.WriteString(" (literal " + param(r.link) + ")")
			}
		}
		s.WriteString(")\n")
	}
	s.WriteString("(allow mach-lookup")
	for _, m := range machServices {
		s.WriteString(` (global-name "` + m + `")`)
	}
	s.WriteString(")\n")
	s.WriteString(`(allow ipc-posix-shm-read* (ipc-posix-name-prefix "apple.shm.notification_center"))` + "\n")
	// The network: only the forwarded loopback ports and the proxy.
	s.WriteString("(allow network-outbound")
	for _, port := range ports {
		s.WriteString(` (remote ip "localhost:` + strconv.Itoa(port) + `")`)
	}
	s.WriteString(")\n")
	p.profile = s.String()
	return p
}

// args are sandbox-exec's arguments to run path and its args in the wall.
func (b *Box) args(path string, args ...string) []string {
	out := append([]string{"-p", b.plat.profile}, b.plat.params...)
	return append(append(out, "--", path), args...)
}

// Start starts cmd in the wall. cmd's Path, Args, Env, Dir and SysProcAttr
// are as for a plain start; Start puts sandbox-exec in front.
func (b *Box) Start(cmd *exec.Cmd) error {
	var rest []string
	if len(cmd.Args) > 1 {
		rest = cmd.Args[1:]
	}
	cmd.Args = append([]string{sandboxExec}, b.args(cmd.Path, rest...)...)
	cmd.Path = sandboxExec
	cmd.Err = nil
	return cmd.Start()
}

// loopbackPorts are the ports of addrs, each a loopback ip:port; ok is
// false when one is not.
func loopbackPorts(addrs []string) (ports []int, ok bool) {
	for _, a := range addrs {
		h, p, err := net.SplitHostPort(a)
		if err != nil {
			return nil, false
		}
		ip := net.ParseIP(h)
		port, err := strconv.Atoi(p)
		if ip == nil || !ip.IsLoopback() || err != nil || port <= 0 || port > 65535 {
			return nil, false
		}
		ports = append(ports, port)
	}
	return ports, true
}
