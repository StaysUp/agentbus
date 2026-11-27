package isolation

import (
	"context"
	"io/fs"

	"golang.org/x/sys/unix"
)

// cloneTree copies the folder src to dst, which must not exist, in one
// clonefile(2) call where the disk is APFS: the copy shares every block
// with src until either side writes it; that one call is quick, and a stop
// waits for it. Anywhere else, file by file, until ctx ends.
func cloneTree(ctx context.Context, src, dst string) (string, error) {
	if err := unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW); err == nil {
		return MethodClone, nil
	}
	return copyTree(ctx, src, dst)
}

// cloneFile copies the file src to dst, which must not exist, by
// clonefile(2), or byte by byte where the disk cannot clone.
func cloneFile(src, dst string, perm fs.FileMode) error {
	if unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW) == nil {
		return nil
	}
	_, err := copyFile(context.Background(), src, dst, perm, false)
	return err
}
