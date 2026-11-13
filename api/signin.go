package api

// The one-time sign-in link. agentbus up prints and opens
// http://127.0.0.1:<port>/?t=<code>; the cockpit, served from that same
// address, sends the code once to POST /api/v0/signin and gets the main token
// back, so nobody pastes a token. A code works once, for SignInTTL.
//
// Only the cockpit's own origin may use it. The route is answered before
// ServeHTTP's CORS step, so its reply never carries Access-Control-Allow-Origin
// and no preflight for it is answered: a page on another origin cannot read
// the token. It also refuses a request whose Origin is not this service's own
// address, or that a browser marks as cross-site, and it takes only
// application/json, which a page on another origin cannot send without a
// preflight.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// SignInPath is the route that trades a sign-in code for the main token.
const SignInPath = "/api/v0/signin"

// SignInTTL is how long a sign-in code works.
const SignInTTL = 10 * time.Minute

// maxCodes bounds the codes waiting at once; the oldest goes first.
const maxCodes = 32

// SignInRequest is POST /api/v0/signin's body.
type SignInRequest struct {
	Code string `json:"code" doc:"The code from the sign-in link's t parameter."`
}

// SignedIn is POST /api/v0/signin's reply.
type SignedIn struct {
	Token string `json:"token" doc:"The main token: send it as the bearer token on every other call."`
}

// signIns are the codes not yet used, by their SHA-256.
type signIns struct {
	mu    sync.Mutex
	codes map[[32]byte]time.Time // when each expires
}

// NewSignInCode makes a new one-time code. Only its hash is kept.
func (a *API) NewSignInCode() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	code := hex.EncodeToString(b)
	now := a.cfg.Now()
	a.signin.mu.Lock()
	defer a.signin.mu.Unlock()
	if a.signin.codes == nil {
		a.signin.codes = map[[32]byte]time.Time{}
	}
	var oldest [32]byte
	var oldestAt time.Time
	for k, exp := range a.signin.codes {
		if !now.Before(exp) {
			delete(a.signin.codes, k)
			continue
		}
		if oldestAt.IsZero() || exp.Before(oldestAt) {
			oldest, oldestAt = k, exp
		}
	}
	if len(a.signin.codes) >= maxCodes {
		delete(a.signin.codes, oldest)
	}
	a.signin.codes[sha256.Sum256([]byte(code))] = now.Add(SignInTTL)
	return code
}

// take uses up code: true once, while it has not expired.
func (a *API) take(code string) bool {
	sum := sha256.Sum256([]byte(code))
	a.signin.mu.Lock()
	defer a.signin.mu.Unlock()
	exp, ok := a.signin.codes[sum]
	if !ok {
		return false
	}
	delete(a.signin.codes, sum)
	return a.cfg.Now().Before(exp)
}

// signIn answers POST /api/v0/signin. ServeHTTP calls it after the Host
// check and before CORS.
func (a *API) signIn(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		notAllowed(w, "POST")
		return
	}
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "a sign-in code is taken only from this service's own address")
		return
	}
	ct, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
	if !strings.EqualFold(strings.TrimSpace(ct), "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "the body must be application/json")
		return
	}
	var req SignInRequest
	if !decodeObject(r, &req) || req.Code == "" {
		writeError(w, http.StatusBadRequest, "the body must be {\"code\": \"...\"}")
		return
	}
	if !a.take(req.Code) {
		writeError(w, http.StatusUnauthorized, "the sign-in code is wrong, used or expired; restart agentbus up for a new link")
		return
	}
	writeJSON(w, http.StatusOK, SignedIn{Token: a.cfg.Token})
}

// sameOrigin reports whether r comes from a page on this service's own
// address, or from no page at all (a program on this machine sends no
// Origin). A browser sends Origin on every POST, and Sec-Fetch-Site too.
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// Public reports whether ServeHTTP answers r without a token: the docs and
// every public row of the route table (the descriptions and the sign-in
// code's trade). The caller bounds how long such a
// request may take (cmd/agentbus, boundPublic), since anyone may send one.
func Public(r *http.Request) bool {
	p := r.URL.Path
	if isDocs(p) || p == SignInPath {
		return true
	}
	// By path, whatever the method: a public path answers every method
	// without a token, if only with 405.
	for i := range routes {
		if routes[i].public && routes[i].path == p {
			return true
		}
	}
	return false
}
