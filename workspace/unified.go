package workspace

import (
	"fmt"
	"strings"
)

// Limits of the per-file diffs a checkpoint diff, or an apply's preview,
// carries.
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

// Unified is the unified diff of path from a to b, with three lines of
// context, as git shows it.
func Unified(path string, a, b []byte, change string) string {
	d, _, _ := UnifiedCount(path, a, b, change)
	return d
}

// UnifiedCount is Unified, with how many lines it adds and removes.
func UnifiedCount(path string, a, b []byte, change string) (diff string, added, removed int) {
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
		switch o.kind {
		case '+':
			added++
		case '-':
			removed++
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
	return out.String(), added, removed
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
