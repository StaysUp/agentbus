//go:build !darwin && !linux

package isolation

import (
	"context"
	"io/fs"
)

// cloneTree copies the folder src to dst file by file.
func cloneTree(ctx context.Context, src, dst string) (string, error) { return copyTree(ctx, src, dst) }

// cloneFile copies the file src to dst, which must not exist.
func cloneFile(src, dst string, perm fs.FileMode) error {
	_, err := copyFile(context.Background(), src, dst, perm, false)
	return err
}
