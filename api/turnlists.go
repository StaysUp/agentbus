package api

// A turn's three lists, each on its own paged route, so none is cut: its
// file changes (GET /api/v0/turns/{id}/changes), its model calls
// (.../calls) and its tool calls (.../tools). GET /api/v0/turns/{id}/diff is
// the summary of the same turn, with none of these lists in it. Each list
// pages with the turn list's cursor: ?before=<n>&limit=<n> in, next_before
// out (see paging.go).

import (
	"errors"
	"net/http"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/jobs"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/store"
)

// TurnChangesPage is the answer to GET /api/v0/turns/{id}/changes.
type TurnChangesPage struct {
	Store          string       `json:"store" doc:"The store id."`
	Seq            int64        `json:"seq" doc:"The seq the page was read at."`
	Count          int          `json:"count" doc:"The files the turn added, modified or deleted; with truncated, see there."`
	Truncated      bool         `json:"truncated,omitempty" doc:"changes is not every change: files were left out past 1000, or to fit the runner's finish, and count still counts them; or the copy held more files than a checkpoint keeps, and count misses some too."`
	DiffsCut       bool         `json:"diffs_cut,omitempty" doc:"Some text files have no diff: all the turn's diffs together passed 1 MiB and the later ones were left out, each marked diff_left_out."`
	ContentLeftOut bool         `json:"content_left_out,omitempty" doc:"Some small text files have no content, to keep the result small."`
	Error          string       `json:"error,omitempty" doc:"The workspace could not be read at the end of the turn."`
	Changes        []FileChange `json:"changes" doc:"A page of the turn's changed files, sorted by path. Empty for a turn in a direct workspace (sandbox false): its changes go straight into the person's folder and are not recorded."`
	NextBefore     *int64       `json:"next_before" doc:"The before for the next, earlier page, or null when the page reaches the first file."`
}

// TurnCallsPage is the answer to GET /api/v0/turns/{id}/calls.
type TurnCallsPage struct {
	Store      string      `json:"store" doc:"The store id."`
	Seq        int64       `json:"seq" doc:"The seq the page was read at."`
	Count      int         `json:"count" doc:"The turn's model calls in all; calls holds one page."`
	Calls      []ModelCall `json:"calls" doc:"A page of the turn's model calls, oldest first."`
	NextBefore *int64      `json:"next_before" doc:"The before for the next, earlier page, or null when the page reaches the first call."`
}

// TurnToolsPage is the answer to GET /api/v0/turns/{id}/tools.
type TurnToolsPage struct {
	Store      string     `json:"store" doc:"The store id."`
	Seq        int64      `json:"seq" doc:"The seq the page was read at."`
	Count      int        `json:"count" doc:"The turn's tool calls in all; calls holds one page."`
	Calls      []ToolCall `json:"calls" doc:"A page of the turn's tool calls, oldest first, each with the commands it ran."`
	NextBefore *int64     `json:"next_before" doc:"The before for the next, earlier page, or null when the page reaches the first tool call."`
}

// turnByID resolves the turn id (and an optional ?session=) to the recorded
// turn, writing the 404 or 400 when it cannot.
func (a *API) turnByID(w http.ResponseWriter, r *http.Request, id string) (store.Turn, bool) {
	tq := store.TurnQuery{Turn: id, Limit: 1}
	if q := r.URL.Query(); q.Has("session") {
		tq.Session = q.Get("session")
		if !sessionID.MatchString(tq.Session) {
			writeError(w, http.StatusBadRequest, "session must match "+sessionID.String())
			return store.Turn{}, false
		}
	}
	list, err := a.cfg.Store.Turns(tq)
	if err != nil {
		a.internal(w)
		return store.Turn{}, false
	}
	if len(list.Turns) == 0 {
		writeError(w, http.StatusNotFound, noTurn)
		return store.Turn{}, false
	}
	return list.Turns[0], true
}

// turnRecordChanges reads a turn's file changes from the record, or nil when
// this service did not run it, the turn has not ended, or it has no queue.
func (a *API) turnRecordChanges(t store.Turn) (*event.TurnChanges, error) {
	if a.cfg.Queue == nil {
		return nil, nil
	}
	j, err := a.cfg.Queue.JobByTurn(queue.DefaultTeam, t.ID)
	switch {
	case errors.Is(err, queue.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, err
	}
	res, _ := queue.ReadResult(j.Result)
	if (j.Session != t.Session && res.Session != t.Session) || j.Ended == nil {
		return nil, nil
	}
	return jobs.Changes(a.cfg.Store, j)
}

// GET /api/v0/turns/{id}/changes: a page of the turn's file changes.
func (a *API) turnChanges(w http.ResponseWriter, r *http.Request, id string) {
	t, ok := a.turnByID(w, r, id)
	if !ok {
		return
	}
	before, limit, ok := pageParams(w, r)
	if !ok {
		return
	}
	c, err := a.turnRecordChanges(t)
	if err != nil {
		a.internal(w)
		return
	}
	changes, truncated, diffsCut, contentOut, cerr := fileChanges(c)
	// Count is the turn's full change count, which can be more than the files
	// the record kept (truncated); the page walks the files it kept.
	count := 0
	if c != nil {
		count = c.Count
	}
	window, next := pageBefore(changes, before, limit)
	if window == nil {
		window = []FileChange{}
	}
	writeJSON(w, http.StatusOK, TurnChangesPage{Store: a.cfg.Store.ID(), Seq: a.cfg.Store.Last(),
		Count: count, Truncated: truncated, DiffsCut: diffsCut, ContentLeftOut: contentOut, Error: cerr,
		Changes: window, NextBefore: next})
}

// GET /api/v0/turns/{id}/calls: a page of the turn's model calls.
func (a *API) turnCalls(w http.ResponseWriter, r *http.Request, id string) {
	t, ok := a.turnByID(w, r, id)
	if !ok {
		return
	}
	before, limit, ok := pageParams(w, r)
	if !ok {
		return
	}
	act, err := a.buildActivity(t.ID, t.Session)
	if err != nil {
		a.internal(w)
		return
	}
	window, next := pageBefore(act.Calls, before, limit)
	if window == nil {
		window = []ModelCall{}
	}
	writeJSON(w, http.StatusOK, TurnCallsPage{Store: a.cfg.Store.ID(), Seq: a.cfg.Store.Last(),
		Count: len(act.Calls), Calls: window, NextBefore: next})
}

// GET /api/v0/turns/{id}/tools: a page of the turn's tool calls.
func (a *API) turnTools(w http.ResponseWriter, r *http.Request, id string) {
	t, ok := a.turnByID(w, r, id)
	if !ok {
		return
	}
	before, limit, ok := pageParams(w, r)
	if !ok {
		return
	}
	act, err := a.buildActivity(t.ID, t.Session)
	if err != nil {
		a.internal(w)
		return
	}
	window, next := pageBefore(act.Tools, before, limit)
	if window == nil {
		window = []ToolCall{}
	}
	writeJSON(w, http.StatusOK, TurnToolsPage{Store: a.cfg.Store.ID(), Seq: a.cfg.Store.Last(),
		Count: len(act.Tools), Calls: window, NextBefore: next})
}
