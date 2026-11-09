package isolation

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// cloneInto makes tmp, a new file of rt, a clone of the file src
// (clonefileat(2)): no bytes are copied. The folder is opened through rt,
// so no symbolic link can take the write outside it. False where the disk
// cannot clone; nothing is written then.
func cloneInto(rt *os.Root, tmp, src string) bool {
	d, err := rt.Open(filepath.Dir(tmp))
	if err != nil {
		return false
	}
	defer d.Close()
	return unix.Clonefileat(unix.AT_FDCWD, src, int(d.Fd()), filepath.Base(tmp), unix.CLONE_NOFOLLOW) == nil
}
