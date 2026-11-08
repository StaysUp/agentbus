package api

// Turn tokens. POST /api/v0/turns gives each turn's child its own token in
// AGENTBUS_TOKEN, with the service's address in AGENTBUS_URL, so the agent can
// read its own session's history with curl. A turn token is read-only and
// limited to that one session: route.turn says which routes take one, and how
// each checks the session. The main token never reaches a child.
//
// The engine mints and keeps the tokens (package run), in memory only, as
// their SHA-256. This file says which routes take one, and how each checks
// the session.

import (
	"net/http"

	"github.com/staysup/agentbus/run"
)

// What a turn token may do on a route.
const (
	// mainOnly: a turn token is refused. The zero value, so a new route is
	// closed to turn tokens until it says otherwise.
	mainOnly = iota
	// ownSessionPath: {id} is a session id, and it must be the token's own.
	ownSessionPath
	// ownSessionQuery: the session query must be the token's own. Absent, it
	// is set to the token's own, so the reply holds that session only.
	ownSessionQuery
	// ownCall: {id} is a model call, and its stored request must belong to
	// the token's own session.
	ownCall
)

// Refusals a turn token gets. Fixed wording: they never quote the request.
const (
	turnWriteRefused   = "a turn token is read-only and reads only its own session's turns, events, calls and search; this route takes the main token"
	turnSessionRefused = "a turn token reads only its own session"
	turnCallRefused    = "a turn token reads only its own session's calls"
)

// allowTurn checks a turn token's request on rt, and says why not. It may set
// the session query to the token's own (ownSessionQuery).
func (a *API) allowTurn(w http.ResponseWriter, r *http.Request, rt *route, id string, g run.Grant) bool {
	refuse := func(msg string) bool {
		writeError(w, http.StatusForbidden, msg)
		return false
	}
	switch rt.turn {
	case ownSessionPath:
		if id != g.Session {
			return refuse(turnSessionRefused)
		}
	case ownSessionQuery:
		q := r.URL.Query()
		if vs, ok := q["session"]; ok {
			if len(vs) != 1 || vs[0] != g.Session {
				return refuse(turnSessionRefused)
			}
			break
		}
		q.Set("session", g.Session)
		r.URL.RawQuery = q.Encode()
	case ownCall:
		session, found, err := a.cfg.Store.CallSession(id)
		if err != nil {
			a.internal(w)
			return false
		}
		if !found {
			writeError(w, http.StatusNotFound, "no request body is stored for this call")
			return false
		}
		if session != g.Session {
			return refuse(turnCallRefused)
		}
	default:
		return refuse(turnWriteRefused)
	}
	return true
}
