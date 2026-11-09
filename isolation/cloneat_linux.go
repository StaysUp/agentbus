package isolation

import (
	"os"

	"golang.org/x/sys/unix"
)

// cloneInto makes tmp, a new file of rt, a reflink of the file src
// (FICLONE): no bytes are copied. False where the disk cannot, or src is
// on another disk; nothing is left then.
func cloneInto(rt *os.Root, tmp, src string) bool {
	in, err := os.Open(src)
	if err != nil {
		return false
	}
	defer in.Close()
	out, err := rt.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false
	}
	ok := unix.IoctlFileClone(int(out.Fd()), int(in.Fd())) == nil
	if err := out.Close(); err != nil {
		ok = false
	}
	if !ok {
		rt.Remove(tmp)
	}
	return ok
}
