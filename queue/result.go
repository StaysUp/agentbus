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
