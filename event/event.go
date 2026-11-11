// Package event defines the vendor-neutral events the framework emits.
//
// Every vendor's traffic becomes the same shape. A program reads these and never
// learns which vendor or which harness produced them.
package event

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// Kind names what happened.
type Kind string

const (
	TurnStarted    Kind = "turn.started"
	ModelRequested Kind = "model.requested"
	ModelResponded Kind = "model.responded"
	ToolRequested  Kind = "tool.requested"
	ToolCompleted  Kind = "tool.completed"
	TurnCompleted  Kind = "turn.completed"

	// EventsDropped says that events were lost because the output did not keep
	// up. Dropped holds how many, and Sessions every session that lost one.
	EventsDropped Kind = "events.dropped"

	// SessionRegistered: written by older versions, when a launcher or a run
	// registered the session with its agent and folder. Nothing writes it now;
	// old stores keep theirs and readers still accept them.
	SessionRegistered Kind = "session.registered"
	// SessionOpened and SessionClosed: a run's child started or exited. Hold
	// names that child, so each opened has its own closed.
	SessionOpened Kind = "session.opened"
	SessionClosed Kind = "session.closed"

	// SchemaUnexpected: the vendor sent a shape the adapter does not know, such
	// as a new block type or usage field. Unexpected says what, as names only.
	SchemaUnexpected Kind = "schema.unexpected"

	// ReplayFailed: a model request of a replayed turn could not be given the
	// earlier turn's history, so the gateway refused it rather than send it
	// without that history. Error says why.
	ReplayFailed Kind = "replay.failed"

	// ToolRefused: the gateway refused a tool call of a run's turn, because
	// its command matched a forbid entry of permissions.toml. The agent did
	// not run it. Rule names the entry.
	ToolRefused Kind = "tool.refused"

	// SessionRenamed: the person, or a turn of the session's request tree,
	// gave the session a name (PATCH /api/v0/sessions/{id}). Title is the new
	// name and OldTitle the one it replaced. The latest names the session.
	SessionRenamed Kind = "session.renamed"

	// ToolsListed: control served a turn's tool list (the tool server's
	// tools/list, [agent_api] tools), once per turn. Server names the tool
	// server, Tools the tools it served, and ToolsHidden the harness's own
	// tools the turn does not get, with why. Names only, never a value.
	ToolsListed Kind = "tools.listed"

	// ContextAdded: what the run engine gave a turn besides the person's
	// words, stored once, just before its agent starts. Added holds each
	// piece: its kind, size and text. Settings and folder content, never a
	// secret.
	ContextAdded Kind = "context.added"

	// NetworkRefused: a closed-off agent asked to reach a host the policy
	// does not allow ([network] of permissions.toml), and the runner's proxy
	// refused it. Host is what it asked for; Error says why.
	NetworkRefused Kind = "network.refused"

	// The person's shell command in a session's copy, with no model call
	// (POST /api/v0/sessions/{id}/commands), as control records it: queued
	// in the session's lane, started by the session's runner, then ran (how
	// it ended and its output, capped) or failed (it did not run, or its
	// runner was lost). Command.ID ties them. No turn: a command sits between
	// turns, and the session's next turn is told of each one that ran.
	CommandQueued  Kind = "command.queued"
	CommandStarted Kind = "command.started"
	CommandRan     Kind = "command.ran"
	CommandFailed  Kind = "command.failed"

	// Control's own operations, in no session: a subscription connected or
	// failed to (Harness, and Error says why), a runner enrolled or stopped
	// answering (Runner), and a settings file changed (File). Never a value.
	SubscriptionConnected Kind = "subscription.connected"
	SubscriptionFailed    Kind = "subscription.failed"
	RunnerEnrolled        Kind = "runner.enrolled"
	RunnerGone            Kind = "runner.gone"
	SettingsChanged       Kind = "settings.changed"
)

// KindInfo says what one framework kind means and which fields it always
// carries, beyond those every stored event carries. The API's description is
// built from Kinds, so a kind added here is documented with no other edit.
type KindInfo struct {
	Kind  Kind
	Doc   string
	Needs []string // JSON field names
}

// Kinds lists every framework kind. A program's own kind, x.<program>.<name>,
// is not one of them. A test checks that every Kind constant is listed.
var Kinds = []KindInfo{
	{TurnStarted, "A person's message started a turn. Stored once per turn id; text holds their words; agent and folder name the run's agent and folder, empty when no run started the session. A turn started by POST /api/v0/turns has its turn.started stored by the API, with the prompt as sent, before its agent starts.", nil},
	{ModelRequested, "The harness sent a request to the model: model and tools_offered.", nil},
	{ModelResponded, "The model replied: status, text, stop_reason, usage, latency_ms, cost_usd, or error.", nil},
	{ToolRequested, "The model asked for a tool: tool_id, tool_name, tool_input.", []string{"tool_id"}},
	{ToolCompleted, "A tool's result reached the model: tool_id, tool_output, tool_error.", []string{"tool_id"}},
	{TurnCompleted, "The turn ended, from the model's stop reason: outcome and stop_reason.", []string{"outcome"}},
	{EventsDropped, "Events were lost because the output did not keep up: dropped, and sessions or all_sessions.", []string{"dropped"}},
	{SessionRegistered, "Older stores only: a launcher or a run registered the session, with agent and folder. Nothing writes it now.", []string{"session"}},
	{SessionOpened, "A run's child is starting: hold names it; agent and folder say what and where; turn is the turn it runs. Stored just before the child starts.", []string{"session", "hold"}},
	{SessionClosed, "A run's child exited: hold, exit_code, resume_id, turn, stopped when the person stopped the turn, and unsettled when the gateway could not confirm all of the turn's calls were stored. Also stored at start, with unsettled, for a child the previous service left running.", []string{"session"}},
	{SchemaUnexpected, "The vendor sent a shape the adapter does not know: unexpected, as names only. Show it as a quiet warning.", []string{"unexpected"}},
	{ReplayFailed, "A model request of a replayed turn (a turn.started with replay_from) could not be given the earlier turn's history: error says why, such as the harness's request layout not being one the gateway knows. The gateway refused that request with 400 and did not send it to the vendor, so the turn fails rather than continue without its history. Show it as an error.", []string{"error"}},
	{ToolRefused, "The gateway refused a tool call before the agent could run it, because its command matched a forbid entry of permissions.toml: tool_id, tool_name, tool_input, rule (the entry), and text (what the model is told in place of the output). The call's tool.requested is still stored, and its tool.completed carries that text. Show it as a warning.", []string{"tool_id", "rule"}},
	{ToolsListed, "Control served the turn's tool list: its agent's tool server (POST /api/v0/mcp, [agent_api] tools in permissions.toml) answered tools/list for the turn's token. Stored once per turn, at the first tools/list, however often the agent asks. server is the tool server's name, as tool names carry it (claude calls a tool mcp__agentbus__<name>); tools are the names it served, which the turn's token may call; tools_hidden are the harness's own tools the turn does not get, each with why. A turn with no tools.listed got no tool server, or its agent never asked for the list. Names only: never an input, a value or a token.", []string{"server"}},
	{ContextAdded, "What AgentBus gave the turn besides the person's words, the same for every harness: added, each piece with its kind, bytes and text. Kinds: agents_md (the folder's AGENTS.md, when the harness does not read it by itself), instructions (instructions.md, filled in), asked_by (who asked an asked agent for the work), mcp_instructions (the tool server's own instructions, when the turn has it) and commands (the shell commands the person ran in the session since its previous turn, with their output, each once). Stored once per turn, after its turn.started, just before its agent starts. Absent when nothing was added. Each text is as the agent got it: AGENTS.md is bounded at 32 KiB and instructions.md at 64 KiB; commands keeps the newest commands within 32 KiB, each stream cut at 8 KiB.", []string{"added"}},
	{NetworkRefused, "A turn's agent runs closed off from the network ([network] of permissions.toml), and asked its runner's proxy for a host the policy does not allow: host (host:port as asked) and error (why). The connection was refused. Each refusal is its own event. Show it as a warning.", []string{"host"}},
	{CommandQueued, "The person asked for a shell command in the session's copy (POST /api/v0/sessions/{id}/commands): command holds its id and line. It waits in the session's lane, behind what runs there. No turn.", []string{"session", "command"}},
	{CommandStarted, "The session's runner started the queued command: command holds its id and line. No turn.", []string{"session", "command"}},
	{CommandFailed, "The command did not run, or its runner stopped answering while it ran: command holds its id and line, error says why. No turn. Show it as an error.", []string{"session", "command", "error"}},
	{CommandRan, "The person ran a shell command in the session's copy, on the runner that holds it, with no model call (POST /api/v0/sessions/{id}/commands): command holds its id, the line, exit_code, timed_out, stdout and stderr (each at most 64 KiB, the start kept), output_bytes (all the command wrote) and duration_ms. No turn. The session's next turn gets it in its prompt once (context.added, kind commands). Show it as a shell entry in the conversation.", []string{"session", "command"}},
	{SubscriptionConnected, "Control kept a token from the person's own login for a subscription (POST /api/v0/subscriptions/{id}/connect): agent is the harness. No session. The token is never in the record.", []string{"agent"}},
	{SubscriptionFailed, "A subscription's connect ended without a token: agent is the harness, error says why, such as a wrong code or the login waiting too long. No session. Show it as an error.", []string{"agent", "error"}},
	{RunnerEnrolled, "A runner or a connector enrolled with control: runner is its id . No session.", []string{"runner"}},
	{RunnerGone, "A runner or a connector stopped asking control for work for a minute: runner is its id. It may come back; a turn it was running whose lease runs out ends as runner_lost. No session. Show it as a warning.", []string{"runner"}},
	{SettingsChanged, "The settings changed, through the API or by the one-time import of the settings files: setting is permissions, instructions, agents or defaults; keys names what changed (a policy key such as limits.max_depth, text for the instructions, an agent id or id.field such as a_1234.model, version and each key that moved for defaults); settings_version is the settings version the change made. On an import, file is the file the setting came from. Names only: never a value, so never a secret. The change and this event are stored together: the settings database keeps the event until the record has it. No session.", []string{"setting", "settings_version"}},
	{SessionRenamed, "The session was given a name with PATCH /api/v0/sessions/{id}: title is the new name, old_title the one it had before (absent when it had none). The latest names the session in GET /api/v0/sessions; earlier names stay in the record.", []string{"session", "title"}},
}

// Unexpected names one unknown shape: where on the wire, and the unknown value
// or field name. Only identifiers, never content.
type Unexpected struct {
	Where string `json:"where" doc:"Where on the wire, such as response.content[].type."`
	Value string `json:"value,omitempty" doc:"The unknown name, or (not a name)."`
	Field string `json:"field,omitempty" doc:"The unknown field name, or (not a name)."`
}

// Outcome is how a turn ended, in the framework's words. The vendor's own word
// is kept beside it in StopReason, as evidence.
type Outcome string

const (
	Finished Outcome = "finished"
	Partial  Outcome = "partial"
	Refused  Outcome = "refused"
	Unknown  Outcome = "unknown"
)

// Outcomes lists every Outcome. A test checks that every constant is listed.
var Outcomes = []Outcome{Finished, Partial, Refused, Unknown}

// Usage is the token count of one model call.
type Usage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
	// CacheWrite5mTokens and CacheWrite1hTokens split CacheWriteTokens by
	// cache lifetime, when the vendor said which. Each lifetime has its own
	// price. When they do not add up to CacheWriteTokens, the rest has an
	// unknown lifetime.
	CacheWrite5mTokens int `json:"cache_write_5m_tokens,omitempty" doc:"The part of cache_write_tokens with a 5 minute lifetime, when the vendor said."`
	CacheWrite1hTokens int `json:"cache_write_1h_tokens,omitempty" doc:"The part of cache_write_tokens with a 1 hour lifetime, when the vendor said."`
	// ServiceTier and Speed are the tier and speed the vendor reports it
	// served the call at, in its own words. Empty when it reported none.
	ServiceTier string `json:"service_tier,omitempty" doc:"The tier the vendor says served the call, in its own words."`
	Speed       string `json:"speed,omitempty" doc:"The speed the vendor says served the call, such as fast."`
	// InferenceGeo is where the vendor says the call ran: Anthropic's
	// usage.inference_geo, "global" or "us". Empty when it reported none.
	InferenceGeo string `json:"inference_geo,omitempty" doc:"Where the vendor says the call ran, such as global or us."`
	// ServerToolUse counts the tools the vendor ran itself, keyed by the
	// vendor's own name: Anthropic's usage.server_tool_use counts (such as
	// "web_search_requests"), and OpenAI's hosted tool call item types (such
	// as "web_search_call"). Some are charged per use.
	ServerToolUse map[string]int `json:"server_tool_use,omitempty" doc:"Counts of tools the vendor ran itself, by the vendor's own name."`
	// OutputIncomplete means the reply ended before the vendor sent its final
	// output count, so OutputTokens is a lower bound, not the real number.
	OutputIncomplete bool `json:"output_incomplete,omitempty" doc:"The reply ended before the final count: output_tokens is a lower bound."`
}

// Event is one step of an agent's work.
//
// The gateway fills ID, Time, Session, Exchange, Vendor and Client. An adapter
// fills the rest.
type Event struct {
	Seq      int64     `json:"seq,omitempty" doc:"The store's order: 1, 2, 3, without gaps, within one store. Absent until stored."`
	ID       string    `json:"id" doc:"Unique and opaque."`
	Kind     Kind      `json:"kind" doc:"What happened: a framework kind, or a program's own x.<program>.<name>."`
	Time     time.Time `json:"time"`
	Session  string    `json:"session,omitempty" doc:"The session; default for agent requests without /s/<session>. Absent on events.dropped."`
	Exchange string    `json:"exchange" doc:"One model request and its reply. Empty on events the API stores itself."`
	Vendor   string    `json:"vendor" doc:"anthropic, openai, or program for a program's own event. Empty on events the API stores itself."`
	Client   string    `json:"client,omitempty" doc:"The harness, from its User-Agent, such as claude-cli/2.1.318."`
	Turn     string    `json:"turn,omitempty" doc:"The turn this event belongs to. A turn started by POST /api/v0/turns: the id its reply named, on every event of it. Traffic no run started: a hash of the conversation up to the person's message."`

	Model        string   `json:"model,omitempty" doc:"On model.requested: the model asked for. On a run's turn.started and session.opened: the model the run was started with, absent for the harness's default; a session's next turn that names no agent and no model keeps it."`
	Text         string   `json:"text,omitempty" doc:"The person's words on turn.started, the reply on model.responded, a program's text."`
	ToolsOffered []string `json:"tools_offered,omitempty" doc:"On model.requested: the tools the harness offered."`
	StopReason   string   `json:"stop_reason,omitempty" doc:"The vendor's own word for how the reply ended."`
	Outcome      Outcome  `json:"outcome,omitempty" doc:"On turn.completed: how the turn ended, in the framework's words."`
	Usage        *Usage   `json:"usage,omitempty" doc:"On model.responded: absent when the vendor sent none, never zeros."`
	LatencyMS    *int64   `json:"latency_ms,omitempty"`
	Status       int      `json:"status,omitempty" doc:"The HTTP status of the vendor's reply. Absent on a call the agent closed before the vendor replied, as when its turn is stopped."`
	Error        string   `json:"error,omitempty"`
	// Probe marks every event of an exchange that is the harness's own check,
	// not a person's request, such as Claude Code's startup quota check. Such
	// an exchange starts no turn and has no turn id. A cockpit may hide it.
	Probe bool `json:"probe,omitempty" doc:"The harness's own check, not a person's request. No turn. A cockpit may hide it."`
	// CostUSD and CostComplete, only on model.responded. CostUSD is nil when
	// the call had no usage. CostComplete false means CostUSD is a lower
	// bound, or absent.
	CostUSD      *float64 `json:"cost_usd,omitempty" doc:"On model.responded: the call's cost from the built-in price table. Absent without usage."`
	CostComplete *bool    `json:"cost_complete,omitempty" doc:"On model.responded: false means cost_usd is a lower bound, or absent."`

	ToolID     string          `json:"tool_id,omitempty" doc:"Match a tool.completed to its tool.requested by this, never by turn."`
	ToolName   string          `json:"tool_name,omitempty"`
	ToolInput  json.RawMessage `json:"tool_input,omitempty" doc:"On tool.requested: the tool's input, as the model sent it."`
	ToolError  *bool           `json:"tool_error,omitempty"`
	ToolOutput string          `json:"tool_output,omitempty"`
	// Rule, only on tool.refused: the forbid entry that matched.
	Rule string `json:"rule,omitempty" doc:"On tool.refused: the forbid entry of permissions.toml that matched, such as git push."`
	// Server, Tools and ToolsHidden, only on tools.listed: the tool
	// server's name, the tools it served the turn, and the harness's own
	// tools hidden from the turn.
	Server      string       `json:"server,omitempty" doc:"On tools.listed: the tool server's name, as the harness puts it in tool names: agentbus."`
	Tools       []string     `json:"tools,omitempty" doc:"On tools.listed: the names of the tools the server served the turn, in its order. Absent when it served none."`
	ToolsHidden []HiddenTool `json:"tools_hidden,omitempty" doc:"On tools.listed: the harness's own tools the turn does not get, each with why. Absent when none are hidden."`
	// Added, only on context.added: each piece AgentBus gave the turn.
	Added []Added `json:"added,omitempty" doc:"On context.added: each piece AgentBus gave the turn besides the person's words, in the order the agent got them."`
	// Host is what a closed-off agent asked to reach.
	Host string `json:"host,omitempty" doc:"On network.refused: the host and port the agent asked for, such as example.com:443."`
	// Command, only on command.ran: the person's shell command and how it
	// ended.
	Command *Command `json:"command,omitempty" doc:"On command.ran: the person's shell command, how it ended and its output."`
	// Runner, only on runner.enrolled and runner.gone: the runner's id.
	Runner string `json:"runner,omitempty" doc:"On runner.enrolled and runner.gone: the runner's id."`
	// Setting, Keys and File, only on settings.changed: which setting
	// changed, the names of what changed, and on an import the file it came
	// from. Never a value.
	Setting string   `json:"setting,omitempty" doc:"On settings.changed: permissions, instructions, agents or defaults."`
	Keys    []string `json:"keys,omitempty" doc:"On settings.changed: the names of what changed, never a value: policy keys such as limits.max_depth, text, agent ids or id.field. Absent when the import found nothing changed from the defaults."`
	File    string   `json:"file,omitempty" doc:"On settings.changed of the one-time import: the file the setting came from, permissions.toml, instructions.md or agents.json. Before 0.5: the file written."`
	// SettingsVersion, on settings.changed and a run's turn.started: the
	// settings version.
	SettingsVersion int64 `json:"settings_version,omitempty" doc:"On settings.changed: the settings version the change made. On a run's turn.started: the settings version the turn started under. It moves up by one with every change; absent before 0.5."`
	// Title and OldTitle, only on session.renamed: the new name and the one
	// it replaced.
	Title    string `json:"title,omitempty" doc:"On session.renamed: the session's new name."`
	OldTitle string `json:"old_title,omitempty" doc:"On session.renamed: the name the session had before, absent when it had none."`

	Dropped  int      `json:"dropped,omitempty" doc:"On events.dropped: how many events were lost. Never 0."`
	Sessions []string `json:"sessions,omitempty" doc:"On events.dropped: every session that lost events."`
	// AllSessions, only on events.dropped: more sessions lost events than the
	// report can list, so every session counts as affected. Sessions is empty.
	AllSessions bool `json:"all_sessions,omitempty" doc:"On events.dropped: too many sessions to list, so every session counts as affected."`

	// Harness and Folder, on turn.started and session.opened: the harness a
	// run started, such as claude, and the folder it started in. On
	// turn.started they are empty for a session no run started, such as one
	// pointed at the gateway by hand. The JSON name stays agent, for the
	// records already kept.
	Harness string `json:"agent,omitempty" doc:"On turn.started and session.opened: the run's agent, such as claude. Empty on a turn of a session no run started."`
	Folder  string `json:"folder,omitempty" doc:"On turn.started and session.opened: the folder the run's agent started in. Empty on a turn of a session no run started."`
	// FolderRef: the reference of the person's folder the session was
	// started in, issued by its connector. The path stays there.
	FolderRef string `json:"folder_ref,omitempty" doc:"On turn.started and session.opened: the reference (fld_ and 16 hex digits) of the folder the session was started in, as its connector issued it. Absent for a session started before references, or in a folder no connector owns."`
	// AgentID, Access and KeyEnv, on a run's turn.started and session.opened:
	// the saved agent the run started, how it reaches its model, and for an
	// OpenRouter agent the name of the variable that holds its key. Never
	// the key itself. A session's next turn reads them back.
	AgentID string `json:"agent_id,omitempty" doc:"On a run's turn.started and session.opened: the saved agent the run started, as GET /api/v0/agents names it. Absent for a turn started by harness name alone."`
	Access  string `json:"access,omitempty" doc:"On a run's turn.started and session.opened: how the agent reaches its model: subscription or openrouter. Absent means subscription."`
	KeyEnv  string `json:"key_env,omitempty" doc:"On a run's turn.started and session.opened of an OpenRouter agent: the name of the environment variable that holds the key. Never the key."`
	Hold    string `json:"hold,omitempty" doc:"On session.opened and session.closed: names one run's child."`
	// ExitCode, only on session.closed from a run: the child's exit code, or
	// 128 plus the signal that ended it. Nil for a child closed by a service
	// restart.
	ExitCode *int `json:"exit_code,omitempty" doc:"On a run's session.closed: the child's exit code, or 128 plus its signal."`
	// ResumeID, only on session.closed from a run or a turn: the vendor's own
	// session id, read from the child's stdout (claude's session_id, codex's
	// thread_id). It is what the session's next turn continues.
	ResumeID string `json:"resume_id,omitempty" doc:"On a run's session.closed: the vendor's own session id, which the next turn continues."`

	// ReplayFrom, only on turn.started: the earlier turn of the session whose
	// history the gateway puts in front of every model request of this turn,
	// in place of the vendor's own resume. Empty when the turn continues by
	// the vendor's resume, or is a session's first.
	ReplayFrom string `json:"replay_from,omitempty" doc:"On turn.started: the session's earlier turn whose history the gateway replays in front of every model request of this turn, in place of the vendor's resume. Absent for a first turn and for a turn continued by the vendor's resume."`
	// Unsettled, only on a run's session.closed: the gateway could not
	// confirm that all of the turn's model calls were stored (it gave up
	// waiting, or a queue was full), so the turn may be missing calls.
	Unsettled bool `json:"unsettled,omitempty" doc:"On a run's session.closed: the gateway could not confirm that every model call of the turn was stored, so the turn may be missing calls. Own replay does not continue from such a turn."`
	// Stopped, only on a run's session.closed: the person stopped the turn
	// (POST /api/v0/turns/{id}/stop) while its child ran.
	Stopped bool `json:"stopped,omitempty" doc:"On a run's session.closed: the person stopped the turn with POST /api/v0/turns/{id}/stop while its child ran. The session's next turn continues after it, told that the person stopped it."`
	// Continuity, only on a next turn's turn.started: how it reaches the
	// session's earlier turns, "replay", "resume" or "transcript".
	Continuity string `json:"continuity,omitempty" doc:"On the turn.started of a session's next turn started by POST /api/v0/turns: replay (own replay, see replay_from), resume (the vendor's resume_id) or transcript (the turn's harness differs from the previous turn's, so it got the earlier turns as plain text, see context). Absent on a first turn."`
	// Context, only on the turn.started of a next turn: the earlier turns as
	// plain text when its continuity is transcript, then the person's shell
	// commands since the previous turn. The agent's prompt was Context
	// followed by Text; Text stays the person's own words.
	Context string `json:"context,omitempty" doc:"On a turn.started whose continuity is transcript: the session's earlier turns as plain text, each person's message and the agent's final reply, oldest first. On a next turn after the person ran shell commands (command.ran): those commands and their output, after any transcript, the same text as its context.added of kind commands. The agent got context followed by text as its prompt; text is the person's own words."`
	// RecoveredFrom and RecoveredReason, only on a next turn's turn.started:
	// the turn before it did not finish (it was interrupted, its last model
	// call failed, or it never reached the model), and this turn continues
	// after it anyway. The replayed history ends with a note saying so.
	RecoveredFrom   string `json:"recovered_from,omitempty" doc:"On a replayed turn's turn.started: the session's previous turn, which did not finish (interrupted, its last model call failed, or it never reached the model). This turn continues after it, and the model is told what happened. Absent when the previous turn finished."`
	RecoveredReason string `json:"recovered_reason,omitempty" doc:"With recovered_from: one sentence saying how that turn ended, such as its last model request getting status 529. The same words the model is told."`
	// IdempotencyKey and RequestHash, only on the turn.started of a turn
	// started by POST /api/v0/turns with idempotency_key: the client's key,
	// and the SHA-256 of the request's fields, so a retry with the same key
	// finds this turn and a different request under it is refused.
	IdempotencyKey string `json:"idempotency_key,omitempty" doc:"On turn.started: the idempotency_key POST /api/v0/turns was sent with. A retry with the same key returns this turn and starts nothing."`
	RequestHash    string `json:"request_hash,omitempty" doc:"With idempotency_key: the SHA-256, in hex, of the request's fields. A request with the same key and other fields is refused."`
	// Origin, only on a run's turn.started: the turn was not typed by the
	// person. Another agent's turn asked for it through the API, or AgentBus
	// started it to deliver the result of work this session asked for.
	Origin *Origin `json:"origin,omitempty" doc:"On a run's turn.started: who started this turn when the person did not. Absent for a turn the person started."`
	// Replayed, only on model.requested: how many messages of the earlier
	// turn's history the gateway put in front of this request.
	Replayed int `json:"replayed,omitempty" doc:"On model.requested: how many messages of the earlier turn's history the gateway put in front of this request. Absent when it put none."`

	// Data, only on a program's own event (kind x.<program>.<name>): the JSON
	// object it sent.
	Data json.RawMessage `json:"data,omitempty" doc:"On a program's own event: the JSON object it sent."`

	Unexpected *Unexpected `json:"unexpected,omitempty" doc:"On schema.unexpected: the unknown shape."`
}

// Command is one shell command the person ran in a session's copy
// (command.ran).
type Command struct {
	ID   string `json:"id" doc:"The command's id, as POST /api/v0/sessions/{id}/commands answered it: cmd_ and 16 hex digits."`
	Line string `json:"line" doc:"The command line as sent, run by /bin/sh -c."`
	// The rest only on command.ran.
	ExitCode    int    `json:"exit_code,omitempty" doc:"On command.ran: its exit code, or 128 plus the signal that ended it. Absent means 0."`
	TimedOut    bool   `json:"timed_out,omitempty" doc:"On command.ran: it ran past its time limit and was ended."`
	Stdout      string `json:"stdout,omitempty" doc:"On command.ran: what it wrote to stdout, at most 64 KiB, the start kept."`
	Stderr      string `json:"stderr,omitempty" doc:"On command.ran: what it wrote to stderr, at most 64 KiB, the start kept."`
	OutputBytes int64  `json:"output_bytes,omitempty" doc:"On command.ran: all the bytes it wrote to stdout and stderr, kept or not."`
	Truncated   bool   `json:"truncated,omitempty" doc:"On command.ran: some output was cut, as stdout or stderr went past 64 KiB."`
	DurationMS  int64  `json:"duration_ms,omitempty" doc:"On command.ran: how long it ran."`
}

// HiddenTool is one of a harness's own tools a turn does not get.
type HiddenTool struct {
	Name string `json:"name" doc:"The tool's name, as the harness names it, such as Task."`
	Why  string `json:"why" doc:"Why the turn does not get it, such as replaced by AgentBus tools."`
}

// Added is one piece of what AgentBus gave a turn besides the person's
// words (context.added).
type Added struct {
	Kind  string `json:"kind" doc:"agents_md, instructions, asked_by, mcp_instructions or commands."`
	Bytes int    `json:"bytes" doc:"The size of text, in bytes."`
	Text  string `json:"text" doc:"The text as the agent got it."`
}

// The kinds of an Added.
const (
	AddedAgentsMD        = "agents_md"
	AddedInstructions    = "instructions"
	AddedAskedBy         = "asked_by"
	AddedMCPInstructions = "mcp_instructions"
	// AddedCommands: the shell commands the person ran in the session since
	// its previous turn (command.ran), with their output.
	AddedCommands = "commands"
)

// Origin says who started a turn when the person did not.
type Origin struct {
	Kind string `json:"kind" doc:"asked: another agent's turn started this turn through the API with its turn token. result: AgentBus started this turn to deliver the result of work this session asked for; text is that result."`
	// For asked: the session and turn that asked. For result: the session
	// and turn of the work whose result this is.
	Session string `json:"session,omitempty" doc:"asked: the session of the turn that asked. result: the session the work ran in."`
	Turn    string `json:"turn,omitempty" doc:"asked: the turn that asked. result: the work's turn."`
	// TurnKind, for result: the kind of the work's turn. A turn.started
	// stored before 0.4 holds job and job_kind instead.
	TurnKind string `json:"turn_kind,omitempty" doc:"result: turn or review: the kind of the work's turn."`
	Agent    string `json:"agent,omitempty" doc:"result: the saved agent, or harness, that did the work."`
}

// Origin kinds.
const (
	OriginAsked  = "asked"
	OriginResult = "result"
)

// Sink receives events. Implementations must be safe for concurrent use.
type Sink interface {
	Emit(Event)
}

// JSONLines writes one JSON object per line.
type JSONLines struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func NewJSONLines(w io.Writer) *JSONLines {
	return &JSONLines{enc: json.NewEncoder(w)}
}

// Emit writes the event. An event that cannot be encoded still leaves a line,
// so a reader sees that something happened.
func (j *JSONLines) Emit(e Event) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.enc.Encode(e); err != nil {
		_ = j.enc.Encode(Event{ID: e.ID, Kind: e.Kind, Time: e.Time, Session: e.Session,
			Exchange: e.Exchange, Vendor: e.Vendor, Error: "this event could not be encoded"})
	}
}

// Memory keeps events in order. Useful in tests and small programs.
type Memory struct {
	mu     sync.Mutex
	events []Event
}

func (m *Memory) Emit(e Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, e)
}

// Events returns a copy of what was emitted so far.
func (m *Memory) Events() []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Event(nil), m.events...)
}
