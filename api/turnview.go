package api

// A turn as the API answers it. The record (package store) holds what a turn
// did once it started; the queue holds the turn from the moment it is asked
// for, and for a review its verdict. GET /api/v0/turns and /turns/{id} join
// the two, so one turn id answers in every state: queued, running, finished,
// failed or stopped.

import (
	"errors"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/store"
)

// Turn is one turn: what the record says of it, and what its job adds.
type Turn struct {
	store.Turn
	Kind     string           `json:"kind" doc:"turn, or review: a turn that reviews another, by POST /api/v0/turns/{id}/review."`
	Reviews  string           `json:"reviews,omitempty" doc:"For a review: the turn it reviews."`
	Verdict  *string          `json:"verdict,omitempty" doc:"For a review that has ended: pass, fail or unknown. Any P1 finding makes it fail."`
	Findings *[]queue.Finding `json:"findings,omitempty" doc:"For a review that has ended: what the reviewer named, [] when it named nothing. Absent for any other turn."`
	Asked    *time.Time       `json:"asked,omitempty" doc:"When the turn was asked for. Absent for a turn this service did not run."`
	Ahead    int              `json:"ahead,omitempty" doc:"For a queued turn: how many of its session's turns are before it."`
}

// TurnList is a page of turns.
type TurnList struct {
	Store string `json:"store" doc:"The store id. Send it back as store."`
	Seq   int64  `json:"seq" doc:"The seq the list was computed at. Send it back as after to stream from here."`
	Turns []Turn `json:"turns" doc:"The first page: the queued turns, then the turns that started, newest first. Later pages: started turns only. A turn that ended before it started is among the started turns, by when it was asked for, on one page only; it does not count toward limit."`
	// NextBefore is the before for the next, older page.
	NextBefore *int64 `json:"next_before" doc:"The before for the next, older page, or null when there are no older turns. A page stops at limit turns or at 4 MiB of rows, whichever comes first."`
}

// OneTurn is the answer to GET /api/v0/turns/<id>.
type OneTurn struct {
	Store string `json:"store" doc:"The store id. Send it back as store."`
	Seq   int64  `json:"seq" doc:"The seq the turn was computed at. Send it back as after to stream from here."`
	Turn  Turn   `json:"turn"`
}

const (
	defaultTurnLimit = 100
	maxTurnLimit     = 1000
)

// turnSession is the session a job's turn runs in: a review's own, which
// control named for it, or the turn job's.
func turnSession(j queue.Job) string {
	if j.Kind == queue.KindReview {
		return j.Links.ReviewSession
	}
	return j.Session
}

// view is t with what its job adds. j is nil for a turn no job of this
// service ran.
func (a *API) view(t store.Turn, j *queue.Job) Turn {
	v := Turn{Turn: t, Kind: queue.KindTurn}
	if j == nil {
		return v
	}
	v.Kind = j.Kind
	asked := j.Created
	v.Asked = &asked
	// One rule: a turn runs until its job has ended, unless it was stopped.
	// The record says a turn ended when its agent exits; its job ends a
	// moment later, once its runner has reported its changes. Until then the
	// turn reads running, so a client that waits for an end can apply or
	// review at once. A stopped turn reads stopped once its tree has ended,
	// as the stop says.
	if j.Ended == nil && v.State != queue.TurnQueued && v.State != queue.TurnStopped {
		v.State, v.Ended = queue.TurnRunning, nil
	}
	if j.Kind == queue.KindReview {
		v.Verdict = j.Verdict
		if j.Verdict != nil {
			// A list once the review has ended, empty when it named nothing.
			f := j.Findings
			if f == nil {
				f = []queue.Finding{}
			}
			v.Findings = &f
		}
		if r, err := a.cfg.Queue.Get(queue.DefaultTeam, j.Links.ReviewsJob); err == nil {
			v.Reviews = r.Turn
		}
	}
	return v
}

// waiting is the turn of a job whose turn has not started: queued, or ended
// before it started, with why.
func (a *API) waiting(j queue.Job) Turn {
	sp := j.Spec
	// No started: it has not started. asked says when it was asked for.
	t := store.Turn{ID: j.Turn, Session: turnSession(j), Harness: sp.Harness, AgentID: sp.AgentID, Model: sp.Model,
		State: queue.TurnState(j.State)}
	t.Prompt, t.PromptTruncated = previewText(sp.Prompt)
	// Who asked for it, as its turn.started will say once it starts. The
	// job id stays out: it is the queue's, not the API's.
	switch l := j.Links; {
	case l.ResultOf != nil:
		t.Origin = &event.Origin{Kind: event.OriginResult, Session: l.ResultOf.Session, Turn: l.ResultOf.Turn,
			TurnKind: l.ResultOf.Kind, Agent: l.ResultOf.Agent}
	case l.Asker != nil:
		t.Origin = &event.Origin{Kind: event.OriginAsked, Session: l.Asker.Session, Turn: l.Asker.Turn}
	}
	if j.Ended != nil {
		t.Ended = j.Ended
		res, _ := queue.ReadResult(j.Result)
		t.Error = firstNonEmpty(res.Refused, res.Error)
		if t.State == queue.TurnStopped {
			t.Stopped = true
		}
	}
	v := a.view(t, &j)
	if j.State == queue.Queued && j.Kind == queue.KindTurn {
		v.Ahead, _ = a.turnsAhead(j.ID)
	}
	return v
}

// previewText is a prompt's first store.PromptPreviewRunes characters.
func previewText(s string) (string, bool) {
	if utf8.RuneCountInString(s) <= store.PromptPreviewRunes {
		return s, false
	}
	n := 0
	for i := range s {
		if n == store.PromptPreviewRunes {
			return s[:i], true
		}
		n++
	}
	return s, false
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// started says the job's turn has started: its record holds it.
func started(j queue.Job) bool { return startedTurn(j) != "" }

// GET /api/v0/turns: a page of turns, newest first.
func (a *API) turns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tq := store.TurnQuery{Limit: defaultTurnLimit}
	if q.Has("session") {
		tq.Session = q.Get("session")
		if !sessionID.MatchString(tq.Session) {
			writeError(w, http.StatusBadRequest, "session must match "+sessionID.String())
			return
		}
	}
	if q.Has("before") {
		n, err := strconv.ParseInt(q.Get("before"), 10, 64)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "before must be a whole number, 1 or more")
			return
		}
		tq.Before = n
	}
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 || n > maxTurnLimit {
			writeError(w, http.StatusBadRequest, "limit must be from 1 to 1000")
			return
		}
		tq.Limit = n
	}
	list, err := a.cfg.Store.Turns(tq)
	if err != nil {
		a.internal(w)
		return
	}
	out := TurnList{Store: list.Store, Seq: list.Seq, Turns: []Turn{}, NextBefore: list.NextBefore}
	if a.cfg.Queue != nil && tq.Before == 0 {
		waiting, err := a.cfg.Queue.Waiting(queue.DefaultTeam)
		if err != nil {
			a.internal(w)
			return
		}
		for _, j := range waiting {
			if !started(j) && (tq.Session == "" || turnSession(j) == tq.Session) {
				out.Turns = append(out.Turns, a.waiting(j))
			}
		}
	}
	jobs := map[string]queue.Job{}
	if a.cfg.Queue != nil && len(list.Turns) > 0 {
		ids := make([]string, len(list.Turns))
		for i, t := range list.Turns {
			ids[i] = t.ID
		}
		if jobs, err = a.cfg.Queue.JobsByTurns(queue.DefaultTeam, ids); err != nil {
			a.internal(w)
			return
		}
	}
	never, err := a.neverStarted(tq, list)
	if err != nil {
		a.internal(w)
		return
	}
	for _, t := range list.Turns {
		// A turn that ended before it started goes where it was asked for,
		// among the turns that started.
		for len(never) > 0 && never[0].Created.After(t.Started) {
			out.Turns = append(out.Turns, a.waiting(never[0]))
			never = never[1:]
		}
		var jp *queue.Job
		if j, ok := jobs[t.ID]; ok && turnSession(j) == t.Session {
			jp = &j
		}
		out.Turns = append(out.Turns, a.view(t, jp))
	}
	for _, j := range never {
		out.Turns = append(out.Turns, a.waiting(j))
	}
	writeJSON(w, http.StatusOK, out)
}

// neverStarted is the jobs whose turn ended before it started, such as a
// turn stopped while queued, that belong on this page of started turns. A
// page holds those asked for before the earliest start of every newer page's
// turns (to), and, when older pages follow, at or after the earliest start of
// this page's turns and every newer one (from). Pages go down by seq, and a
// later turn can have an earlier clock, so both bounds are minimums over all
// turns from the page's oldest up: each only moves back in time from page to
// page, and each such turn is on exactly one page. The record holds no such
// turn; the queue does.
func (a *API) neverStarted(tq store.TurnQuery, list store.TurnList) ([]queue.Job, error) {
	if a.cfg.Queue == nil {
		return nil, nil
	}
	var from, to time.Time
	if tq.Before > 0 {
		t, ok, err := a.cfg.Store.FirstStartSince(tq.Session, tq.Before)
		if err != nil {
			return nil, err
		}
		if ok {
			to = t
		}
	}
	if list.NextBefore != nil {
		t, ok, err := a.cfg.Store.FirstStartSince(tq.Session, list.Turns[len(list.Turns)-1].StartedSeq)
		if err != nil {
			return nil, err
		}
		if ok {
			from = t
		}
	}
	return a.cfg.Queue.NeverStarted(queue.DefaultTeam, tq.Session, from, to)
}

// GET /api/v0/turns/<id>: one turn, in any state. session picks one
// session's turn: the same turn id can be in two. Without it the newest is
// answered.
func (a *API) getTurn(w http.ResponseWriter, r *http.Request, id string) {
	t, found, err := a.oneTurn(w, r, id)
	if err != nil || !found {
		return
	}
	list := a.cfg.Store
	writeJSON(w, http.StatusOK, OneTurn{list.ID(), list.Last(), t})
}

// oneTurn reads turn id, as its session query picks it. It has answered when
// err is set or found is false.
func (a *API) oneTurn(w http.ResponseWriter, r *http.Request, id string) (Turn, bool, error) {
	tq := store.TurnQuery{Turn: id, Limit: 1}
	if q := r.URL.Query(); q.Has("session") {
		tq.Session = q.Get("session")
		if !sessionID.MatchString(tq.Session) {
			writeError(w, http.StatusBadRequest, "session must match "+sessionID.String())
			return Turn{}, false, nil
		}
	}
	list, err := a.cfg.Store.Turns(tq)
	if err != nil {
		a.internal(w)
		return Turn{}, false, err
	}
	var j *queue.Job
	if a.cfg.Queue != nil {
		got, err := a.cfg.Queue.JobByTurn(queue.DefaultTeam, id)
		switch {
		case err == nil:
			if tq.Session == "" || turnSession(got) == tq.Session {
				j = &got
			}
		case !errors.Is(err, queue.ErrNotFound):
			a.internal(w)
			return Turn{}, false, err
		}
	}
	switch {
	case len(list.Turns) > 0:
		if j != nil && turnSession(*j) != list.Turns[0].Session {
			j = nil
		}
		return a.view(list.Turns[0], j), true, nil
	case j != nil:
		return a.waiting(*j), true, nil
	}
	writeError(w, http.StatusNotFound, noTurn)
	return Turn{}, false, nil
}

// sessionOfTurn is the session of turn id that a request picks: the session
// query when sent, else the turn's newest. "" when there is no such turn.
func (a *API) sessionOfTurn(id, session string) (string, error) {
	if session != "" {
		return session, nil
	}
	list, err := a.cfg.Store.Turns(store.TurnQuery{Turn: id, Limit: 1})
	if err != nil {
		return "", err
	}
	if len(list.Turns) > 0 {
		return list.Turns[0].Session, nil
	}
	if a.cfg.Queue == nil {
		return "", nil
	}
	j, err := a.cfg.Queue.JobByTurn(queue.DefaultTeam, id)
	if errors.Is(err, queue.ErrNotFound) {
		return "", nil
	}
	return turnSession(j), err
}

// callOfTurn serves a route on one model call of turn {id}: {call} must be one
// of the turn's calls.
func callOfTurn(h func(*API, http.ResponseWriter, *http.Request, string)) func(*API, http.ResponseWriter, *http.Request, string) {
	return func(a *API, w http.ResponseWriter, r *http.Request, turn string) {
		call := r.PathValue("call")
		// The call must be of the turn in the session the request names, or
		// the turn's newest: the session a turn token's check read.
		session, err := a.sessionOfTurn(turn, r.URL.Query().Get("session"))
		if err != nil {
			a.internal(w)
			return
		}
		in, err := a.cfg.Store.InTurn(call, session, turn)
		if err != nil {
			a.internal(w)
			return
		}
		if !in {
			writeError(w, http.StatusNotFound, "this turn has no such call; GET /api/v0/turns/{id}/diff lists its calls")
			return
		}
		h(a, w, r, call)
	}
}

// OneSession is the answer to GET /api/v0/sessions/<id>.
type OneSession struct {
	Store   string       `json:"store" doc:"The store id. Send it back as store."`
	Seq     int64        `json:"seq" doc:"The seq the session was computed at."`
	Session Session      `json:"session"`
	Copy    *SessionCopy `json:"copy" doc:"Where the session's turns run, on its runner. Null until the session's first turn on this service has ended."`
}

// SessionCopy is where a session's turns run: for debugging, and to know
// whether its copy is still there.
type SessionCopy struct {
	Runner  string     `json:"runner" doc:"The runner that holds it."`
	Kind    string     `json:"kind" doc:"copy or worktree: an isolated copy of the person's folder, removed when the session closes. folder: the person's folder as it is, never removed."`
	Name    string     `json:"name,omitempty" doc:"The copy's folder name on its runner, under the runner's data folder: sessions/<name>. Absent for kind folder."`
	Folder  string     `json:"folder_ref,omitempty" doc:"For kind folder: the person's folder, used as it is, by its reference."`
	Closed  *time.Time `json:"closed,omitempty" doc:"When the session was closed. It takes no more turns."`
	Removed *time.Time `json:"removed,omitempty" doc:"When its runner removed the copy."`
	Error   string     `json:"error,omitempty" doc:"Why the runner could not remove the copy."`
}

// GET /api/v0/sessions/<id>
func (a *API) getSession(w http.ResponseWriter, _ *http.Request, id string) {
	snap, err := a.cfg.Store.Snapshot()
	if err != nil {
		a.internal(w)
		return
	}
	out := OneSession{Store: snap.Store, Seq: snap.Seq}
	var rec store.Session
	found := false
	for _, s := range snap.Sessions {
		if s.ID == id {
			rec, found = s, true
			break
		}
	}
	closed := map[string]time.Time{}
	if a.cfg.Queue != nil {
		ws, ok, err := a.cfg.Queue.Workspace(queue.DefaultTeam, id)
		if err != nil {
			a.internal(w)
			return
		}
		if ok {
			out.Copy = &SessionCopy{Runner: ws.Runner, Kind: ws.Kind, Name: ws.Space, Folder: ws.FolderRef,
				Closed: ws.Closed, Removed: ws.Removed, Error: ws.RemovalError}
			if ws.Closed != nil {
				closed[id] = *ws.Closed
			}
			if !found {
				rec, found = store.Session{ID: id}, true
			}
		}
	}
	out.Session = a.withClosed(rec, closed)
	if !found {
		writeError(w, http.StatusNotFound, "no such session")
		return
	}
	writeJSON(w, http.StatusOK, out)
}
