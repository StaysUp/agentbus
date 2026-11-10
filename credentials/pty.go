//go:build linux || darwin

package credentials

import (
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// A login run for the cockpit gets a pseudo-terminal, not pipes: claude
// setup-token (2.1.x, seen 2026-11-10) reads the pasted code from a terminal
// only. Given a pipe it took two lines of input and never ended.

// ptyColumns is the terminal's width: wide, so a login's screen does not
// wrap the token across lines.
const ptyColumns = 500

// startInPty starts cmd with a new terminal as its stdin, stdout and stderr,
// in its own session with that terminal as its controlling one. It returns
// the terminal's other end: what cmd prints is read from it, and what is
// written to it is cmd's typing. The terminal does not echo, so a line
// written is not read back.
func startInPty(cmd *exec.Cmd) (*os.File, error) {
	m, tty, err := openPty()
	if err != nil {
		return nil, err
	}
	defer tty.Close() // cmd holds its own copy
	fd := int(tty.Fd())
	if err := unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Row: 50, Col: ptyColumns}); err != nil {
		m.Close()
		return nil, err
	}
	t, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		m.Close()
		return nil, err
	}
	t.Lflag &^= unix.ECHO | unix.ECHONL
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, t); err != nil {
		m.Close()
		return nil, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		m.Close()
		return nil, err
	}
	return m, nil
}

// stopGroup stops cmd and every process it started: a login runs in its
// own session, so its process group is its own.
func stopGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
