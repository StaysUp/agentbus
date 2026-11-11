package api

// The API describes itself: GET /api/v0/openapi.json is its OpenAPI 3.1
// description, generated from the route table at startup (spec.go), /docs an
// interactive reference built from it, /docs/llms.txt a short page for coding
// agents and /docs/reference.md the long one. The runner protocol has its own
// description, /api/v0/runner/openapi.json, and page, /docs/runner. All of it is in the binary,
// served without the token (none of it holds session data) and without the
// internet. The viewer is Redoc's standalone bundle, vendored in viewer/
// (THIRD_PARTY.md). Redoc has no "try it", so the page never handles the
// token.

import (
	_ "embed"
	"fmt"
	"net/http"
	"strings"

	"github.com/staysup/agentbus/docs"
)

//go:embed viewer/redoc.standalone.js
var redocJS []byte

// docsPage loads the viewer from this service and points it at the
// description. It names no other address, so it works offline.
//
// The side menu and each heading show the same short summary ("Start a
// turn"), and the method and path sit under the heading. hide-hostname
// shows the path without the description's server address, which names the
// default port, not this service's. Search is off: the menu is short.
const docsPage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s</title>
<style>body { margin: 0; padding: 0; }</style>
</head>
<body>
<redoc spec-url="%s" hide-hostname path-in-middle-panel side-nav-style="summary-only" disable-search></redoc>
<script src="/docs/redoc.standalone.js"></script>
</body>
</html>
`

// docsPolicy lets the page load only from this service. Redoc styles itself
// with inline styles; the Redocly logo it shows from its own site is not
// loaded. No worker is allowed: Redoc makes one only for its search, which
// the page turns off.
const docsPolicy = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; " +
	"font-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// public answers the paths that need no token. It reports false for every
// other path, which then goes through the token check.
func (a *API) public(w http.ResponseWriter, r *http.Request) bool {
	var body []byte
	var ctype string
	switch r.URL.Path {
	case "/api/v0/openapi.json":
		body, ctype = a.spec, "application/json"
	case "/api/v0/runner/openapi.json":
		body, ctype = a.runnerSpec, "application/json"
	case "/docs":
		body, ctype = []byte(fmt.Sprintf(docsPage, "agentbus client API", "/api/v0/openapi.json")), "text/html; charset=utf-8"
		w.Header().Set("Content-Security-Policy", docsPolicy)
	case "/docs/runner":
		body, ctype = []byte(fmt.Sprintf(docsPage, "agentbus runner protocol", "/api/v0/runner/openapi.json")), "text/html; charset=utf-8"
		w.Header().Set("Content-Security-Policy", docsPolicy)
	case "/docs/llms.txt":
		body, ctype = docs.LLMs, "text/markdown; charset=utf-8"
	case "/docs/reference.md":
		body, ctype = docs.Reference, "text/markdown; charset=utf-8"
	case "/docs/redoc.standalone.js":
		body, ctype = redocJS, "text/javascript; charset=utf-8"
	default:
		if !isDocs(r.URL.Path) {
			return false
		}
		writeError(w, http.StatusNotFound, "no such docs path")
		return true
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Del("Content-Security-Policy")
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return true
	}
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(body)
	}
	return true
}

// isDocs reports whether a path is /docs or under it. Only the bare path is
// ours: /s/<session>/docs is not.
func isDocs(p string) bool { return p == "/docs" || strings.HasPrefix(p, "/docs/") }
