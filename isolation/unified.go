package isolation

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/staysup/agentbus/workspace"
)

// Limits of the per-file diffs a checkpoint diff carries.
const (
	// MaxFileDiff bounds one side of a file that gets a diff.
	MaxFileDiff = 256 << 10
	// MaxFileDiffs bounds every file's diff together: the 1 MiB of diffs a
	// turn's turn.changed holds.
	MaxFileDiffs = 1 << 20
	// maxCells bounds the table the line matcher fills. Past it, the changed
	// middle of a file is shown as all removed, then all added.
	maxCells = 1 << 21
	context3 = 3
)

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

// splitLines splits b into lines, each with its newline; the last one has
// none when b does not end with one.
func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	s := string(b)
	var out []string
	for s != "" {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			out = append(out, s)
			break
		}
		out = append(out, s[:i+1])
		s = s[i+1:]
	}
	return out
}

// op is one line of a diff: ' ' kept, '-' removed, '+' added.
type op struct {
	kind byte
	line string
}

// lineOps matches the lines of a and b: the common start and end are kept,
// and the middle by its longest common subsequence while that fits maxCells.
func lineOps(a, b []string) []op {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	ops := make([]op, 0, len(a)+len(b))
	for _, l := range a[:pre] {
		ops = append(ops, op{' ', l})
	}
	ma, mb := a[pre:len(a)-suf], b[pre:len(b)-suf]
	n, m := len(ma), len(mb)
	if (n+1)*(m+1) > maxCells {
		for _, l := range ma {
			ops = append(ops, op{'-', l})
		}
		for _, l := range mb {
			ops = append(ops, op{'+', l})
		}
	} else {
		// lcs[i*(m+1)+j] is the longest common subsequence of ma[i:], mb[j:].
		w := m + 1
		lcs := make([]int32, (n+1)*w)
		for i := n - 1; i >= 0; i-- {
			for j := m - 1; j >= 0; j-- {
				if ma[i] == mb[j] {
					lcs[i*w+j] = lcs[(i+1)*w+j+1] + 1
				} else {
					lcs[i*w+j] = max(lcs[(i+1)*w+j], lcs[i*w+j+1])
				}
			}
		}
		i, j := 0, 0
		for i < n || j < m {
			switch {
			case i < n && j < m && ma[i] == mb[j]:
				ops = append(ops, op{' ', ma[i]})
				i++
				j++
			case i < n && (j == m || lcs[(i+1)*w+j] >= lcs[i*w+j+1]):
				// A removal comes before the addition that replaces it.
				ops = append(ops, op{'-', ma[i]})
				i++
			default:
				ops = append(ops, op{'+', mb[j]})
				j++
			}
		}
	}
	for _, l := range a[len(a)-suf:] {
		ops = append(ops, op{' ', l})
	}
	return ops
}

// unified is the unified diff of path from a to b, with three lines of
// context, as git shows it.
func unified(path string, a, b []byte, change string) string {
	ops := lineOps(splitLines(a), splitLines(b))
	// apos[i] and bpos[i]: the lines of a and of b before ops[i].
	apos, bpos := make([]int, len(ops)+1), make([]int, len(ops)+1)
	for i, o := range ops {
		apos[i+1], bpos[i+1] = apos[i], bpos[i]
		if o.kind != '+' {
			apos[i+1]++
		}
		if o.kind != '-' {
			bpos[i+1]++
		}
	}
	var out strings.Builder
	from, to := "a/"+path, "b/"+path
	if change == "added" {
		from = "/dev/null"
	}
	if change == "deleted" {
		to = "/dev/null"
	}
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", from, to)
	i := 0
	for i < len(ops) {
		for i < len(ops) && ops[i].kind == ' ' {
			i++
		}
		if i == len(ops) {
			break
		}
		start := max(0, i-context3)
		// The hunk runs to the last change that is at most 2*context kept
		// lines from the one before it.
		last := i
		for j := i; j < len(ops); j++ {
			if ops[j].kind != ' ' {
				if j-last > 2*context3 {
					break
				}
				last = j
			}
		}
		end := min(len(ops), last+context3+1)
		fmt.Fprintf(&out, "@@ -%s +%s @@\n", hunkRange(apos[start], apos[end]-apos[start]),
			hunkRange(bpos[start], bpos[end]-bpos[start]))
		for _, o := range ops[start:end] {
			out.WriteByte(o.kind)
			out.WriteString(o.line)
			if !strings.HasSuffix(o.line, "\n") {
				out.WriteString("\n\\ No newline at end of file\n")
			}
		}
		i = end
	}
	return out.String()
}

// hunkRange is one side of a hunk header: its first line and its count, as
// git writes them (an empty side names the line before it).
func hunkRange(before, count int) string {
	switch count {
	case 0:
		return fmt.Sprintf("%d,0", before)
	case 1:
		return fmt.Sprintf("%d", before+1)
	}
	return fmt.Sprintf("%d,%d", before+1, count)
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
