package isolation

import "io/fs"

// cloneTree copies the folder src to dst file by file, each one a reflink
// (FICLONE) where the disk has them, such as btrfs and xfs, and a plain copy
// where it does not, such as ext4.
func cloneTree(src, dst string) (string, error) { return copyTree(src, dst) }

// cloneFile copies the file src to dst, which must not exist, as a reflink
// where the disk has them.
func cloneFile(src, dst string, perm fs.FileMode) error {
	_, err := copyFile(src, dst, perm, true)
	return err
}
