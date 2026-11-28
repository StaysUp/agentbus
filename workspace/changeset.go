package workspace

import (
	"encoding/json"
	"errors"
	"sort"
	"unicode/utf8"
)

// Changeset is what a session's copy holds at one checkpoint that differs
// from the copy's base: each such file as the base held it and as the
// checkpoint holds it. The runner makes one at the end of each turn and
// keeps it in the control plane's blob store; an apply compares two of the
// same session (Between), so it needs neither the copy nor its runner.
type Changeset struct {
	// Space: the session's copy, by its id on its runner. Two changesets
	// can be compared only when they name the same space.
	Space string `json:"space"`
	// Checkpoint: the checkpoint it describes, by name.
	Checkpoint string `json:"checkpoint"`
	// Snapshot: the base's snapshot id, when every file of the base was
	// hashed; empty on the local fast path, which does not hash the base.
	Snapshot string `json:"snapshot,omitempty"`
	// BaseTaken: when the copy began reading the person's folder for its
	// base, in Unix nanoseconds; 0 when not known. A base file recorded
	// without a hash is as the folder held it then, so the folder side takes
	// a file as that one, without reading it, only when Keeps says so at
	// this time: its change time is older, which no one can set back.
	BaseTaken int64 `json:"base_taken,omitempty"`
	// Files, sorted by path.
	Files []FileChange `json:"files"`
	// Truncated: the copy had more files than a checkpoint holds, so the
	// changeset cannot be applied.
	Truncated bool `json:"truncated,omitempty"`
}

// FileChange is one file of a Changeset. Base nil: the base did not hold it;
// Now nil: the checkpoint does not.
type FileChange struct {
	Path string `json:"path"`
	Base *File  `json:"base,omitempty"`
	Now  *File  `json:"now,omitempty"`
}

// fileChangeJSON is a FileChange as it is written. A file name is bytes, and
// on Linux they need not be UTF-8, but a JSON string must be: encoding/json
// writes U+FFFD for each bad byte, so such a name read back is another name,
// and an apply would act on that one. PathRaw carries the name's own bytes
// beside Path, only for a name that is not valid UTF-8, and a reader takes
// them as the path. A name that is valid UTF-8 is written as it always was,
// and a changeset written before PathRaw reads as it did.
type fileChangeJSON struct {
	Path    string `json:"path"`
	PathRaw []byte `json:"path_raw,omitempty"`
	Base    *File  `json:"base,omitempty"`
	Now     *File  `json:"now,omitempty"`
}

func (f FileChange) MarshalJSON() ([]byte, error) {
	j := fileChangeJSON{Path: f.Path, Base: f.Base, Now: f.Now}
	if !utf8.ValidString(f.Path) {
		j.PathRaw = []byte(f.Path)
	}
	return json.Marshal(j)
}

func (f *FileChange) UnmarshalJSON(b []byte) error {
	var j fileChangeJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	*f = FileChange{Path: j.Path, Base: j.Base, Now: j.Now}
	if j.PathRaw != nil {
		f.Path = string(j.PathRaw)
	}
	return nil
}

// Step is one file an apply changes: the folder must hold From (nil: no file
// there), or already To, and it is made To (nil: removed).
type Step struct {
	Path     string
	From, To *File
}

// ErrNotComparable: two changesets of different copies.
var ErrNotComparable = errors.New("workspace: the changesets are of different copies")

// Between is what turns the folder from as at from into as at to: every
// file whose state differs, sorted by path. A nil from is the base itself.
// Both must be of the same copy, and to must not be truncated.
func Between(from, to *Changeset) ([]Step, error) {
	if to == nil || (from != nil && from.Space != to.Space) {
		return nil, ErrNotComparable
	}
	type pair struct{ from, to *File }
	states := map[string]*pair{}
	if from != nil {
		for _, f := range from.Files {
			// A path only from changed: to holds it as the base did.
			states[f.Path] = &pair{from: f.Now, to: f.Base}
		}
	}
	for _, f := range to.Files {
		p, ok := states[f.Path]
		if !ok {
			// A path only to changed: from holds it as the base did.
			states[f.Path] = &pair{from: f.Base, to: f.Now}
			continue
		}
		p.to = f.Now
	}
	var out []Step
	for path, p := range states {
		if SameFile(p.from, p.to) {
			continue
		}
		out = append(out, Step{Path: path, From: p.from, To: p.to})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// SameFile says a and b are the same file state: both absent, or the same
// content and mode, as a checkpoint diff compares them.
func SameFile(a, b *File) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Hash == b.Hash && a.Mode == b.Mode
}
