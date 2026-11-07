package api

// GET /api/v0/sessions/{id}/search: text search over a session's stored
// history. GET /api/v0/calls/{id}/pieces/{n}: one piece of a call, byte for
// byte. Both are what a turn reads of its own session with its turn token
// (tokens.go), and what a cockpit reads without fetching a whole call.

import (
	"errors"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/staysup/agentbus/store"
)

const (
	defaultSearchLimit = 20
	maxSearchLimit     = 200
	// maxQuery bounds q, in bytes.
	maxQuery = 256
)

// pieceNumber is what {n} may be: a piece's index, or reply.
var pieceNumber = regexp.MustCompile(`^(0|[1-9][0-9]{0,5}|reply)$`)

const searchDoc = "Searches the session's stored history: the messages and input items of its model calls' requests, " +
	"tool calls and tool results among them, each searched once, in the first call that carried it; each call's kept reply; " +
	"and the text of the session's turn.started events (the prompt as sent), of a program's own events, and of a " +
	"model.responded whose reply is not searched as a reply (none is kept, or its call's request is not stored). System blocks, instructions and tool definitions (codex's additional_tools and tool_search_output items included) are not searched. " +
	"Only the string values inside a piece are read, decoded from JSON; ids, types, signatures and base64 data are skipped. " +
	"Case is ignored. Each hit names its turn, call and piece, and read is the path that returns the whole of it."

const callPieceDoc = "One piece of a stored call: n is the piece's index in GET /api/v0/calls/{id}/context's pieces, " +
	"from 0, or reply for the call's kept reply. The body is the piece's bytes exactly as stored, so their SHA-256 is the " +
	"piece's hash, served as application/json. X-Piece-Kind, X-Piece-Hash and X-Piece-Count (the request's number of " +
	"pieces) describe it, and X-Piece-Label holds its label when that is printable ASCII."

// GET /api/v0/sessions/<id>/search
func (a *API) search(w http.ResponseWriter, r *http.Request, session string) {
	q := r.URL.Query()
	text := q.Get("q")
	if vs := q["q"]; len(vs) != 1 || text == "" || len(text) > maxQuery || !utf8.ValidString(text) || strings.IndexByte(text, 0) >= 0 {
		writeError(w, http.StatusBadRequest, "q is required once: 1 to 256 bytes of UTF-8, with no NUL byte")
		return
	}
	limit := defaultSearchLimit
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 || n > maxSearchLimit {
			writeError(w, http.StatusBadRequest, "limit must be from 1 to 200")
			return
		}
		limit = n
	}
	res, err := a.cfg.Store.Search(store.SearchQuery{Session: session, Q: text, Before: q.Get("before"), Limit: limit,
		MaxBytes: store.SearchPageBytes, MaxScan: store.SearchScanBytes})
	if errors.Is(err, store.ErrSearchCursor) {
		writeError(w, http.StatusBadRequest, "before must be the next_before of a previous page")
		return
	}
	if err != nil {
		a.internal(w)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// printable is a label a header can carry as it is.
var printable = regexp.MustCompile(`^[ -~]{1,256}$`)

// GET /api/v0/calls/<id>/pieces/<n>
func (a *API) callPiece(w http.ResponseWriter, r *http.Request, exchange string) {
	which := path.Base(r.URL.Path) // the router matched it against pieceNumber
	n, _ := strconv.Atoi(which)
	p, count, err := a.cfg.Store.CallPiece(exchange, n, which == "reply")
	switch {
	case errors.Is(err, store.ErrNoCall):
		writeError(w, http.StatusNotFound, "no request body is stored for this call")
		return
	case errors.Is(err, store.ErrNoPiece):
		writeError(w, http.StatusNotFound, "this call has no piece with that number; GET its context for the count")
		return
	case errors.Is(err, store.ErrNoReply):
		writeError(w, http.StatusNotFound, "no reply is kept for this call")
		return
	case err != nil:
		a.internal(w)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(p.JSON)))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Piece-Kind", p.Kind)
	h.Set("X-Piece-Hash", p.Hash)
	h.Set("X-Piece-Count", strconv.Itoa(count))
	if printable.MatchString(p.Label) {
		h.Set("X-Piece-Label", p.Label)
	}
	if h.Get("Access-Control-Allow-Origin") != "" {
		// A browser cockpit can read only the headers named here.
		h.Set("Access-Control-Expose-Headers", "X-Piece-Kind, X-Piece-Hash, X-Piece-Count, X-Piece-Label")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(p.JSON)
}
