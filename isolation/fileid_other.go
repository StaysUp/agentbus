//go:build !darwin && !linux

package isolation

import "io/fs"

// fileID is Linux and macOS only.
func fileID(info fs.FileInfo) (id [2]uint64, names uint64, ok bool) { return id, 0, false }
