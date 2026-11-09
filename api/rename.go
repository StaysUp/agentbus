package api

// PATCH /api/v0/sessions/{id}: give a session a name. Each rename is a
// session.renamed event, so the earlier names stay in the record; the latest
// names the session in the snapshot (store.Rename).

import (
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/store"
)

// RenameRequest is the body of PATCH /api/v0/sessions/{id}.
type RenameRequest struct {
	Title string `json:"title" doc:"The session's new name: 1 to 120 characters on one line. Spaces at either end are dropped."`
}

// Renamed is the answer to PATCH /api/v0/sessions/{id}: the session.renamed
// stored.
type Renamed struct {
	Seq      int64  `json:"seq"`
	ID       string `json:"id"`
	Title    string `json:"title" doc:"The new name, as stored."`
	OldTitle string `json:"old_title,omitempty" doc:"The name the session had before. Absent when it had none."`
}

const (
	badRename    = "the body must be one JSON object with title"
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
func (a *API) rename(w http.ResponseWriter, r *http.Request, id string) {
	var body RenameRequest
	if !decodeObject(r, &body) {
		writeError(w, http.StatusBadRequest, badRename)
		return
	}
	t := strings.TrimSpace(body.Title)
	switch {
	case t == "":
		writeError(w, http.StatusBadRequest, titleEmpty)
		return
	case utf8.RuneCountInString(t) > store.TitleRunes:
		writeError(w, http.StatusBadRequest, titleTooLong)
		return
	case strings.IndexFunc(t, breaksLine) >= 0:
		writeError(w, http.StatusBadRequest, titleNotLine)
		return
	}
	known, err := a.cfg.Store.Known(id)
	if err != nil {
		a.internal(w)
		return
	}
	if !known {
		writeError(w, http.StatusNotFound, "no such session")
		return
	}
	out, err := a.cfg.Store.Rename(event.Event{ID: newEventID(), Time: a.cfg.Now().UTC(), Session: id, Title: t})
	if err != nil {
		a.internal(w)
		return
	}
	writeJSON(w, http.StatusOK, Renamed{Seq: out.Seq, ID: out.ID, Title: out.Title, OldTitle: out.OldTitle})
}
