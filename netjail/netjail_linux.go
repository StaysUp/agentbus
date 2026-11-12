package netjail

// How an agent is started closed off. The runner starts this same program
// as a helper, born in a new user and network namespace (CLONE_NEWUSER |
// CLONE_NEWNET), mapped to the runner's own uid. In its own user namespace
// the helper may bring loopback up and listen on it, which the runner
// itself could not without capabilities. It binds every address the agent
// needs, sends the listening sockets back over a socket pair, and then
// becomes the agent (execve, so the agent has the helper's pid). The
// runner serves those sockets from outside the namespace.
//
// The helper runs in init, before main, so it works in any program that
// imports this package, tests included. It never returns there.
//
// The agent keeps the runner's mount namespace and files. Its user
// namespace is a child of the runner's, so even an agent that is root
// inside cannot join the runner's network namespace again.

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// The helper's own variables. It takes them out of its environment before
// it becomes the agent.
const (
	envFD     = "AGENTBUS_NETJAIL_FD"     // the socket to report on
	envListen = "AGENTBUS_NETJAIL_LISTEN" // ip:port, comma separated
	envExec   = "AGENTBUS_NETJAIL_EXEC"   // the agent's program; empty for Check's probe
)

// Supported: Linux can close an agent off.
const Supported = true

// readyTimeout bounds how long the runner waits for the helper's sockets.
const readyTimeout = 10 * time.Second

func init() {
	v, ok := os.LookupEnv(envFD)
	if !ok {
		return
	}
	helper(v)
}

// helper readies the namespace, reports, and becomes the agent. It never
// returns.
func helper(fdText string) {
	listen, path := os.Getenv(envListen), os.Getenv(envExec)
	for _, k := range []string{envFD, envListen, envExec} {
		os.Unsetenv(k)
	}
	fd, err := strconv.Atoi(fdText)
	if err != nil {
		os.Exit(126)
	}
	fail := func(why string) {
		unix.Sendmsg(fd, []byte("refused: "+why), nil, nil, 0)
		os.Exit(126)
	}
	if err := loopbackUp(); err != nil {
		fail("loopback could not be brought up in the agent's namespace (" + errnoText(err) + ")" + hint(err))
	}
	var fds []int
	if listen != "" {
		for _, addr := range strings.Split(listen, ",") {
			l, err := listenOn(addr)
			if err != nil {
				fail(addr + " could not be listened on in the agent's namespace (" + errnoText(err) + ")")
			}
			fds = append(fds, l)
		}
	}
	if err := unix.Sendmsg(fd, []byte("ok"), unix.UnixRights(fds...), nil, 0); err != nil {
		os.Exit(126)
	}
	for _, l := range fds {
		unix.Close(l)
	}
	unix.Close(fd)
	if path == "" {
		os.Exit(0)
	}
	// The agent gets no capability: the ambient one goes on the thread that
	// execs, which is the only thread the new program keeps.
	runtime.LockOSThread()
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		fmt.Fprintln(os.Stderr, "agentbus: the agent's capabilities could not be cleared")
		os.Exit(126)
	}
	syscall.Exec(path, os.Args, os.Environ())
	fmt.Fprintln(os.Stderr, "agentbus: the agent could not be started")
	os.Exit(127)
}

// loopbackUp brings lo up, which gives it 127.0.0.1 and ::1.
func loopbackUp() error {
	s, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(s)
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(s, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	return unix.IoctlIfreq(s, unix.SIOCSIFFLAGS, ifr)
}

// listenOn is a listening TCP socket on addr, an ip:port.
func listenOn(addr string) (int, error) {
	ap, err := netipAddrPort(addr)
	if err != nil {
		return -1, err
	}
	family, sa := unix.AF_INET, unix.Sockaddr(nil)
	if ip4 := ap.ip.To4(); ip4 != nil {
		s := &unix.SockaddrInet4{Port: ap.port}
		copy(s.Addr[:], ip4)
		sa = s
	} else {
		family = unix.AF_INET6
		s := &unix.SockaddrInet6{Port: ap.port}
		copy(s.Addr[:], ap.ip.To16())
		sa = s
	}
	s, err := unix.Socket(family, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	unix.SetsockoptInt(s, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
	if err := unix.Bind(s, sa); err != nil {
		unix.Close(s)
		return -1, err
	}
	if err := unix.Listen(s, 128); err != nil {
		unix.Close(s)
		return -1, err
	}
	return s, nil
}

type ipPort struct {
	ip   net.IP
	port int
}

func netipAddrPort(addr string) (ipPort, error) {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return ipPort{}, unix.EINVAL
	}
	ip := net.ParseIP(h)
	port, err := strconv.Atoi(p)
	if ip == nil || err != nil || port <= 0 || port > 65535 {
		return ipPort{}, unix.EINVAL
	}
	return ipPort{ip, port}, nil
}

// Start starts cmd closed off: through the helper, in a new user and
// network namespace, and serves what the agent may reach. cmd's Path, Args,
// Env and SysProcAttr are as for a plain start; Start adds to them. On an
// Error the helper has ended and nothing runs.
func (j *Jail) Start(cmd *exec.Cmd) error {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return &Error{Why: "a socket pair could not be made"}
	}
	mine := pair[0]
	defer unix.Close(mine)
	theirs := os.NewFile(uintptr(pair[1]), "netjail")
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	cmd.Env = append(append([]string{}, env...),
		envFD+"="+strconv.Itoa(3+len(cmd.ExtraFiles)), envListen+"="+strings.Join(j.listen(), ","), envExec+"="+cmd.Path)
	cmd.ExtraFiles = append(cmd.ExtraFiles, theirs)
	cmd.Path = "/proc/self/exe"
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	sys := cmd.SysProcAttr
	sys.Cloneflags |= syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET
	sys.UidMappings = []syscall.SysProcIDMap{{ContainerID: os.Getuid(), HostID: os.Getuid(), Size: 1}}
	sys.GidMappings = []syscall.SysProcIDMap{{ContainerID: os.Getgid(), HostID: os.Getgid(), Size: 1}}
	sys.GidMappingsEnableSetgroups = false
	// The helper is not root inside when the runner is not: it keeps these
	// across its exec, to bring loopback up and to listen on a port below
	// 1024, and clears them before it becomes the agent.
	sys.AmbientCaps = []uintptr{unix.CAP_NET_ADMIN, unix.CAP_NET_BIND_SERVICE}
	err = cmd.Start()
	theirs.Close()
	if err != nil {
		why := "its namespace could not be made"
		var errno syscall.Errno
		if errors.As(err, &errno) {
			why += " (" + errno.Error() + ")" + hint(errno)
		}
		return &Error{Why: why}
	}
	lns, why := receive(mine, len(j.listen()))
	if why != "" {
		cmd.Process.Kill()
		cmd.Wait()
		return &Error{Why: why}
	}
	j.serve(lns)
	return nil
}

// receive reads the helper's report: n listening sockets, or why not.
func receive(fd, n int) ([]net.Listener, string) {
	tv := unix.NsecToTimeval(readyTimeout.Nanoseconds())
	unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)
	buf := make([]byte, 1024)
	oob := make([]byte, unix.CmsgSpace(4*n))
	// Close-on-exec as they arrive: a program the runner starts meanwhile,
	// such as git, must not inherit them.
	m, oobn, _, _, err := unix.Recvmsg(fd, buf, oob, unix.MSG_CMSG_CLOEXEC)
	if err != nil {
		return nil, "the agent's namespace did not get ready in time"
	}
	msg := string(buf[:m])
	var fds []int
	if oobn > 0 {
		if cms, err := unix.ParseSocketControlMessage(oob[:oobn]); err == nil {
			for i := range cms {
				if got, err := unix.ParseUnixRights(&cms[i]); err == nil {
					fds = append(fds, got...)
				}
			}
		}
	}
	closeAll := func() {
		for _, f := range fds {
			unix.Close(f)
		}
	}
	switch {
	case m == 0:
		closeAll()
		return nil, "the helper that readies the agent's namespace ended before it was ready"
	case strings.HasPrefix(msg, "refused: "):
		closeAll()
		return nil, strings.TrimPrefix(msg, "refused: ")
	case msg != "ok" || len(fds) != n:
		closeAll()
		return nil, "the helper that readies the agent's namespace sent an answer it should not"
	}
	lns := make([]net.Listener, 0, n)
	for i, f := range fds {
		file := os.NewFile(uintptr(f), "netjail-listener")
		l, err := net.FileListener(file)
		file.Close()
		if err != nil {
			for _, l := range lns {
				l.Close()
			}
			for _, rest := range fds[i+1:] {
				unix.Close(rest)
			}
			return nil, "a listening socket from the agent's namespace could not be used"
		}
		lns = append(lns, l)
	}
	return lns, ""
}

// hint is what to do when Linux refuses a user namespace its capabilities.
func hint(err error) string {
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
		return AppArmorAdvice()
	}
	if errors.Is(err, unix.ENOSPC) || errors.Is(err, unix.EUSERS) {
		return "; the limit on user namespaces is reached (user.max_user_namespaces)"
	}
	return ""
}

func errnoText(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno.Error()
	}
	return "failed"
}

var check struct {
	once sync.Once
	err  error
}

// Check says whether this machine can close an agent off, by readying one
// namespace with nothing in it. It runs once per process; the answer is
// kept.
func Check() error {
	check.once.Do(func() {
		j := New(Spec{})
		// No program: the helper reports and ends.
		cmd := &exec.Cmd{Args: []string{"agentbus-netjail-check"}}
		if err := j.Start(cmd); err != nil {
			check.err = err
			return
		}
		cmd.Wait()
		j.Close()
	})
	return check.err
}
