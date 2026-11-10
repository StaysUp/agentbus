package credentials

import (
	"bytes"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	ioctlGetTermios = unix.TIOCGETA
	ioctlSetTermios = unix.TIOCSETA
)

// openPty opens a new pseudo-terminal: its controlling end and the terminal.
func openPty() (m, tty *os.File, err error) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	m = os.NewFile(uintptr(fd), "/dev/ptmx")
	var name [128]byte
	for _, req := range []struct {
		op  uintptr
		arg uintptr
	}{{unix.TIOCPTYGRANT, 0}, {unix.TIOCPTYUNLK, 0}, {unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))}} {
		if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req.op, req.arg); e != 0 {
			m.Close()
			return nil, nil, e
		}
	}
	n := bytes.IndexByte(name[:], 0)
	if n < 0 {
		n = len(name)
	}
	path := string(name[:n])
	tty, err = os.OpenFile(path, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		return nil, nil, err
	}
	return m, tty, nil
}
