package api

// PATCH /api/v0/sessions/{id}: name a session, or close it. Each rename is a
// session.renamed event, so the earlier names stay in the record; the latest
// names the session in the snapshot (store.Rename). A close ends the
// session's work: its runner removes its copy (jobs.Server.CloseSession).

import (
	"errors"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/store"
)

// SessionUpdate is the body of PATCH /api/v0/sessions/{id}: title, closed, or
// both.
type SessionUpdate struct {
	Title  *string `json:"title,omitempty" doc:"The session's new name: 1 to 120 characters on one line. Spaces at either end are dropped."`
	Closed *bool   `json:"closed,omitempty" doc:"true closes the session: it takes no more turns, and its runner removes its copy and its home. false is refused: a closed session stays closed."`
}

// Renamed is the answer to PATCH /api/v0/sessions/{id}.
type Renamed struct {
	ID       string `json:"id"`
	Seq      int64  `json:"seq,omitempty" doc:"The seq of the session.renamed stored. Absent when no title was sent."`
	Title    string `json:"title,omitempty" doc:"The new name, as stored."`
	OldTitle string `json:"old_title,omitempty" doc:"The name the session had before. Absent when it had none."`
	Closed   bool   `json:"closed,omitempty" doc:"The session is closed: its copy is being removed."`
}

const (
	badRename    = "the body must be one JSON object with title, closed, or both"
	titleEmpty   = "title must hold at least one character that is not a space"
	titleTooLong = "title is at most 120 characters"
	titleNotLine = "title is one line: no newline, line separator, tab or other control character"
)

// breaksLine says r ends a line or is a control character. Unicode's line and
// paragraph separators are neither control characters nor allowed.
func breaksLine(r rune) bool {
	return unicode.IsControl(r) || r == '\u2028' || r == '\u2029'
}

// PATCH /api/v0/sessions/{id}
func (a *API) updateSession(w http.ResponseWriter, r *http.Request, id string) {
	var body SessionUpdate
	if !decodeObject(r, &body) || (body.Title == nil && body.Closed == nil) {
		writeError(w, http.StatusBadRequest, badRename)
		return
	}
	if body.Closed != nil && !*body.Closed {
		writeError(w, http.StatusBadRequest, "closed may only be true: a closed session stays closed")
		return
	}
	if body.Closed != nil && grantOf(r) != nil {
		writeError(w, http.StatusForbidden, "a turn token cannot close a session; only the person can")
		return
	}
	var t string
	if body.Title != nil {
		t = strings.TrimSpace(*body.Title)
		if msg := checkTitle(t); msg != "" {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
	}
	known, err := a.cfg.Store.Known(id)
	if err != nil {
		a.internal(w)
		return
	}
	var out Renamed
	out.ID = id
	if body.Closed != nil {
		if a.cfg.Jobs == nil {
			writeError(w, http.StatusNotFound, "no such session")
			return
		}
		_, err := a.cfg.Jobs.CloseSession(id)
		switch {
		case errors.Is(err, queue.ErrNotFound):
			// The queue holds a session's copy once its first turn has
			// ended: before that, a session with a turn is busy.
			busy, err := a.cfg.Queue.SessionHasWork(queue.DefaultTeam, id)
			switch {
			case err != nil:
				a.internal(w)
			case busy:
				writeError(w, http.StatusConflict, "a turn of the session has not ended; wait for it or stop it")
			default:
				writeError(w, http.StatusNotFound, "no such session of this service")
			}
			return
		case errors.Is(err, queue.ErrSessionBusy):
			writeError(w, http.StatusConflict, "a turn of the session has not ended; wait for it or stop it")
			return
		case err != nil:
			a.internal(w)
			return
		}
		out.Closed = true
	}
	if body.Title == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	if !known {
		writeError(w, http.StatusNotFound, "no such session")
		return
	}
	ev, err := a.cfg.Store.Rename(event.Event{ID: newEventID(), Time: a.cfg.Now().UTC(), Session: id, Title: t})
	if err != nil {
		a.internal(w)
		return
	}
	out.Seq, out.Title, out.OldTitle = ev.Seq, ev.Title, ev.OldTitle
	writeJSON(w, http.StatusOK, out)
}

// checkTitle says what is wrong with a session's new name, or "".
func checkTitle(t string) string {
	switch {
	case t == "":
		return titleEmpty
	case utf8.RuneCountInString(t) > store.TitleRunes:
		return titleTooLong
	case strings.IndexFunc(t, breaksLine) >= 0:
		return titleNotLine
	}
	return ""
}
