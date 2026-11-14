package queue

import (
	"encoding/json"

	"github.com/staysup/agentbus/workspace"
)

// Result is what a finished job's result holds: how its turn ended, and what
// it changed in its workspace. The runner writes it; the queue and the API
// read it back. It is the one declaration of that format.
type Result struct {
	// Session is the session the turn ran in. For a turn job it is the job's
	// own session; a review runs in a session of its own.
	Session    string   `json:"session,omitempty"`
	State      string   `json:"state,omitempty"`
	ExitCode   *int     `json:"exit_code,omitempty"`
	Stopped    bool     `json:"stopped,omitempty"`
	Unsettled  bool     `json:"unsettled,omitempty"`
	Error      string   `json:"error,omitempty"`
	Reply      string   `json:"reply,omitempty"`
	ModelCalls int      `json:"model_calls,omitempty"`
	CostUSD    *float64 `json:"cost_usd,omitempty"`
	// Refused: why the turn did not start, and the status that says what
	// kind (run.Refusal).
	Refused string `json:"refused,omitempty"`
	Status  int    `json:"status,omitempty"`
	// ReplyText, for a review job and asked work: the whole text of the turn's
	// last reply, at most runner.MaxReplyText bytes. The queue reads the
	// verdict from it. Reply is only its first 500 characters.
	ReplyText string `json:"reply_text,omitempty"`
	// Folder and Changes: the job's workspace and what changed in it, set
	// when the runner watched a folder.
	Folder  string             `json:"folder,omitempty"`
	Changes *workspace.Changes `json:"changes,omitempty"`
	// Apply, for an apply job: what it wrote, or the files that refused it.
	Apply *ApplyResult `json:"apply,omitempty"`
	// Preview, for a preview job: what an apply would write, or the files
	// that would refuse it. Nothing was written.
	Preview *PreviewResult `json:"preview,omitempty"`
	// Changeset, for a turn in an isolated copy: the blob, in the control
	// plane's store, of what the copy holds at the turn's end that differs
	// from its base (workspace.Changeset). An apply reads it.
	Changeset string `json:"changeset,omitempty"`
	// Snapshot, for a snapshot job: the snapshot it took.
	Snapshot *SnapshotTaken `json:"snapshot,omitempty"`
	// FolderRef, for a folder job: the reference the connector registered
	// the typed folder under.
	FolderRef string `json:"folder_ref,omitempty"`

	// Pointers to the outcomes control keeps in its record, from 0.5 (the
	// record holds every outcome once; the job only names it): the turn's
	// turn.changed in place of Changes, and an apply's turn.applied in
	// place of Apply and Refused. A job from before 0.5 has none and keeps
	// its outcome here. (A retired command job's ran and ran_event are no
	// longer read.)
	ChangesEvent string `json:"changes_event,omitempty"`
	// Checkpoint: the checkpoint the turn ended with (Changes.To), kept
	// beside ChangesEvent: the session's next turn starts from it.
	Checkpoint   string `json:"checkpoint,omitempty"`
	AppliedEvent string `json:"applied_event,omitempty"`
	// OutcomeDue: the job ended with no finish (its runner was lost), so
	// no finisher wrote its outcome; control's sweep writes it to the
	// record and sets the pointer in its place (OutcomesDue, KeepOutcome).
	OutcomeDue bool `json:"outcome_due,omitempty"`
}

// SnapshotTaken is a snapshot a connector took: its id, the blob of its
// files' times, and how many files it holds. Its trees and files are blobs
// in the control plane's store.
type SnapshotTaken struct {
	ID    string `json:"id"`
	Times string `json:"times,omitempty"`
	Files int    `json:"files"`
	// Git: the content tree of the folder's .git, carried beside the
	// snapshot and not part of its id.
	Git string `json:"git,omitempty"`
}

// ApplyResult is what an apply job did in the person's folder.
type ApplyResult struct {
	// Files were written or removed, sorted. On a refusal, none. Count is
	// all of them; Files may hold fewer, with Truncated (Fit).
	Files     []string `json:"files"`
	Count     int      `json:"count"`
	Truncated bool     `json:"truncated,omitempty"`
	// Pruned: the checkpoints before the turn's end were removed.
	Pruned bool `json:"pruned,omitempty"`
	// Conflicts: the files that refused the apply, each with why.
	Conflicts []ApplyConflict `json:"conflicts,omitempty"`
}

// Fit shrinks a so that its JSON is at most max bytes: files, then conflicts,
// are cut from the end, and Truncated says so. Count stays the whole count.
func (a *ApplyResult) Fit(max int) {
	size := func() int { b, _ := json.Marshal(a); return len(b) }
	for size() > max && (len(a.Files) > 0 || len(a.Conflicts) > 0) {
		if len(a.Files) > 0 {
			a.Files = a.Files[:len(a.Files)/2]
		} else {
			a.Conflicts = a.Conflicts[:len(a.Conflicts)/2]
		}
		a.Truncated = true
	}
}

// ApplyConflict is one file an apply would not overwrite, and why.
type ApplyConflict struct {
	Path string `json:"path"`
	Why  string `json:"why"`
}

// ReadResult reads a job's result. An empty one is the zero Result.
func ReadResult(raw json.RawMessage) (Result, error) {
	var r Result
	if len(raw) == 0 {
		return r, nil
	}
	err := json.Unmarshal(raw, &r)
	return r, err
}

// PreviewResult is what an apply would do, as its dry run (KindPreview)
// found it: nothing was written.
type PreviewResult struct {
	// Files: each file the apply would write or remove, and each file that
	// would refuse it, sorted by path. Count is all of them; Files may hold
	// fewer, with Truncated (Fit).
	Files     []PreviewFile `json:"files"`
	Count     int           `json:"count"`
	Truncated bool          `json:"truncated,omitempty"`
	// DiffsCut: some files have no diff because all diffs together passed
	// their budget, or to keep the result small.
	DiffsCut bool `json:"diffs_cut,omitempty"`
	// Conflicts: the files that would refuse the apply, each with why, as
	// the apply itself would name them.
	Conflicts []ApplyConflict `json:"conflicts,omitempty"`
}

// PreviewFile is one file of a preview: its change against the person's
// folder now.
type PreviewFile struct {
	Path   string `json:"path"`
	Change string `json:"change"` // added, modified or deleted
	// Added and Removed: the diff's lines. 0 when there is no diff.
	Added   int `json:"added"`
	Removed int `json:"removed"`
	// Diff: the unified diff from the folder now to what the apply would
	// write, for a small text file on both sides.
	Diff        *string `json:"diff,omitempty"`
	DiffLeftOut bool    `json:"diff_left_out,omitempty"`
}

// Fit shrinks p so that its JSON is at most max bytes: first every diff
// goes, with its line counts, then the files no conflict names, from the
// end, then conflicts from the end, each with its file. So every conflict
// kept has its entry in Files. DiffsCut, DiffLeftOut and Truncated say so.
// Count stays the whole count.
func (p *PreviewResult) Fit(max int) {
	size := func() int { b, _ := json.Marshal(p); return len(b) }
	if size() <= max {
		return
	}
	for i := range p.Files {
		if f := &p.Files[i]; f.Diff != nil {
			f.Diff, f.Added, f.Removed, f.DiffLeftOut, p.DiffsCut = nil, 0, 0, true, true
		}
	}
	for size() > max && (len(p.Files) > 0 || len(p.Conflicts) > 0) {
		p.Truncated = true
		conflicted := map[string]bool{}
		for _, c := range p.Conflicts {
			conflicted[c.Path] = true
		}
		var others []int
		for i, f := range p.Files {
			if !conflicted[f.Path] {
				others = append(others, i)
			}
		}
		if len(others) > 0 {
			// Half of the other files go, the last ones.
			gone := map[int]bool{}
			for _, i := range others[len(others)/2:] {
				gone[i] = true
			}
			p.Files = keepFiles(p.Files, func(i int, _ PreviewFile) bool { return !gone[i] })
			continue
		}
		p.Conflicts = p.Conflicts[:len(p.Conflicts)/2]
		kept := map[string]bool{}
		for _, c := range p.Conflicts {
			kept[c.Path] = true
		}
		p.Files = keepFiles(p.Files, func(_ int, f PreviewFile) bool { return kept[f.Path] })
	}
}

// keepFiles is the files of fs that keep says to keep, in order.
func keepFiles(fs []PreviewFile, keep func(int, PreviewFile) bool) []PreviewFile {
	out := fs[:0:0]
	for i, f := range fs {
		if keep(i, f) {
			out = append(out, f)
		}
	}
	return out
}
