package main

import "golang.org/x/sys/unix"

// copyWay: on APFS, Copy makes a session's copy in one clonefile(2), for a
// folder on the data folder's disk (isolation/clone_darwin.go); anywhere
// else file by file.
func copyWay(home string) finding {
	var st unix.Statfs_t
	if unix.Statfs(existingDir(home), &st) != nil {
		return finding{statusNote, "the data folder's disk could not be read, so how Copy copies a folder is not known"}
	}
	if fs := unix.ByteSliceToString(st.Fstypename[:]); fs != "apfs" {
		return finding{statusNote, "the data folder's disk is " + fs + ", not APFS, so Copy copies a folder file by file: " +
			"set AGENTBUS_HOME to a folder on an APFS disk for a clone in one step"}
	}
	return finding{statusOK, "Copy clones a folder on the data folder's disk in one step (APFS)"}
}

// appArmorFinding: macOS has no AppArmor.
func appArmorFinding() *finding { return nil }

func freeSpace(dir string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}
