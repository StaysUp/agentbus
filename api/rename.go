package api

// PATCH /api/v0/sessions/{id}: name a session, or archive it. Each rename is a
// session.renamed event, so the earlier names stay in the record; the latest
// names the session in the snapshot (store.Rename). Archive is only a view,
// recorded as a session.archived event (store.SetArchived) like a rename, so
// it hides the session from the default list and removes nothing. Close is
// retired (B14): closed:true maps to archive, so an old client keeps working
// without removing a copy.

import (
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/queue"
	"github.com/staysup/agentbus/store"
)

// SessionUpdate is the body of PATCH /api/v0/sessions/{id}: title, archived,
// closed, or a mix. closed is the retired name for archived.
type SessionUpdate struct {
	Title    *string `json:"title,omitempty" doc:"The session's new name: 1 to 120 characters on one line. Spaces at either end are dropped."`
	Archived *bool   `json:"archived,omitempty" doc:"true hides the session from the default list; false un-hides it. Only a view: the session keeps its copy, history and name, and can still take a turn. Reversible."`
	Closed   *bool   `json:"closed,omitempty" doc:"Retired: close is gone. true now archives the session (hides it, removes nothing); false is refused. Send archived instead."`
}

// Renamed is the answer to PATCH /api/v0/sessions/{id}.
type Renamed struct {
	ID       string `json:"id"`
	Seq      int64  `json:"seq,omitempty" doc:"The seq of the session.renamed stored. Absent when no title was sent."`
	Title    string `json:"title,omitempty" doc:"The new name, as stored."`
	OldTitle string `json:"old_title,omitempty" doc:"The name the session had before. Absent when it had none."`
	Archived bool   `json:"archived,omitempty" doc:"The session is archived: hidden from the default list, nothing removed."`
}

const (
	badRename    = "the body must be one JSON object with title, archived, closed, or a mix"
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
	if !decodeObject(r, &body) || (body.Title == nil && body.Archived == nil && body.Closed == nil) {
		writeError(w, http.StatusBadRequest, badRename)
		return
	}
	// Resolve the archive intent from archived (preferred) or the retired
	// closed. closed:true archives; closed:false is refused; a mix that
	// disagrees is refused.
	var archive *bool
	if body.Archived != nil {
		archive = body.Archived
	}
	if body.Closed != nil {
		if !*body.Closed {
			writeError(w, http.StatusBadRequest, "closed is retired and may only be true, which archives the session; send archived:false to un-hide one")
			return
		}
		if archive != nil && !*archive {
			writeError(w, http.StatusBadRequest, "archived:false and closed:true disagree")
			return
		}
		yes := true
		archive = &yes
	}
	var t string
	if body.Title != nil {
		var msg string
		if t, msg = normalizeTitle(*body.Title); msg != "" {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
	}
	known, err := a.cfg.Store.Known(id)
	if err != nil {
		a.internal(w)
		return
	}
	// A title needs the session in the record (to attach the session.renamed).
	// Check it before any archive, so a request carrying both does not change
	// archived and then fail the rename.
	if body.Title != nil && !known {
		writeError(w, http.StatusNotFound, "no such session")
		return
	}
	var out Renamed
	out.ID = id
	if archive != nil {
		// Archive is only a view, recorded as a session.archived event like a
		// rename, so it works on any session this service knows -- through the
		// record, or through the queue when its first turn is still queued and
		// has no record yet. It is never refused for busy work.
		exists := known
		if !exists && a.cfg.Queue != nil {
			hasWork, werr := a.cfg.Queue.SessionHasWork(queue.DefaultTeam, id)
			if werr != nil {
				a.internal(w)
				return
			}
			exists = hasWork
		}
		if !exists {
			writeError(w, http.StatusNotFound, "no such session")
			return
		}
		if _, _, err := a.cfg.Store.SetArchived(event.Event{ID: newEventID(), Time: a.cfg.Now().UTC(), Session: id, Archived: *archive}); err != nil {
			a.internal(w)
			return
		}
		out.Archived = *archive
	}
	if body.Title == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	// known is true here: checked above when a title was sent.
	ev, err := a.cfg.Store.Rename(event.Event{ID: newEventID(), Time: a.cfg.Now().UTC(), Session: id, Title: t})
	if err != nil {
		a.internal(w)
		return
	}
	out.Seq, out.Title, out.OldTitle = ev.Seq, ev.Title, ev.OldTitle
	writeJSON(w, http.StatusOK, out)
}

// normalizeTitle trims a session title and says what is wrong with it, or ""
// and the trimmed title. POST /api/v0/sessions and PATCH /api/v0/sessions/{id}
// both use it, so a title is validated and stored one way: a whitespace-only
// title is refused, and leading or trailing spaces are dropped (his rule,
// 2026-11-21: share one implementation, do not re-check titles per handler).
func normalizeTitle(raw string) (title, msg string) {
	title = strings.TrimSpace(raw)
	return title, checkTitle(title)
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
