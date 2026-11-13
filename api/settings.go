package api

// The settings routes: the settings database's state, the policy, and the instructions every turn gets, read and
// written through the settings store (settings.Store), so a cockpit or a
// script can show and change them. The saved agents are settings too, at
// /api/v0/agents (agents.go).
//
// Every write names the version it read, in If-Match, with the ETag a GET
// sent. A write by someone else in between makes another version, and the
// write is refused with 412 rather than undo theirs.
//
// The store records each change itself: the change and its settings.changed
// event are saved in one transaction of the settings database, and the event
// is stored in the record from there (RecordSettings).
//
// Only the main token reads or writes settings. A turn token never does:
// these rows leave route.turn at mainOnly, and the turns token at false.

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/staysup/agentbus/event"
	"github.com/staysup/agentbus/settings"
	"github.com/staysup/agentbus/store"
)

// SettingsInfo is the body of GET /api/v0/settings.
type SettingsInfo struct {
	Version       int64      `json:"version" doc:"The settings version: it moves up by one with every change of the policy, the instructions or the saved agents. Each turn records the version it started under (settings_version on turn.started). Also the ETag."`
	Created       time.Time  `json:"created" doc:"When the settings database was made."`
	Changed       *time.Time `json:"changed,omitempty" doc:"When the settings last changed. Absent when they never did."`
	Imported      *time.Time `json:"imported,omitempty" doc:"When the settings files of 0.4 and before were imported into the database. Absent when the import has not run."`
	ImportedFiles []string   `json:"imported_files" doc:"The files the import read: permissions.toml, instructions.md and agents.json, those that were there. Empty for a new data folder."`
}

// settingsDB is a store that keeps a settings database: control's.
type settingsDB interface {
	Info() (settings.Info, error)
}

// SettingsEvent is the settings.changed event of a change.
func SettingsEvent(c settings.Change) event.Event {
	return event.Event{ID: c.ID, Kind: event.SettingsChanged, Time: c.Time.UTC(), Setting: c.Setting,
		Keys: c.Keys, File: c.File, SettingsVersion: c.Version}
}

// RecordSettings is the settings database's sink (settings.DB.SetSink): it
// stores a change's event in the record once, however often it is handed
// the same change.
func RecordSettings(st *store.Store) func(settings.Change) error {
	return func(c settings.Change) error {
		_, err := st.AppendOnce(SettingsEvent(c))
		return err
	}
}

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
	{method: "GET", path: "/api/v0/settings", op: "getSettings",
		tag:     "Settings",
		summary: "Read the settings' state",
		doc: "The settings database's state: the settings version, when the settings last changed, " +
			"and what the one-time import of the settings files read.\n\n" +
			"Control keeps the settings in settings.db in its data folder, reached only through this API. The defaults are in the code; " +
			"the database holds only what the person changed. On its first start, a 0.5 control copies its data folder beside it " +
			"(its name with the date and before-settings-db), then imports permissions.toml, instructions.md and agents.json once. " +
			"It never reads those files again. When a build changes a default, its first start usually writes the old value into the person's policy, " +
			"unless they set that key, so nothing changes for them; to take the new default, change that key. " +
			"A change meant to reach everyone is not kept that way: 0.5.2 turned agent_api.tools on and subagents off for every person who never set them.\n\nETag is the settings version.",
		replies:   []reply{ok(200, SettingsInfo{}, "The settings' state."), noSettings, fail(http.StatusInternalServerError, "The settings could not be read.")},
		versioned: true,
		serve:     plain((*API).getSettings)},
	{method: "GET", path: "/api/v0/settings/permissions", op: "getPermissions",
		tag:     "Settings",
		summary: "Read the policy",
		doc: "The policy: what a turn may do without a person to ask, the limits on work agents ask for, and what a turn token may call. " +
			"Every value is filled: a key the person never changed reads as its default. Folders are as written, ~ and all: " +
			"they name folders on the machine that runs the turn.\n\n" +
			"ETag is the policy's version: send it back in If-Match to change it.",
		replies:   []reply{ok(200, settings.Policy{}, "The policy."), settingsBroken, noSettings},
		versioned: true,
		serve:     plain((*API).getPermissions)},
	{method: "PUT", path: "/api/v0/settings/permissions", op: "putPermissions",
		tag:     "Settings",
		summary: "Change the policy",
		doc: "Replace the policy whole. A key left out takes its default. The next turn runs under it, and a turn token's next request.\n\n" +
			versionDoc + "The settings database keeps only the keys whose value differs from the defaults. " +
			"The change and its settings.changed event, which names the keys that changed and never their values, are saved together. " +
			"A PUT that changes nothing makes no event and no new settings version. " +
			"A folder is checked for shape here, and for being there when a turn runs. " +
			"A body from an older client that holds sandbox.read is read with that value as home_files, unless it sets home_files too.",
		body: settings.Policy{},
		replies: []reply{ok(200, settings.Policy{}, "The policy, as stored."),
			fail(http.StatusBadRequest, "A value is refused; error names the key."), settingsReadOnly, settingsChanged, settingsNoMatch, noSettings,
			fail(http.StatusInternalServerError, "The setting could not be written.")},
		versioned: true,
		serve:     plain((*API).putPermissions)},
	{method: "PATCH", path: "/api/v0/settings/permissions", op: "patchPermissions",
		tag:     "Settings",
		summary: "Change some policy keys",
		doc: "Change only the keys the body holds, as nested JSON such as {\"agent_api\":{\"tools\":false}}. A key left out keeps what it holds now. " +
			"Each key sent is kept as the person's own, even when its value is the default, so a later build that changes that default keeps it. " +
			"This is how the cockpit's Settings saves: only what the person changed, never a full copy with the defaults filled in. " +
			"A key sent as null is no change: a key cannot be returned to the default here; PUT replaces the policy whole.\n\n" +
			versionDoc + "The change and its settings.changed event, which names the keys sent whose stored value changed, are saved together. " +
			"A PATCH that changes nothing makes no event. A stored policy that cannot be used is refused here: replace it with PUT.",
		body: settings.Policy{},
		replies: []reply{ok(200, settings.Policy{}, "The policy, as stored, every value filled."),
			fail(http.StatusBadRequest, "A value is refused; error names the key."), settingsReadOnly, settingsChanged, settingsNoMatch, noSettings,
			fail(http.StatusInternalServerError, "The setting could not be written.")},
		versioned: true,
		serve:     plain((*API).patchPermissions)},
	{method: "GET", path: "/api/v0/settings/instructions", op: "getInstructions",
		tag:     "Settings",
		summary: "Read the instructions",
		doc: "The text every turn gets as an instruction of its agent's own (Claude Code's --append-system-prompt, codex's developer_instructions). " +
			"The default text until the person changes it.\n\nETag is its version, as for the policy.",
		replies:   []reply{ok(200, InstructionsBody{}, "The instructions."), settingsBroken, noSettings},
		versioned: true,
		serve:     plain((*API).getInstructions)},
	{method: "PUT", path: "/api/v0/settings/instructions", op: "putInstructions",
		tag:     "Settings",
		summary: "Change the instructions",
		doc: "Replace the instructions. The next turn gets them. Keep them the same across a session's turns where you can: " +
			"own replay and the vendor's prompt cache rely on it.\n\n" + versionDoc +
			"The change and its settings.changed event (keys: text, never the text itself) are saved together.",
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

// PATCH /api/v0/settings/permissions
func (a *API) patchPermissions(w http.ResponseWriter, r *http.Request) {
	match, ok := a.matched(w, r)
	if !ok {
		return
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "the body could not be read")
		return
	}
	change, err := settings.DecodePolicyChange(b)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p, v, err := settings.ChangePolicy(a.cfg.Settings, change, match)
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

// settingsInfo is the body of GET /api/v0/settings.
func settingsInfo(in settings.Info) SettingsInfo {
	out := SettingsInfo{Version: in.Version, Created: in.Created.UTC(), ImportedFiles: append([]string{}, in.Files...)}
	if !in.Changed.IsZero() {
		t := in.Changed.UTC()
		out.Changed = &t
	}
	if !in.Imported.IsZero() {
		t := in.Imported.UTC()
		out.Imported = &t
	}
	return out
}

// GET /api/v0/settings
func (a *API) getSettings(w http.ResponseWriter, _ *http.Request) {
	db, ok := a.cfg.Settings.(settingsDB)
	if !ok {
		writeError(w, http.StatusNotFound, "this service keeps no settings database: a runner's settings are the server's")
		return
	}
	in, err := db.Info()
	if err != nil {
		a.internal(w)
		return
	}
	setVersion(w, settings.Version(strconv.FormatInt(in.Version, 10)))
	writeJSON(w, http.StatusOK, settingsInfo(in))
}
