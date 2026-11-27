package workspace

import (
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// SameName says whether a and b may be two spellings of one name to a disk
// that folds names: equal once both are in Unicode's composed form (NFC) and
// fully case-folded, as a disk that ignores case (APFS and HFS+ by default,
// ext4 casefold, NTFS) or Unicode form (APFS, HFS+) takes them. The folding is
// Unicode's full one, which APFS uses: ß and SS, ﬁ and FI are one name there
// (measured 2026-11-27, macOS 13 APFS). It says only that a disk may take
// them as one name, never that this one does: that is the disk's answer, a
// look-up of one landing on the file the other names. A hard link under
// another name is the same file and never the same name, so a caller asks
// both: the file (os.SameFile), and the name (SameName).
func SameName(a, b string) bool {
	return fold(a) == fold(b)
}

// fold is s composed, fully case-folded, and composed again (folding can
// leave a form that is not composed).
func fold(s string) string {
	return norm.NFC.String(cases.Fold().String(norm.NFC.String(s)))
}
