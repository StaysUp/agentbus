package isolation

import (
	"io/fs"

	"golang.org/x/sys/unix"
)

// cloneTree copies the folder src to dst, which must not exist, in one
// clonefile(2) call where the disk is APFS: the copy shares every block
// with src until either side writes it. Anywhere else, file by file.
func cloneTree(src, dst string) (string, error) {
	if err := unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW); err == nil {
		return MethodClone, nil
	}
	return copyTree(src, dst)
}

// cloneFile copies the file src to dst, which must not exist, by
// clonefile(2), or byte by byte where the disk cannot clone.
func cloneFile(src, dst string, perm fs.FileMode) error {
	if unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW) == nil {
		return nil
	}
	_, err := copyFile(src, dst, perm, false)
	return err
}
