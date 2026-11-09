//go:build !linux

package isolation

import "os"

// reflink is Linux only.
func reflink(dst, src *os.File) bool { return false }
