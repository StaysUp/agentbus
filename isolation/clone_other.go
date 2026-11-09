//go:build !darwin && !linux

package isolation

import "io/fs"

// cloneTree copies the folder src to dst file by file.
func cloneTree(src, dst string) (string, error) { return copyTree(src, dst) }

// cloneFile copies the file src to dst, which must not exist.
func cloneFile(src, dst string, perm fs.FileMode) error {
	_, err := copyFile(src, dst, perm, false)
	return err
}
