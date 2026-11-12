package settings

import "golang.org/x/sys/unix"

// cloneFile makes dst a clone of src, which APFS makes without copying the
// bytes.
func cloneFile(src, dst string) error { return unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW) }
