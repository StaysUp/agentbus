//go:build !darwin && !linux

package api

// readProcs cannot read the process table on this platform, so a run's
// tree is not tracked and only the child's process group is signalled.
func readProcs(minStart int64) ([]proc, error) { return nil, errNoProcTable }
