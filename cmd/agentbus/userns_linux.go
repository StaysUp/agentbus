package main

// A service that runs turns (up, runner) and is not root runs in its own
// user and mount namespace, so it can mount a session's overlay copy without
// root. It starts itself again once, as a child in the namespace: the user
// id inside is the person's own (not root: some agents refuse to run as
// root), and the child holds the capabilities a mount needs. Before it
// starts anything, the child clears its ambient capabilities on every
// thread, so the agents it starts run with none. The agents inherit the
// namespace and see every copy at its path; the person's own tools, outside
// it, reach a copy only through /proc/<pid>/root/<path>. The network is the
// host's: no network namespace is made.
//
// Where the namespace cannot be made, or makes no mount (Ubuntu 24.04
// restricts unprivileged user namespaces with AppArmor until a profile
// allows them), the service runs as it is, and overlay falls back to copy
// with that reason.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/staysup/agentbus/isolation"
	"github.com/staysup/agentbus/netjail"
)

// usernsEnv: "off" keeps the service outside a namespace; the child is
// started with usernsChild, and takes it out of its environment at once,
// so no agent inherits it.
const (
	usernsEnv   = "AGENTBUS_USERNS"
	usernsChild = "child"
	usernsOff   = "off"
)

// mountCaps are what mounting an overlay as a user other than root needs
// in the namespace: measured 2026-11-09 on Linux 5.15 and 6.8, where
// CAP_SYS_ADMIN alone fails EPERM, since overlay writes its work folder
// with the mounter's credentials.
var mountCaps = []uintptr{unix.CAP_SYS_ADMIN, unix.CAP_DAC_OVERRIDE, unix.CAP_DAC_READ_SEARCH,
	unix.CAP_FOWNER, unix.CAP_CHOWN, unix.CAP_MKNOD, unix.CAP_FSETID}

// apparmorHint is what to do when AppArmor refuses the namespace: what is
// missing and the one command that adds it; "" when AppArmor is not why.
func apparmorHint() string { return netjail.AppArmorAdvice() }

// userns runs the service in its own namespace where it should: for up and
// runner, on Linux, not root, unless AGENTBUS_USERNS=off. done is true when
// this process has nothing more to do: it was the parent and the child ran
// the service, ending with code. Otherwise the service runs in this
// process: in the namespace (the child), or outside it, with the reason
// given to isolation. main calls it, never run, so a test's process is
// never started again.
func userns(args []string, getenv func(string) string, stderr io.Writer) (code int, done bool) {
	if len(args) == 0 || (args[0] != "up" && args[0] != modeRunner) {
		return 0, false
	}
	logf := log.New(stderr, "agentbus: ", log.LstdFlags).Printf
	if getenv(usernsEnv) == usernsChild {
		os.Unsetenv(usernsEnv)
		home, err := dataFolder(getenv)
		if err != nil {
			home = ""
		}
		return usernsInside(home, logf)
	}
	if os.Geteuid() == 0 {
		return 0, false
	}
	if getenv(usernsEnv) == usernsOff {
		isolation.NoNamespace(usernsEnv + "=" + usernsOff + " keeps it out of one")
		return 0, false
	}
	return usernsStart(logf)
}

// usernsStart starts this program again as a child in a new user and mount
// namespace and, once the child says the namespace works, waits for it and
// passes it the signals this process gets.
func usernsStart(logf func(string, ...any)) (int, bool) {
	outside := func(why string) (int, bool) {
		runtime.UnlockOSThread()
		isolation.NoNamespace(why)
		logf("running outside a user namespace, so a session's copy is not an overlay mount: %s", why)
		return 0, false
	}
	r, w, err := os.Pipe()
	if err != nil {
		return outside("a pipe to the child could not be made")
	}
	cmd := exec.Command("/proc/self/exe", os.Args[1:]...)
	cmd.Args[0] = os.Args[0]
	cmd.Env = append(os.Environ(), usernsEnv+"="+usernsChild)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{w} // fd 3: the child's report
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: os.Getuid(), HostID: os.Getuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: os.Getgid(), HostID: os.Getgid(), Size: 1}},
		GidMappingsEnableSetgroups: false,
		AmbientCaps:                mountCaps,
		// The child stops when this process ends: Pdeathsig follows the
		// thread that started it, which stays this goroutine's.
		Pdeathsig: syscall.SIGTERM,
	}
	runtime.LockOSThread()
	sig := make(chan os.Signal, 4)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sig)
	err = cmd.Start()
	w.Close()
	if err != nil {
		r.Close()
		why := "the namespace could not be made"
		var errno syscall.Errno
		if errors.As(err, &errno) {
			why += ": " + errno.Error()
			if errno == syscall.EACCES || errno == syscall.EPERM {
				why += apparmorHint()
			}
		}
		return outside(why)
	}
	line, _ := bufio.NewReader(r).ReadString('\n')
	r.Close()
	if line != "ok\n" {
		err := cmd.Wait()
		if line == "" || err != nil {
			// The child ended before it could say: a signal, or a crash.
			return exitCode(cmd), true
		}
		return outside(strings.TrimSuffix(line, "\n"))
	}
	go func() {
		for s := range sig {
			cmd.Process.Signal(s)
		}
	}()
	cmd.Wait()
	return exitCode(cmd), true
}

func exitCode(cmd *exec.Cmd) int {
	if cmd.ProcessState == nil {
		return 1
	}
	if c := cmd.ProcessState.ExitCode(); c >= 0 {
		return c
	}
	return 1 // ended by a signal
}

// usernsInside readies the namespace in the child and tells the parent on
// fd 3: "ok", or why not, after which the child ends and the parent runs
// the service outside.
func usernsInside(home string, logf func(string, ...any)) (int, bool) {
	report := os.NewFile(3, "userns-report")
	refuse := func(why string) (int, bool) {
		fmt.Fprintln(report, why)
		report.Close()
		return 0, true
	}
	// Mounts made here never reach the host; a host mount made later, such
	// as a drive plugged in, still reaches here.
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_SLAVE, ""); err != nil {
		why := "the namespace was made but cannot mount (make-rslave: " + errnoText(err) + ")"
		if errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM) {
			why += apparmorHint()
		}
		return refuse(why)
	}
	// The ambient capabilities let this process mount; the agents it starts
	// must not inherit them. Every thread holds its own set.
	if err := clearAmbient(); err != nil {
		return refuse("the capabilities could not be cleared before agents start: " + err.Error())
	}
	if home == "" {
		return refuse("the data folder could not be found")
	}
	workdir := filepath.Join(home, copiesDir)
	if err := os.MkdirAll(workdir, 0o700); err != nil {
		return refuse("the workdir could not be made")
	}
	if why := isolation.OverlayWhy(workdir); why != "" {
		return refuse("in the namespace, " + why)
	}
	fmt.Fprintln(report, "ok")
	report.Close()
	logf("running in its own user and mount namespace as uid %d, so a session's copy is an overlay mount; "+
		"your own tools reach a copy at /proc/%d/root/<its path>", os.Getuid(), os.Getpid())
	return 0, false
}

// clearAmbient clears the ambient capabilities on every thread, then reads
// every thread's set back. A build with cgo cannot clear them from Go
// (ENOTSUP): userns_cgo_linux.go cleared them before Go started, and the
// read says whether it did.
func clearAmbient() error {
	_, _, e := syscall.AllThreadsSyscall6(unix.SYS_PRCTL, unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0, 0)
	if e != 0 && e != syscall.ENOTSUP {
		return e
	}
	tasks, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return err
	}
	for _, t := range tasks {
		b, err := os.ReadFile(filepath.Join("/proc/self/task", t.Name(), "status"))
		if errors.Is(err, os.ErrNotExist) {
			continue // the thread ended
		}
		if err != nil {
			return err
		}
		if amb, ok := capAmb(string(b)); !ok || amb != 0 {
			return fmt.Errorf("thread %s still holds ambient capabilities %x", t.Name(), amb)
		}
	}
	return nil
}

// capAmb reads the CapAmb line of a /proc status file.
func capAmb(status string) (uint64, bool) {
	for _, line := range strings.Split(status, "\n") {
		if v, ok := strings.CutPrefix(line, "CapAmb:"); ok {
			n, err := strconv.ParseUint(strings.TrimSpace(v), 16, 64)
			return n, err == nil
		}
	}
	return 0, false
}

func errnoText(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno.Error()
	}
	return "failed"
}
