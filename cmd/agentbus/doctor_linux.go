package main

import (
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/staysup/agentbus/isolation"
	"github.com/staysup/agentbus/netjail"
)

// copyWay: Copy mounts an overlay for each session where this machine can
// mount one, else it copies the folder, file by file, each file a reflink
// where the disk has them (isolation.For). The overlay is tried in a folder
// of its own beside the data folder, removed after, on the disk a session's
// copy would be on.
func copyWay(home string) finding {
	why := "nothing beside the data folder can be written to try one in"
	if dir, err := os.MkdirTemp(existingDir(home), ".agentbus-doctor-"); err == nil {
		why = isolation.OverlayWhy(dir)
		_ = os.RemoveAll(dir)
	}
	if why == "" {
		return finding{statusOK, "Copy mounts an overlay for each session"}
	}
	// AppArmor's advice, when the namespace was refused for it, ends the
	// reason; the AppArmor line gives it once.
	if advice := netjail.AppArmorAdvice(); advice != "" {
		why = strings.TrimSuffix(why, advice)
	}
	var st unix.Statfs_t
	then := "a plain copy of each file"
	if unix.Statfs(existingDir(home), &st) == nil {
		switch st.Type {
		case unix.BTRFS_SUPER_MAGIC:
			then = "a reflink of each file (btrfs)"
		case unix.XFS_SUPER_MAGIC:
			then = "a reflink of each file where xfs was made with them, else a plain copy"
		}
	}
	return finding{statusNote, "Copy cannot mount an overlay here, so it makes " + then + ": " + why}
}

// appArmorFinding: on Ubuntu 24.04 and later AppArmor gives user namespaces
// only to a program with a profile, and Copy's walls need one.
func appArmorFinding() *finding {
	advice := netjail.AppArmorAdvice()
	if advice == "" {
		return &finding{statusOK, "AppArmor leaves agentbus its user namespaces"}
	}
	return &finding{statusFail, strings.TrimPrefix(advice, "; ")}
}

func freeSpace(dir string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}
