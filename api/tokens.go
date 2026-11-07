package api

// Turn tokens. POST /api/v0/turns gives each turn's child its own token in
// AGENTBUS_TOKEN, with the service's address in AGENTBUS_URL, so the agent can
// read its own session's history with curl. A turn token is read-only and
// limited to that one session: route.turn says which routes take one, and how
// each checks the session. The main token never reaches a child.
//
// Tokens live in memory only, keyed by the SHA-256 of the token, so the token
// itself is not kept. A token is removed when its turn's child exits, just
// before the turn's session.closed is stored, and on every path where the
// child never starts. A restart forgets every token: the turns the previous
// process started are closed by CloseStaleHolds, and none of their tokens
// works in the new process. At most MaxRuns tokens exist at once.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
)

// grant is what one turn token may read: its own session. turn names the turn
// it was minted for, for the log of a refusal.
type grant struct {
	session, turn string
}

// turnTokens holds the live turn tokens.
type turnTokens struct {
	mu sync.Mutex
	m  map[[32]byte]grant
}

// turnTokenPrefix marks a turn token, so a person who finds one in a log can
// tell it from the main token.
const turnTokenPrefix = "abt_"

// mint makes a token for one turn of session: 32 random bytes, 256 bits.
func (t *turnTokens) mint(session, turn string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := turnTokenPrefix + hex.EncodeToString(b)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[[32]byte]grant{}
	}
	t.m[sha256.Sum256([]byte(tok))] = grant{session, turn}
	return tok, nil
}

// revoke removes a token. Removing one twice, or "", does nothing.
func (t *turnTokens) revoke(tok string) {
	if tok == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.m, sha256.Sum256([]byte(tok)))
}

// lookup finds a live token's grant. The map is keyed by the token's hash, so
// a lookup's timing says nothing about how close a guess was.
func (t *turnTokens) lookup(tok string) (grant, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	g, ok := t.m[sha256.Sum256([]byte(tok))]
	return g, ok
}

// live counts the live tokens, for tests.
func (t *turnTokens) live() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.m)
}

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
func (a *API) allowTurn(w http.ResponseWriter, r *http.Request, rt *route, id string, g grant) bool {
	refuse := func(msg string) bool {
		writeError(w, http.StatusForbidden, msg)
		return false
	}
	switch rt.turn {
	case ownSessionPath:
		if id != g.session {
			return refuse(turnSessionRefused)
		}
	case ownSessionQuery:
		q := r.URL.Query()
		if vs, ok := q["session"]; ok {
			if len(vs) != 1 || vs[0] != g.session {
				return refuse(turnSessionRefused)
			}
			break
		}
		q.Set("session", g.session)
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
		if session != g.session {
			return refuse(turnCallRefused)
		}
	default:
		return refuse(turnWriteRefused)
	}
	return true
}

// turnEnv is env with AGENTBUS_URL and AGENTBUS_TOKEN set to base and tok,
// replacing any the service's own environment had. env nil means the
// service's own environment, as exec.Cmd reads it.
func turnEnv(env, parent []string, base, tok string) []string {
	if env == nil {
		env = parent
	}
	set := []string{"AGENTBUS_URL=" + base, "AGENTBUS_TOKEN=" + tok}
	out := make([]string, 0, len(env)+len(set))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if k == "AGENTBUS_URL" || k == "AGENTBUS_TOKEN" {
			continue
		}
		out = append(out, kv)
	}
	return append(out, set...)
}
