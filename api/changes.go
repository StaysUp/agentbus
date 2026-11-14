package api

// A conversation's changes not applied yet: GET /api/v0/sessions/{id}/changes.
// It is the dry run of the apply that POST /api/v0/turns/{id}/apply of the
// session's latest turn would make: the same two changesets (queue.Preview
// reads them where Apply does), on the runner of the folder's machine, by
// the apply's own code stopped before it writes (connector.Preview). It
// writes nothing in the folder and stores no event; the preview is a job in
// the queue, as an apply is, removed once read (queue.DropPreview).

import (
	"errors"
	"net/http"

	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/settings"
)

// SessionChanges is the answer to GET /api/v0/sessions/{id}/changes.
type SessionChanges struct {
	Session   string                `json:"session"`
	Isolated  bool                  `json:"isolated" doc:"The session works in an isolated copy of the person's folder, so its changes reach the folder only by an apply. false: it works in the folder as it is, there is nothing to apply, and why says so."`
	Unapplied bool                  `json:"unapplied" doc:"An apply would change the person's folder: files or conflicts is not empty. false when every change is applied, when the folder already holds them, or when nothing can be applied (why says why)."`
	Turn      string                `json:"turn,omitempty" doc:"The session's latest ended turn with changes: the turn to name in POST /api/v0/turns/{id}/apply to apply everything. Also set when it is applied already. Absent when the session has none, or its first turn has not ended."`
	Turns     []string              `json:"turns,omitempty" doc:"Every turn whose changes that apply would carry, oldest first."`
	Files     []PreviewFile         `json:"files" doc:"Each file that apply would write or remove, and each file in conflicts, sorted by path, with its change against the person's folder now."`
	Count     int                   `json:"count" doc:"How many files. files may list fewer, with truncated."`
	Truncated bool                  `json:"truncated,omitempty" doc:"files or conflicts was cut to keep the answer small."`
	DiffsCut  bool                  `json:"diffs_cut,omitempty" doc:"Some text files have no diff here: all diffs together passed 1 MiB, and the later ones were left out (each marked diff_left_out)."`
	Conflicts []queue.ApplyConflict `json:"conflicts" doc:"Each file in the person's folder that is neither as it was at the start of the changes nor already as they leave it, with why: exactly the files that would refuse the apply. Not empty: the apply would write nothing."`
	Why       string                `json:"why,omitempty" doc:"Why nothing can be applied: the session does not work in an isolated copy, or it was closed."`
}

// PreviewFile is one file of SessionChanges.
type PreviewFile struct {
	Path        string  `json:"path"`
	Change      string  `json:"change" doc:"added, modified or deleted, against the person's folder now."`
	Added       int     `json:"added" doc:"Lines the change adds. 0 when no diff could be made."`
	Removed     int     `json:"removed" doc:"Lines the change removes. 0 when no diff could be made."`
	Diff        *string `json:"diff,omitempty" doc:"The unified diff from the file in the person's folder now to what the apply would write, when both are small text (at most 256 KiB)."`
	DiffLeftOut bool    `json:"diff_left_out,omitempty" doc:"The content changes and no diff is here: a binary file, a link, a file over 256 KiB, a folder in the way, or past the 1 MiB of all diffs."`
}

// Answers of the changes route, in fixed wording.
const (
	changesNotIsolated = "this session works in your folder as it is, not in an isolated copy, so its changes are already there: there is nothing to apply"
	changesSlow        = "the folder's machine did not answer in time, so no preview was made; nothing was written: ask again"
	changesFailed      = "the preview could not be made; nothing was written"
)

// GET /api/v0/sessions/<id>/changes: what applying the session's changes
// would do to the person's folder now.
func (a *API) sessionChanges(w http.ResponseWriter, r *http.Request, id string) {
	out := SessionChanges{Session: id, Isolated: true, Files: []PreviewFile{}, Conflicts: []queue.ApplyConflict{}}
	if a.cfg.Queue == nil {
		a.changesWithoutQueue(w, out)
		return
	}
	job, turn, err := a.cfg.Queue.Preview(queue.DefaultTeam, id)
	out.Turn = turn
	switch {
	case errors.Is(err, queue.ErrNotFound):
		a.changesWithoutQueue(w, out)
		return
	case errors.Is(err, queue.ErrNotIsolated):
		out.Isolated, out.Why = false, changesNotIsolated
		writeJSON(w, http.StatusOK, out)
		return
	case errors.Is(err, queue.ErrNotApplicable):
		out.Why = applyNotApplicable
		writeJSON(w, http.StatusOK, out)
		return
	case errors.Is(err, queue.ErrSessionClosed):
		out.Why = applySessionClosed
		writeJSON(w, http.StatusOK, out)
		return
	case errors.Is(err, queue.ErrAllApplied):
		writeJSON(w, http.StatusOK, out)
		return
	case errors.Is(err, queue.ErrNoCopyYet):
		// The first turn asked for a copy; isolation none runs it in the
		// folder as it is.
		none, err := a.isolationNone()
		if err != nil {
			a.internal(w)
			return
		}
		if none {
			out.Isolated, out.Why = false, changesNotIsolated
		}
		writeJSON(w, http.StatusOK, out)
		return
	case errors.Is(err, queue.ErrApplyBusy):
		writeError(w, http.StatusConflict, applyBusy)
		return
	case errors.Is(err, queue.ErrMachineOffline):
		writeError(w, http.StatusServiceUnavailable, applyOffline)
		return
	case errors.Is(err, queue.ErrFull):
		writeError(w, http.StatusTooManyRequests, "too many turns and applies are waiting; retry later")
		return
	case err != nil:
		a.internal(w)
		return
	}
	j, err := a.awaitEnd(r.Context(), job)
	if err != nil || j.Ended == nil {
		// Nobody will read it: a preview not ended is stopped, so it does
		// not take a folder place for nothing.
		_, _ = a.cfg.Queue.Cancel(queue.DefaultTeam, job.ID)
		switch {
		case r.Context().Err() != nil:
		case err != nil:
			a.internal(w)
		default:
			writeError(w, http.StatusGatewayTimeout, changesSlow)
		}
		return
	}
	// Read here and nowhere else: the job goes once it is read.
	defer func() { _ = a.cfg.Queue.DropPreview(queue.DefaultTeam, job.ID) }()
	res, _ := queue.ReadResult(j.Result)
	p := res.Preview
	if j.State != queue.Finished || p == nil {
		msg := res.Refused
		if msg == "" {
			msg = changesFailed
		}
		writeError(w, http.StatusConflict, msg)
		return
	}
	out.Turns, out.Count, out.Truncated, out.DiffsCut = j.Links.AppliesTurns, p.Count, p.Truncated, p.DiffsCut
	for _, f := range p.Files {
		out.Files = append(out.Files, PreviewFile{Path: f.Path, Change: f.Change, Added: f.Added, Removed: f.Removed,
			Diff: f.Diff, DiffLeftOut: f.DiffLeftOut})
	}
	if p.Conflicts != nil {
		out.Conflicts = p.Conflicts
	}
	out.Unapplied = out.Count > 0 || len(out.Conflicts) > 0
	writeJSON(w, http.StatusOK, out)
}

// isolationNone says whether the policy's isolation is none: a session's
// turns run in the folder as it is, even when its first turn asked for a
// copy.
func (a *API) isolationNone() (bool, error) {
	if a.cfg.Settings == nil {
		return false, nil
	}
	p, _, err := a.cfg.Settings.Policy()
	if err != nil {
		return false, err
	}
	return p.Isolation == settings.IsolationNone, nil
}

// changesWithoutQueue answers for a session the queue does not hold: one
// the record holds worked in a folder as it is (its turns came through the
// hooks or the gateway), else there is no such session.
func (a *API) changesWithoutQueue(w http.ResponseWriter, out SessionChanges) {
	snap, err := a.cfg.Store.Snapshot()
	if err != nil {
		a.internal(w)
		return
	}
	for _, s := range snap.Sessions {
		if s.ID == out.Session {
			out.Isolated, out.Why = false, changesNotIsolated
			writeJSON(w, http.StatusOK, out)
			return
		}
	}
	writeError(w, http.StatusNotFound, "no such session")
}
