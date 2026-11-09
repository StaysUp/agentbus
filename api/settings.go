package api

// The settings routes: the policy (permissions.toml) and the instructions
// every turn gets (instructions.md), read and written through the settings
// store (settings.Store), so a cockpit or a script can show and change them.
//
// Every write names the version it read, in If-Match, with the ETag a GET
// sent. A file the person changed by hand in between has another version, and
// the write is refused with 412 rather than undo their edit.
//
// Only the main token reads or writes settings. A turn token never does:
// these rows leave route.turn at mainOnly, and the turns token at false.

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/staysup/agentbus/settings"
)

// MaxSettingsBody bounds a settings write: 64 KiB of instructions, escaped as
// JSON.
const MaxSettingsBody = 512 << 10

// InstructionsBody is the body of GET and PUT /api/v0/settings/instructions.
type InstructionsBody struct {
	Text string `json:"text" doc:"The text every turn gets as an instruction of its agent's own, before {session} becomes the turn's session id. At most 64 KiB of UTF-8, with no NUL. Empty gives no instructions."`
}

var (
	settingsBroken   = fail(http.StatusServiceUnavailable, "What is stored cannot be used: error says why. ETag is the version of what is there, so a PUT can replace it.")
	settingsChanged  = fail(http.StatusPreconditionFailed, "The setting changed since the version in If-Match was read, perhaps by hand. GET it again and apply your change to what is there now.")
	settingsNoMatch  = fail(http.StatusPreconditionRequired, "If-Match is missing.")
	settingsReadOnly = fail(http.StatusConflict, "This service keeps no settings of its own: a runner's are the server's. Change them on the server.")
	noSettings       = fail(http.StatusNotFound, "This service has no settings store.")
)

const versionDoc = "Send If-Match with the ETag of your GET, or * to replace whatever is there. "

var settingsRoutes = []route{
	{method: "GET", path: "/api/v0/settings/permissions", op: "getPermissions",
		tag:     "Settings",
		summary: "Read the policy",
		doc: "The policy: what a turn may do without a person to ask, the limits on work agents ask for, and what a turn token may call. " +
			"Every value is filled: a key left out of permissions.toml reads as its default. Folders are as written, ~ and all: " +
			"they name folders on the machine that runs the turn.\n\n" +
			"ETag is the policy's version: send it back in If-Match to change it. An edit of the file by hand changes it too.",
		replies:   []reply{ok(200, settings.Policy{}, "The policy."), settingsBroken, noSettings},
		versioned: true,
		serve:     plain((*API).getPermissions)},
	{method: "PUT", path: "/api/v0/settings/permissions", op: "putPermissions",
		tag:     "Settings",
		summary: "Change the policy",
		doc: "Replace the policy whole. A key left out takes its default. The next turn runs under it, and a turn token's next request.\n\n" +
			versionDoc + "The file is rewritten in the starter's layout with its comments; comments of your own in it are not kept. " +
			"A folder is checked for shape here, and for being there when a turn runs.",
		body: settings.Policy{},
		replies: []reply{ok(200, settings.Policy{}, "The policy, as stored."),
			fail(http.StatusBadRequest, "A value is refused; error names the key."), settingsReadOnly, settingsChanged, settingsNoMatch, noSettings,
			fail(http.StatusInternalServerError, "The setting could not be written.")},
		versioned: true,
		serve:     plain((*API).putPermissions)},
	{method: "GET", path: "/api/v0/settings/instructions", op: "getInstructions",
		tag:     "Settings",
		summary: "Read the instructions",
		doc: "The text every turn gets as an instruction of its agent's own (Claude Code's --append-system-prompt, codex's developer_instructions). " +
			"Without instructions.md, the default text.\n\nETag is its version, as for the policy.",
		replies:   []reply{ok(200, InstructionsBody{}, "The instructions."), settingsBroken, noSettings},
		versioned: true,
		serve:     plain((*API).getInstructions)},
	{method: "PUT", path: "/api/v0/settings/instructions", op: "putInstructions",
		tag:     "Settings",
		summary: "Change the instructions",
		doc: "Replace the instructions. The next turn gets them. Keep them the same across a session's turns where you can: " +
			"own replay and the vendor's prompt cache rely on it.\n\n" + versionDoc,
		body: InstructionsBody{},
		replies: []reply{ok(200, InstructionsBody{}, "The instructions, as stored."),
			fail(http.StatusBadRequest, "The text is refused; error says why."), settingsReadOnly, settingsChanged, settingsNoMatch, noSettings,
			fail(http.StatusInternalServerError, "The setting could not be written.")},
		versioned: true,
		serve:     plain((*API).putInstructions)},
}

// setVersion sends v as the ETag.
func setVersion(w http.ResponseWriter, v settings.Version) {
	if v != "" {
		w.Header().Set("ETag", strconv.Quote(string(v)))
		// A page on another origin reads it only when it is exposed.
		w.Header().Set("Access-Control-Expose-Headers", "ETag")
	}
}

// ifMatch is the version a write names, and false when it names none.
func ifMatch(r *http.Request) (settings.Version, bool) {
	m := strings.TrimSpace(r.Header.Get("If-Match"))
	switch {
	case m == "":
		return "", false
	case m == "*":
		return settings.Any, true
	}
	v, err := strconv.Unquote(m)
	if err != nil || v == "" || v == string(settings.Any) {
		return "", false
	}
	return settings.Version(v), true
}

// settingsRead answers a read that failed.
func (a *API) settingsRead(w http.ResponseWriter, err error) {
	var se *settings.Error
	if errors.As(err, &se) {
		setVersion(w, se.Version)
		writeError(w, http.StatusServiceUnavailable, se.Error())
		return
	}
	a.internal(w)
}

// settingsWrite answers a write that failed.
func (a *API) settingsWrite(w http.ResponseWriter, err error) {
	var inv *settings.Invalid
	switch {
	case errors.As(err, &inv):
		writeError(w, http.StatusBadRequest, inv.Why)
	case errors.Is(err, settings.ErrChanged):
		writeError(w, http.StatusPreconditionFailed, "the setting changed since the version in If-Match was read, perhaps by hand; GET it again and apply your change to what is there now")
	case errors.Is(err, settings.ErrReadOnly):
		writeError(w, http.StatusConflict, "this service keeps no settings of its own: a runner's are the server's; change them on the server")
	default:
		writeError(w, http.StatusInternalServerError, "the setting could not be written")
	}
}

// matched is the If-Match of a write, or false once it has answered.
func (a *API) matched(w http.ResponseWriter, r *http.Request) (settings.Version, bool) {
	if a.cfg.Settings == nil {
		writeError(w, http.StatusNotFound, "this service has no settings store")
		return "", false
	}
	v, ok := ifMatch(r)
	if !ok {
		writeError(w, http.StatusPreconditionRequired, "send If-Match with the ETag of your GET, or * to replace whatever is there")
	}
	return v, ok
}

// GET /api/v0/settings/permissions
func (a *API) getPermissions(w http.ResponseWriter, _ *http.Request) {
	if a.cfg.Settings == nil {
		writeError(w, http.StatusNotFound, "this service has no settings store")
		return
	}
	p, v, err := a.cfg.Settings.Policy()
	if err != nil {
		a.settingsRead(w, err)
		return
	}
	setVersion(w, v)
	writeJSON(w, http.StatusOK, p)
}

// PUT /api/v0/settings/permissions
func (a *API) putPermissions(w http.ResponseWriter, r *http.Request) {
	match, ok := a.matched(w, r)
	if !ok {
		return
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "the body could not be read")
		return
	}
	p, err := settings.DecodePolicy(b)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p, v, err := a.cfg.Settings.SetPolicy(p, match)
	if err != nil {
		a.settingsWrite(w, err)
		return
	}
	setVersion(w, v)
	writeJSON(w, http.StatusOK, p)
}

// GET /api/v0/settings/instructions
func (a *API) getInstructions(w http.ResponseWriter, _ *http.Request) {
	if a.cfg.Settings == nil {
		writeError(w, http.StatusNotFound, "this service has no settings store")
		return
	}
	text, v, err := a.cfg.Settings.Instructions()
	if err != nil {
		a.settingsRead(w, err)
		return
	}
	setVersion(w, v)
	writeJSON(w, http.StatusOK, InstructionsBody{Text: text})
}

// PUT /api/v0/settings/instructions
func (a *API) putInstructions(w http.ResponseWriter, r *http.Request) {
	match, ok := a.matched(w, r)
	if !ok {
		return
	}
	var body InstructionsBody
	if !decodeObject(r, &body) {
		writeError(w, http.StatusBadRequest, "the body must be one JSON object with text")
		return
	}
	v, err := a.cfg.Settings.SetInstructions(body.Text, match)
	if err != nil {
		a.settingsWrite(w, err)
		return
	}
	setVersion(w, v)
	writeJSON(w, http.StatusOK, body)
}
