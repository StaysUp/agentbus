//go:build darwin || linux

package isolation

import (
	"io/fs"
	"syscall"
)

// fileID is the disk and inode of the file info describes, and how many
// names it has. False where the system does not say.
func fileID(info fs.FileInfo) (id [2]uint64, names uint64, ok bool) {
	st, _ := info.Sys().(*syscall.Stat_t)
	if st == nil {
		return id, 0, false
	}
	return [2]uint64{uint64(st.Dev), uint64(st.Ino)}, uint64(st.Nlink), true
}
