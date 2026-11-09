package isolation

import (
	"os"

	"golang.org/x/sys/unix"
)

// reflink makes dst share src's blocks (FICLONE). False where the disk
// cannot, or src and dst are on two disks.
func reflink(dst, src *os.File) bool {
	return unix.IoctlFileClone(int(dst.Fd()), int(src.Fd())) == nil
}
