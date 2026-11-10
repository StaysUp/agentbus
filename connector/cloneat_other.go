//go:build !darwin && !linux

package connector

import "os"

// cloneInto is a clone where the disk has them: none here.
func cloneInto(rt *os.Root, tmp, src string) bool { return false }
