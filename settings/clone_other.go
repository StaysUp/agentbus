//go:build !darwin

package settings

import "errors"

// cloneFile is not made here: the copy copies the bytes.
func cloneFile(src, dst string) error { return errors.New("no clone") }
