package api

// A workspace's changes not applied yet: GET /api/v0/workspaces/{id}/changes.
// It pages the apply's dry run - what POST /api/v0/workspaces/{id}/apply would
// do - from the record. A preview runs on the folder's machine
// (connector.Preview, nothing written) and control stores it as a
// changes.previewed event, the one source; the route serves and pages that
// event. A later request reuses the stored preview until the workspace changes
// or ?fresh=true asks for a new one; an apply always re-checks the folder
// itself, so the safety stays there.

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/ids"
	"github.com/staysup/agentbus/queue"
)

// WorkspaceChangesPage is the answer to GET /api/v0/workspaces/{id}/changes: a
// page of the apply preview, from the record's latest changes.previewed.
type WorkspaceChangesPage struct {
	Workspace   string              `json:"workspace"`
	Isolated    bool                `json:"isolated" doc:"The workspace works in an isolated copy of the person's folder, so its changes reach the folder only by an apply. false: it works in the folder as it is, there is nothing to apply, and why says so."`
	Unapplied   bool                `json:"unapplied" doc:"An apply would change the person's folder: files or conflicts is not empty. false when every change is applied, when the folder already holds them, or when nothing can be applied (why says why)."`
	Turn        string              `json:"turn,omitempty" doc:"The workspace's latest ended turn with changes: the turn to name on POST /api/v0/workspaces/{id}/apply to apply everything. Also set when it is applied already. Absent when the workspace has none, or its first turn has not ended."`
	Turns       []string            `json:"turns,omitempty" doc:"Every turn whose changes that apply would carry, oldest first."`
	PreviewedAt *time.Time          `json:"previewed_at,omitempty" doc:"When this preview was taken. A reused preview is as of then, not now; ?fresh=true takes a new one. Absent when there is nothing to preview."`
	Store       string              `json:"store" doc:"The store id."`
	Event       string              `json:"event,omitempty" doc:"The changes.previewed event this page is from; send it back as event to page the same preview."`
	Count       int                 `json:"count" doc:"Every file the apply would write or remove, and every conflict, counted whole; files is a page of them, continued by next_before, or fewer than count with truncated when the stored preview omitted some."`
	Truncated   bool                `json:"truncated,omitempty" doc:"The stored preview kept fewer files than count: they were too many to send, so some are not in the record at all, and count still counts them. This is not paging: a page holding fewer than count is continued by next_before."`
	DiffsCut    bool                `json:"diffs_cut,omitempty" doc:"Some text files have no diff here: all diffs together passed 1 MiB, and the later ones were left out (each marked diff_left_out)."`
	Files       []PreviewFile       `json:"files" doc:"A page of the files the apply would write or remove, and the conflicts, sorted by path, each with its change against the person's folder now."`
	Conflicts   []event.RefusedFile `json:"conflicts" doc:"Each file in the person's folder that is neither as it was at the start of the changes nor already as they leave it, with why: exactly the files that would refuse the apply. Not empty: the apply would write nothing."`
	NextBefore  *int64              `json:"next_before" doc:"The before for the next, earlier page, or null when the page reaches the first file."`
	Stale       bool                `json:"stale,omitempty" doc:"This preview is out of date: the workspace changed, but the runner is offline so a fresh one cannot run. It is the last preview taken, as of previewed_at; retry when the runner is back, or ?fresh=true."`
	Why         string              `json:"why,omitempty" doc:"Why nothing can be applied: the workspace does not work in an isolated copy, or it was closed; or, with stale, that the runner is offline."`
}

// PreviewFile is one file of a session changes page.
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
	changesNotIsolated = "this workspace works in your folder as it is, not in an isolated copy, so its changes are already there: there is nothing to apply"
	changesSlow        = "the folder's machine did not answer in time, so no preview was made; nothing was written: ask again"
	changesFailed      = "the preview could not be made; nothing was written"
	changesStale       = "the workspace changed after this preview was taken, so it is not current: ?fresh=true takes a new one, which needs the folder's machine online"
)

// GET /api/v0/workspaces/<id>/changes: what applying the workspace's changes
// would do to the person's folder now. id is the workspace, so the preview
// event, the reuse gate and the page are all keyed by the workspace id.
func (a *API) workspaceChanges(w http.ResponseWriter, r *http.Request, id string) {
	before, limit, ok := pageParams(w, r)
	if !ok {
		return
	}
	// Validate the id is a real workspace, so a session id (or any other) cannot
	// probe a workspace's copy; the sandbox mode comes from the same record.
	ws := ids.WorkspaceID(id)
	wsp, found, err := a.cfg.Store.Workspace(ws)
	if err != nil {
		a.internal(w)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "no such workspace")
		return
	}
	sandbox := wsp.Sandbox
	q := r.URL.Query()
	// Paging a preview already served: read the pinned event and page it, so
	// every page is of the same snapshot even if the folder changes meanwhile.
	if pin := q.Get("event"); pin != "" {
		e, found, err := a.cfg.Store.PreviewByID(id, pin)
		if err != nil {
			a.internal(w)
			return
		}
		if !found || e.Previewed == nil {
			writeError(w, http.StatusNotFound, "no such preview; ask again without event")
			return
		}
		// A pinned page carries the same stale status as the first: if the
		// session has moved off this preview's cursor (or was closed), every
		// page of it is out of date, not only the first.
		stale, err := a.previewStale(ws, e.Previewed)
		if err != nil {
			a.internal(w)
			return
		}
		a.writePreviewPage(w, id, e, before, limit, stale)
		return
	}
	// Fast path: reuse the session's latest preview, without taking the gate,
	// when it is already current.
	if a.servedCurrent(w, r, id, ws, before, limit) {
		return
	}
	// Otherwise take the session's preview gate, so GETs that arrive together
	// run one preview, not several racing to store: wait for any in-flight one,
	// then reuse its result or run a preview. A ?fresh GET reuses nothing, so it
	// runs its own once the in-flight one is done. One deadline covers the wait
	// and the run together -- the same budget the await uses -- so a line of
	// waiters cannot each add a full await; it is passed on to runPreview.
	wait := a.cfg.TurnWait
	if wait == 0 {
		wait = DefaultTurnWait
	}
	ctx, cancel := context.WithTimeout(r.Context(), wait)
	defer cancel()
	leave, ok := a.previews.enter(ctx, id)
	if !ok {
		// Timed out waiting for the gate, or the client went away. Answer the
		// timeout as a slow preview does; say nothing when the client is gone.
		if ctx.Err() == context.DeadlineExceeded {
			writeError(w, http.StatusGatewayTimeout, changesSlow)
		}
		return
	}
	defer leave()
	r = r.WithContext(ctx)
	if a.servedCurrent(w, r, id, ws, before, limit) {
		return
	}
	a.runPreview(w, r, id, ws, sandbox, before, limit)
}

// servedCurrent serves the session's latest stored preview when it may be
// reused -- the request is not ?fresh, a preview is stored, and it is not stale
// (the appliable cursor still equals the one it recorded, and the session is
// not closed) -- and returns true. It returns false when a preview must be run;
// on an internal error it writes the error and returns true, the request done.
func (a *API) servedCurrent(w http.ResponseWriter, r *http.Request, id string, ws ids.WorkspaceID, before int64, limit int) bool {
	if r.URL.Query().Get("fresh") == "true" {
		return false
	}
	e, _, found, err := a.cfg.Store.LatestPreview(id)
	if err != nil {
		a.internal(w)
		return true
	}
	if !found || e.Previewed == nil {
		return false
	}
	stale, err := a.previewStale(ws, e.Previewed)
	if err != nil {
		a.internal(w)
		return true
	}
	if stale {
		return false
	}
	a.writePreviewPage(w, id, e, before, limit, false)
	return true
}

// previewStale says whether the stored preview p is out of date for the
// session now: it was closed, or its appliable cursor has moved off the one
// the preview recorded (a new appliable turn, or any apply). It is what the
// reuse check tests, read here so a pinned page reports the same stale status
// the first page did.
func (a *API) previewStale(ws ids.WorkspaceID, p *event.Previewed) (bool, error) {
	if a.sessionClosed(ws) {
		return true, nil
	}
	if a.cfg.Queue == nil {
		return false, nil
	}
	// The cursor and the closed state are the WORKSPACE's (the preview is of its
	// changes): a second session of a workspace reads the same cursor as the
	// first and its fresh preview is not marked stale for having no turn of its
	// own. The caller resolved the workspace (workspaceOf) and already answered
	// 404 for an unknown session.
	turn, applies, ok, err := a.cfg.Queue.AppliableCursor(queue.DefaultTeam, ws)
	if err != nil {
		return false, err
	}
	return !ok || turn != p.Turn || applies != p.Applies, nil
}

// sessionClosed says the session's workspace copy is gone, so any stored preview
// of it is stale. Workspace Close is in the record; permanent session closes
// from older builds are still held by the queue.
func (a *API) sessionClosed(ws ids.WorkspaceID) bool {
	if a.cfg.Store != nil {
		w, ok, err := a.cfg.Store.Workspace(ws)
		if err == nil && ok && w.Closed != nil {
			return true
		}
	}
	if a.cfg.Queue == nil {
		return false
	}
	w, ok, err := a.cfg.Queue.Workspace(queue.DefaultTeam, ws)
	return err == nil && ok && w.Closed != nil
}

// runPreview runs a fresh dry run, stores it as a changes.previewed event,
// and pages it. A session with nothing to apply answers without an event;
// only a real preview is stored.
func (a *API) runPreview(w http.ResponseWriter, r *http.Request, id string, ws ids.WorkspaceID, sandbox bool, before int64, limit int) {
	out := WorkspaceChangesPage{Workspace: id, Isolated: true, Store: a.cfg.Store.ID(),
		Files: []PreviewFile{}, Conflicts: []event.RefusedFile{}}
	if a.cfg.Queue == nil {
		a.changesWithoutQueue(w, id, sandbox)
		return
	}
	// Preview the workspace's changes. The stored preview is keyed by the
	// workspace id (the route's id), the one id its sessions share.
	job, turn, applies, err := a.cfg.Queue.Preview(queue.DefaultTeam, ws)
	out.Turn = turn
	switch {
	case errors.Is(err, queue.ErrNotFound):
		a.changesWithoutQueue(w, id, sandbox)
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
		out.Isolated, out.Why = false, applySessionClosed
		writeJSON(w, http.StatusOK, out)
		return
	case errors.Is(err, queue.ErrAllApplied):
		writeJSON(w, http.StatusOK, out)
		return
	case errors.Is(err, queue.ErrNoCopyYet):
		// The first turn has not recorded its copy yet. The workspace's sandbox
		// is the one switch: a sandbox workspace is isolated while its first turn
		// runs (the copy is coming), a direct one is not. The isolation setting
		// is only the backend and never turns the wall off (his rule, 2026-11-21).
		if !sandbox {
			out.Isolated, out.Why = false, changesNotIsolated
		}
		writeJSON(w, http.StatusOK, out)
		return
	case errors.Is(err, queue.ErrApplyBusy):
		writeError(w, http.StatusConflict, applyBusy)
		return
	case errors.Is(err, queue.ErrMachineOffline):
		// The runner is offline, so a fresh preview cannot run. Serve the last
		// stored preview rather than nothing, marked stale only when the
		// session has moved off it (otherwise it is still current); only when
		// there is none to serve is this a 503.
		if se, _, found, serr := a.cfg.Store.LatestPreview(id); serr == nil && found && se.Previewed != nil {
			stale, serr := a.previewStale(ws, se.Previewed)
			if serr != nil {
				a.internal(w)
				return
			}
			a.writePreviewPage(w, id, se, before, limit, stale)
			return
		}
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
		case errors.Is(r.Context().Err(), context.Canceled):
			// The client went away; there is nothing to answer.
		case err != nil && !errors.Is(err, context.DeadlineExceeded):
			a.internal(w)
		default:
			// The await's deadline (its own or the request's budget) passed.
			writeError(w, http.StatusGatewayTimeout, changesSlow)
		}
		return
	}
	// The preview job goes once read; store its result as the record's one
	// source, then page from it.
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
	// The preview records the appliable cursor it was planned against (turn
	// and applies), so the route reuses it only while that cursor holds. It is
	// keyed by the WORKSPACE (never the session): a preview is of a workspace's
	// changes, and keeping the session field empty keeps a workspace id out of
	// the session namespace, so no session reader ever sees a preview.
	e := event.Event{Kind: event.ChangesPreviewed, Workspace: id, Previewed: previewedFrom(applies, turn, j.Links.AppliesTurns, p)}
	e.ID, e.Time = newEventID(), a.cfg.Now().UTC()
	if _, _, err := a.cfg.Store.Append(e); err != nil {
		a.internal(w)
		return
	}
	// Running the preview took time; if the session moved off its cursor while
	// it ran, it is already out of date, so it is served with that said.
	stale, err := a.previewStale(ws, e.Previewed)
	if err != nil {
		a.internal(w)
		return
	}
	a.writePreviewPage(w, id, e, before, limit, stale)
}

// previewedFrom turns a preview job's result into the stored event's payload.
func previewedFrom(applies int, turn string, turns []string, p *queue.PreviewResult) *event.Previewed {
	pv := &event.Previewed{Applies: applies, Turn: turn, Turns: turns, Count: p.Count, Truncated: p.Truncated, DiffsCut: p.DiffsCut,
		Files: make([]event.PreviewedFile, 0, len(p.Files))}
	for _, f := range p.Files {
		pv.Files = append(pv.Files, event.PreviewedFile{Path: f.Path, Change: f.Change, Added: f.Added, Removed: f.Removed,
			Diff: f.Diff, DiffLeftOut: f.DiffLeftOut})
	}
	for _, c := range p.Conflicts {
		pv.Conflicts = append(pv.Conflicts, event.RefusedFile{Path: c.Path, Why: c.Why, Markers: c.Markers})
	}
	return pv
}

// writePreviewPage pages a stored changes.previewed event's file list. key is
// the workspace id the preview is of (the page's workspace field).
func (a *API) writePreviewPage(w http.ResponseWriter, key string, e event.Event, before int64, limit int, stale bool) {
	p := e.Previewed
	window, next := pageBefore(p.Files, before, limit)
	files := make([]PreviewFile, 0, len(window))
	for _, f := range window {
		files = append(files, PreviewFile{Path: f.Path, Change: f.Change, Added: f.Added, Removed: f.Removed,
			Diff: f.Diff, DiffLeftOut: f.DiffLeftOut})
	}
	conflicts := p.Conflicts
	if conflicts == nil {
		conflicts = []event.RefusedFile{}
	}
	at := e.Time
	why := ""
	if stale {
		why = changesStale
	}
	writeJSON(w, http.StatusOK, WorkspaceChangesPage{Workspace: key, Isolated: true,
		Unapplied: p.Count > 0 || len(conflicts) > 0, Turn: p.Turn, Turns: p.Turns, PreviewedAt: &at,
		Store: a.cfg.Store.ID(), Event: e.ID, Count: p.Count, Truncated: p.Truncated, DiffsCut: p.DiffsCut,
		Files: files, Conflicts: conflicts, NextBefore: next, Stale: stale, Why: why})
}

// workspaceOf is the workspace a session's turns run in, and whether the record
// knows the id as a session at all. It is the ONE door from a session id to a
// workspace id: store.WorkspaceOf (which gives a migrated session its own id as
// its workspace), then, for a session the store does not yet know but whose
// turn the queue holds, queue.SessionWorkspace. found is false only for an id
// neither knows as a session -- a workspace id among them -- and the caller
// answers 404, never a raw cast that would let a workspace id masquerade as a
// session and probe its own copy row.
func (a *API) workspaceOf(session string) (ids.WorkspaceID, bool, error) {
	ws, found, err := a.cfg.Store.WorkspaceOf(ids.SessionID(session))
	if err != nil || found {
		return ws, found, err
	}
	if a.cfg.Queue == nil {
		return "", false, nil
	}
	return a.cfg.Queue.SessionWorkspace(queue.DefaultTeam, ids.SessionID(session))
}

// changesWithoutQueue answers for a workspace the queue holds no copy row for
// yet: before its first turn ends, or one that works in the folder as it is (a
// hook or gateway workspace). The workspace's sandbox is the one switch -- a
// sandbox workspace is isolated with nothing unapplied yet, a direct one is not
// -- so the route reads ws.Sandbox, never the isolation setting (his rule,
// 2026-11-21). The caller validated the workspace exists.
func (a *API) changesWithoutQueue(w http.ResponseWriter, ws string, sandbox bool) {
	out := WorkspaceChangesPage{Workspace: ws, Isolated: sandbox, Store: a.cfg.Store.ID(),
		Files: []PreviewFile{}, Conflicts: []event.RefusedFile{}}
	if !sandbox {
		out.Why = changesNotIsolated
	}
	writeJSON(w, http.StatusOK, out)
}
