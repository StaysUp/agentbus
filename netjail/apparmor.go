package netjail

import "strings"

// ProfilePathChars are the characters a path an AppArmor profile names may
// hold: AppArmor reads a profile's path as a pattern, and these are never
// part of one, so the path names one file. install.sh's plain_path tests the
// same set; cmd/agentbus's TestProfilePathShared runs both on the same paths.
// Spelled out, not as ranges: a shell's A-Z under a UTF-8 locale may take
// in letters such as ü.
const ProfilePathChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._+-"

// ProfilePathPlain says p holds only ProfilePathChars, so a profile may
// name it as it is.
func ProfilePathPlain(p string) bool {
	return p != "" && strings.Trim(p, ProfilePathChars) == ""
}
