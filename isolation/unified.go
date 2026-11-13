package isolation

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/staysup/agentbus/workspace"
)

// Limits of the per-file diffs a checkpoint diff carries.
const (
	// MaxFileDiff bounds one side of a file that gets a diff.
	MaxFileDiff = workspace.MaxFileDiff
	// MaxFileDiffs bounds every file's diff together: the 1 MiB of diffs a
	// turn's turn.changed holds.
	MaxFileDiffs = workspace.MaxFileDiffs
)

// unified is the unified diff of path from a to b (workspace.Unified).
func unified(path string, a, b []byte, change string) string {
	return workspace.Unified(path, a, b, change)
}

// addFileDiffs gives each changed text file of c its own unified diff, from
// its content at a to its content at b. A file's content at either side is
// read by end: its saved copy, or the space's folder while the file there is
// still as it was then. Its earlier content may also be a blob of the
// snapshot the copy was built from, or, on the local fast path, the
// person's folder read through the Lender while the file there is still as
// it was at a (a clone's base saves nothing). A file with no diff that has a
// change of content is marked DiffLeftOut: binary, a link, too big, past the
// budget, or its earlier content is not kept.
func addFileDiffs(s *Space, end func(rel string, f workspace.File, max int) ([]byte, bool), a, b *workspace.Snapshot, c *workspace.Changes) {
	read := func(rel string, f workspace.File) ([]byte, bool) {
		if f.Mode&fs.ModeSymlink != 0 || f.Size > MaxFileDiff {
			return nil, false
		}
		return end(rel, f, MaxFileDiff)
	}
	budget := MaxFileDiffs
	for i := range c.Files {
		ch := &c.Files[i]
		var old, cur []byte
		ok := true
		if ch.Change != "added" {
			fa, _ := a.File(ch.Path)
			old, ok = read(ch.Path, fa)
			if !ok && fa.Mode&fs.ModeSymlink == 0 && fa.Size <= MaxFileDiff && fa.Hash != "" && baseContent(s, ch.Path, fa) {
				old, ok = read(ch.Path, fa)
			}
		}
		if ok && ch.Change != "deleted" {
			fb, _ := b.File(ch.Path)
			cur, ok = read(ch.Path, fb)
		}
		if ok && (!isText(old) || !isText(cur)) {
			ok = false
		}
		if !ok {
			ch.DiffLeftOut = true
			continue
		}
		if bytes.Equal(old, cur) {
			continue // only its mode changed
		}
		d := unified(ch.Path, old, cur, ch.Change)
		if len(d) > budget {
			ch.DiffLeftOut, c.DiffsCut = true, true
			continue
		}
		budget -= len(d)
		ch.Diff = &d
	}
}

// readMatching reads rel of rt, at most max bytes, when its content still
// has hash.
func readMatching(rt *os.Root, rel, hash string, max int) ([]byte, bool) {
	body, ok := readRegular(rt, filepath.FromSlash(rel), max)
	if !ok {
		return nil, false
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != hash {
		return nil, false
	}
	return body, true
}

// isText: valid UTF-8 with no NUL byte.
func isText(b []byte) bool {
	return bytes.IndexByte(b, 0) < 0 && utf8.Valid(b)
}

// baseContent saves the earlier content of rel, f, among s's saved files,
// from the snapshot's blobs or the person's folder, and says whether it
// could.
func baseContent(s *Space, rel string, f workspace.File) bool {
	if fromBlobs(s, f.Hash) {
		return true
	}
	h, saved, ok := lend(s, rel, f, true)
	return ok && saved && h == f.Hash
}
