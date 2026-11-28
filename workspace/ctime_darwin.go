package workspace

import (
	"io/fs"
	"syscall"
)

// changeTime is when info's file last changed, as the system keeps it: a
// write, a rename, a new link and a change of mode set it to now, and no
// program can set it back.
func changeTime(info fs.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Ctimespec.Nano()
	}
	return info.ModTime().UnixNano()
}
