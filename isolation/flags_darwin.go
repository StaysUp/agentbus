package isolation

import (
	"encoding/binary"
	"syscall"

	"golang.org/x/sys/unix"
)

// userLocks are the flags Finder's Locked and chflags uchg set (and uappnd):
// a file or folder with one cannot be removed or renamed, and clonefile keeps
// them on the copy.
const userLocks = unix.UF_IMMUTABLE | unix.UF_APPEND

// unlock clears the user's lock flags on p itself, a file or folder of a copy.
// It sets them by path with setattrlist and FSOPT_NOFOLLOW, never opening p,
// so a folder its mode keeps closed (chmod 0) is unlocked too, and a link put
// in p's place is changed itself, never what it points to. Nothing happens
// where p has none, or is not there.
func unlock(p string) {
	var st syscall.Stat_t
	if syscall.Lstat(p, &st) != nil || st.Flags&userLocks == 0 {
		return
	}
	al := unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Commonattr: unix.ATTR_CMN_FLAGS}
	buf := make([]byte, 4)
	binary.NativeEndian.PutUint32(buf, st.Flags&^userLocks)
	_ = unix.Setattrlist(p, &al, buf, unix.FSOPT_NOFOLLOW)
}
