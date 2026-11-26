//go:build !linux && !darwin

package workspace

import "io/fs"

// changeTime is the modification time where Go gives no change time (Windows
// has none in its file info), so a file is judged by its modification time.
func changeTime(info fs.FileInfo) int64 { return info.ModTime().UnixNano() }
