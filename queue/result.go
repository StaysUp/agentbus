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
	ModelCalls int      `json:"model_calls,omitempty"`
	CostUSD    *float64 `json:"cost_usd,omitempty"`
	// Output, OutputBytes, TimedOut and DurationMS, for an exec job: the end
	// of what the command wrote to stdout and stderr together (at most
	// MaxCommandOutput bytes, the end kept), all the bytes it wrote, whether
	// it ran past its time and was stopped, and how long it ran. ExitCode is
	// its exit code, or 128 plus the signal that ended it; Stopped says a
	// person or an agent stopped it.
	Output      string `json:"output,omitempty"`
	OutputBytes int64  `json:"output_bytes,omitempty"`
	TimedOut    bool   `json:"timed_out,omitempty"`
	DurationMS  int64  `json:"duration_ms,omitempty"`
	// Refused: why the turn did not start, and the status that says what
	// kind (run.Refusal).
	Refused string `json:"refused,omitempty"`
	Status  int    `json:"status,omitempty"`
	// The turn's reply is not copied here: it lives once in the record (the
	// model.responded event). A review's verdict and an asked result's reply
	// are read from there by its turn (store.TurnText), and the single turn
	// GET /api/v0/turns/{id} serves the whole reply.
	//
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
	// Update, for an update job: the outcome of merging the person's folder
	// into the copy (From/To changesets on a clean merge, Conflicts on a
	// conflict). The runner fills it at the finish.
	Update *UpdateResult `json:"update,omitempty"`
	// Snapshot, for a snapshot job: the snapshot it took.
	Snapshot *SnapshotTaken `json:"snapshot,omitempty"`
	// FolderRef, for a folder job: the reference the connector registered
	// the typed folder under.
	FolderRef string `json:"folder_ref,omitempty"`
	// BaseCommit, for a folder job that named a base: the commit it
	// resolved to in the folder's repo.
	BaseCommit string `json:"base_commit,omitempty"`
	// Branches, for a folder job that asked for them: the branches of the
	// folder's repo, or of one of its remotes.
	Branches *BranchList `json:"branches,omitempty"`
	// Home, for a home job: the usual config files its machine's home
	// holds. Names, kinds and sizes only.
	Home []HomeFile `json:"home,omitempty"`

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
	// DiscardedEvent: a discard's workspace.discarded, by its id, in place of
	// the outcome itself (as AppliedEvent is for an apply).
	DiscardedEvent string `json:"discarded_event,omitempty"`
	// ClosedEvent is the close's workspace.closed in the record.
	ClosedEvent string `json:"closed_event,omitempty"`
	// UpdatedEvent: an update's workspace.updated, by its id, in place of the
	// outcome itself (as DiscardedEvent is for a discard).
	UpdatedEvent string `json:"updated_event,omitempty"`
	// FailedEvent: a discard's, update's or close's workspace.failed, by its
	// id, when the job ended without doing its work (in place of Refused).
	FailedEvent string `json:"failed_event,omitempty"`
	// Kept: a put-away that control ended without a runner, since the
	// workspace was not quiet and settled when it came due: why it kept the
	// copy. KeptEvent is its workspace.kept in the record, once written.
	Kept      string `json:"kept,omitempty"`
	KeptEvent string `json:"kept_event,omitempty"`
	// OutcomeDue: the job ended with no finish (its runner was lost), so
	// no finisher wrote its outcome; control's sweep writes it to the
	// record and sets the pointer in its place (OutcomesDue, KeepOutcome).
	OutcomeDue bool `json:"outcome_due,omitempty"`
}

// MaxCommandOutput bounds the end of a command's output an exec job's result
// keeps, and so what its delivery and command.ended carry.
const MaxCommandOutput = 16 << 10

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
	// Markers: the file is refused because it still holds the conflict
	// markers an update's merge left, not because the folder changed. The
	// agent combining it clears it; an update does not.
	Markers bool `json:"markers,omitempty" doc:"The file still holds the conflict markers an update's merge left (<<<<<<< this session ... >>>>>>> your folder), rather than having changed in the folder: the agent combining it in a turn clears it, an update does not."`
}

// UpdateResult is what an update job did: on a clean merge From is the new
// baseline changeset (the folder as it is now) and To the merged changeset (the
// copy's new head), both blobs in the control plane's store; on a real conflict
// From and To are empty and Conflicts names the files. Clean reports which.
//
// TheirsCheckpoint and MergedCheckpoint are the copy's own checkpoint names for
// the folder snapshot and the merge: the shadow names, which control passes back
// as the merge base of the next update (the folder the copy last matched) and
// the reset point of a discard after this merge was applied. Changesets name
// content by blob; these name it by checkpoint, which the copy's git needs.
type UpdateResult struct {
	From             string   `json:"from,omitempty"`
	To               string   `json:"to,omitempty"`
	TheirsCheckpoint string   `json:"theirs_checkpoint,omitempty"`
	MergedCheckpoint string   `json:"merged_checkpoint,omitempty"`
	Conflicts        []string `json:"conflicts,omitempty"`
	// Unresolved, on a merge that met a conflict: the files left holding
	// conflict markers for the agent to resolve. The merge is still recorded
	// (From and To set), so Clean is true.
	Unresolved []string `json:"unresolved,omitempty"`
	// Unmergeable, on a merge that changed nothing: the conflicting files
	// conflict markers cannot show (binary, a link, removed on one side).
	Unmergeable []string `json:"unmergeable,omitempty"`
}

// Clean reports whether the update merged the folder in (From and To set), as
// against leaving the copy unchanged on a conflict.
func (u *UpdateResult) Clean() bool { return u != nil && u.To != "" && len(u.Conflicts) == 0 }

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

// HomeFile is one config path a home job found (KindHome): its name, what
// it is and its size, never its content.
type HomeFile struct {
	Path  string `json:"path" doc:"As home_files takes it, such as ~/.aws/config; a folder's ends in /."`
	Kind  string `json:"kind" doc:"file or folder."`
	Size  int64  `json:"size" doc:"Bytes: a file's size, or a folder's files' in all, the vendor sign-in files left out."`
	Files int    `json:"files,omitempty" doc:"For a folder: how many files it holds, at any depth."`
	Part  string `json:"part,omitempty" doc:"Set when only a part of the file is copied: mcpServers for ~/.claude.json."`
	Over  bool   `json:"over,omitempty" doc:"A folder with more files or bytes than home_files copies (10000 files, 64 MiB): listing it would refuse each turn."`
}

// BranchList is what a folder job that lists branches found: the repo's
// remotes, and the branches of the repo itself or of the remote named.
type BranchList struct {
	Remotes  []string `json:"remotes" doc:"The remotes of the folder's repo, such as origin, sorted."`
	Remote   string   `json:"remote,omitempty" doc:"The remote whose branches these are. Absent for the repo's own branches."`
	Current  string   `json:"current,omitempty" doc:"For the repo's own branches: the branch the folder has checked out. Absent on a detached HEAD."`
	Branches []Branch `json:"branches" doc:"The branches, most recently changed first for the repo's own, by name for a remote's. At most 1000."`
	// Stale: the remote could not be reached, so these are the branches the
	// repo last fetched from it.
	Stale     bool `json:"stale,omitempty" doc:"True when the remote could not be reached: the branches are the ones the repo last fetched from it, which may be behind."`
	Truncated bool `json:"truncated,omitempty" doc:"True when there were more than 1000."`
}

// Branch is one branch: its name and the commit it points at.
type Branch struct {
	Name   string `json:"name" doc:"The branch's name, such as feature/login: what base takes."`
	Commit string `json:"commit" doc:"The commit it points at."`
}
