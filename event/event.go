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

	"github.com/staysup/agentbus/applied"
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
	// RunStarted and RunEnded: a run's child started or exited. Hold
	// names that child, so each run.started has its own run.ended.
	RunStarted Kind = "run.started"
	RunEnded   Kind = "run.ended"

	// SchemaUnexpected: the vendor sent a shape the adapter does not know, such
	// as a new block type or usage field. Unexpected says what, as names only.
	SchemaUnexpected Kind = "schema.unexpected"

	// ReplayFailed: a model request of a replayed turn could not be given the
	// earlier turn's history, so the gateway refused it rather than send it
	// without that history. Error says why.
	ReplayFailed Kind = "replay.failed"

	// HistoryShortened: own replay shortened the history it puts in front of
	// a turn, to keep it within the history budget. History says the step.
	// HistoryCompacted: the gateway's verdict on one main-thread call, that
	// its harness had compacted it; it went on as sent. Own replay reads it
	// in a turn that carries a history; in a turn with none it applies the
	// same test to the stored request (replay's compacted).
	HistoryShortened Kind = "history.shortened"
	HistoryCompacted Kind = "history.compacted"

	// ToolRefused: the gateway refused a tool call of a run's turn, because
	// its command matched a forbid entry of permissions.toml. The agent did
	// not run it. Rule names the entry.
	ToolRefused Kind = "tool.refused"

	// ApprovalRequested: the gateway refused a tool call of a run's turn because
	// its command matched an ask entry of permissions.toml, and recorded it for
	// a person to allow or refuse. The turn ends; nothing is held. Approval is
	// its id, Commands the ask commands, Rules the entries they matched; Rule is
	// the first entry and Text the commands one per line. Control lists the
	// pending ones from the record (a requested with no approval.decided), so a
	// refusal on a separate runner is still answered on control.
	ApprovalRequested Kind = "approval.requested"
	// ApprovalDecided: a recorded ask command was allowed or refused, written by
	// control when a person answers. Approval ties it to its approval.requested.
	// Text says how it ended and who answered.
	ApprovalDecided Kind = "approval.decided"

	// SessionCreated: a session was created in a workspace (POST
	// /api/v0/sessions), before any turn of it. Session is its id, Workspace
	// the workspace its turns run in, and Title its name when one was given. A
	// session groups turns; it owns only a title and its archived view, never a
	// folder or a mode -- those are the workspace's. A session may have no turn
	// yet: created and then left, which is harmless.
	SessionCreated Kind = "session.created"
	// SessionRenamed: the person, or a turn of the session's request tree,
	// gave the session a name (PATCH /api/v0/sessions/{id}). Title is the new
	// name and OldTitle the one it replaced. The latest names the session.
	SessionRenamed Kind = "session.renamed"

	// SessionArchived: the session was archived or un-archived (PATCH
	// /api/v0/sessions/{id}). Archived is true to hide it from the default
	// list, false to un-hide it. The latest decides. Only a view: nothing is
	// removed and the session still takes turns. Stored like a rename, so it
	// works on a session that has no workspace row yet (its first turn still
	// queued).
	SessionArchived Kind = "session.archived"

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

	// CallRefused: a gateway with a credential of control's refused a
	// turn's call to its vendor: not a call the vendor's adapter records
	// (method, path or query), or a reply in a format it does not read. Call
	// says which; Error says why. VendorCalled: a call of the harness's own
	// besides its model calls, which the adapter names (adapter.Call
	// Service), recorded by Call and Status, its body unread.
	CallRefused  Kind = "call.refused"
	VendorCalled Kind = "vendor.called"

	// The person's shell command in a session's copy, with no model call:
	// the "!" of 0.4 (POST /api/v0/sessions/{id}/commands). Retired in 0.5,
	// where a "!" is a message to the session's agent, which runs it with its
	// own shell tool. Nothing writes these any more; they stay known so a
	// record from before reads as it is. Command.ID ties them.
	CommandQueued  Kind = "command.queued"
	CommandStarted Kind = "command.started"
	CommandRan     Kind = "command.ran"
	CommandFailed  Kind = "command.failed"

	// Control's own operations, in no session: a subscription connected or
	// failed to (Harness, and Error says why), a runner enrolled, stopped
	// answering or was removed (Runner), and a settings file changed (File). Never a value.
	SubscriptionConnected Kind = "subscription.connected"
	SubscriptionFailed    Kind = "subscription.failed"
	RunnerEnrolled        Kind = "runner.enrolled"
	RunnerGone            Kind = "runner.gone"
	RunnerRemoved         Kind = "runner.removed"
	SettingsChanged       Kind = "settings.changed"

	// TurnChanged: control's record of what a turn changed in its copy,
	// stored when control accepts the turn job's finish. A Direct turn (in
	// the person's folder as it is) has none. Changes holds the changed files and the diffs. The list can be
	// cut: truncated says it is not complete.
	// TurnApplied: an apply of a session's turns to the person's folder
	// ended. Applied says which turns, who asked, how it ended and the files.
	// Pre-workspace records keep theirs; the workspace reader reads them as the
	// apply history of the turn's workspace. Nothing writes it now.
	TurnChanged Kind = "turn.changed"
	TurnApplied Kind = "turn.applied"

	// WorkspaceCreated, WorkspaceApplied and WorkspaceDiscarded are a
	// workspace's own events. A workspace is where turns run: it owns the
	// folder, the machine, the sandbox mode (copy + wall, or the real folder),
	// its checkpoints, Apply and Discard. One workspace backs one session.
	//
	// WorkspaceCreated is written by control when a workspace is created,
	// before any turn job of it is queued. Workspace is its id, FolderRef (and
	// Folder on a local agentbus up) the folder, Machine the connector, and
	// Sandbox the mode (always present: true for a copy behind a wall, false
	// for the person's real folder). It is the single source of the
	// workspace's folder, machine and mode. Stored once per workspace
	// (AppendOnce with event id wc_<workspace>).
	//
	// WorkspaceApplied is written when an apply job of the workspace ends on
	// the folder's machine. Workspace, Session and Turn name it; Applied holds
	// the turns carried, the from/to changesets, the result, the files and who
	// asked. Old turn.applied events are read by the same reader for
	// pre-workspace turns.
	//
	// WorkspaceDiscarded is written when the unapplied changes of a sandbox
	// workspace are dropped. Workspace and Session name it; Discarded holds the
	// changeset it was reset to (or "base" when never applied), the turns whose
	// file changes were dropped, and who asked. The conversation stays.
	//
	// WorkspaceUpdated is written when an update job of the workspace ends: the
	// person's folder as it is now is merged into the copy, so the session's
	// work sits on top of it and the next Apply writes only that work. Workspace
	// and Session name it; Updated holds the result (updated or conflict), the
	// new baseline (from, the folder's current state) and target (to, the merge)
	// changesets on a clean update, the folded turns, the conflicting files on a
	// conflict, and who asked.
	WorkspaceCreated   Kind = "workspace.created"
	WorkspaceApplied   Kind = "workspace.applied"
	WorkspaceDiscarded Kind = "workspace.discarded"
	WorkspaceUpdated   Kind = "workspace.updated"
	// WorkspaceClosed records removal of a sandbox copy. The next turn rebuilds
	// from the folder; the record and conversation stay.
	WorkspaceClosed Kind = "workspace.closed"
	// WorkspaceKept records a put-away that kept the copy: when it came due,
	// the workspace was not quiet or not settled. put_away says why.
	WorkspaceKept Kind = "workspace.kept"
	// WorkspaceSnapshot is the applied state of a workspace whose record was
	// written before 0.6.6, when its outcome events did not yet carry it all
	// (a turn's changeset, an apply's to, an update's head and checkpoints):
	// written once, at control's start, from the job rows of that time
	// (jobs.MigrateApplied). A reader folds the workspace's applied state from
	// it on.
	WorkspaceSnapshot Kind = "workspace.snapshot"

	// FolderRegistered is control's record that a folder a person typed was
	// checked and registered on its machine's runner (queue.KindFolder), so a
	// folder becoming known to control is in the record and not only in the
	// folder registry. FolderRef is its reference and Machine the connector
	// that holds it. The path is not carried: it is kept off control's disk
	// (queue/folder.go). No session. Stored once per folder reference and
	// machine, as the folder is first registered.
	FolderRegistered Kind = "folder.registered"

	// TurnEnded: control's record that a turn job ended, written for every
	// ended turn job whatever its outcome (finished, failed, stopped,
	// cancelled or its runner lost), once per turn job. It is the signal a
	// reactor reads to learn a turn ended -- from the record, not from the
	// scheduler's job table (C0). state is how it ended; error says why.
	// It is keyed on the turn and carries NO session: a reactor trigger, not
	// part of a session's event stream, so a turn refused before it ran cannot
	// conjure a phantom session or shift a real one's snapshot.
	// The changes and the reply are elsewhere (turn.changed, the job result).
	TurnEnded Kind = "turn.ended"

	// ChangesPreviewed: what applying a workspace's changes to the person's
	// folder would do now, the dry run's result, stored so GET
	// /workspaces/{id}/changes serves it from the record (one source) and pages
	// it. Previewed holds it. It is keyed by its workspace (its session field is
	// empty). Stored when a preview runs; reused until the workspace changes.
	ChangesPreviewed Kind = "changes.previewed"

	// PlanUsage: how much of a subscription's plan is used in one of its
	// windows, as the vendor said in a model reply's headers (gateway/plan.go).
	// Plan holds the reading. Stored when the reading changes, and again
	// at most every few minutes while it holds. No session.
	PlanUsage Kind = "plan.usage"
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
	{WorkspaceClosed, "A sandbox workspace's copy was removed, by POST /api/v0/workspaces/{id}/close or by AgentBus putting it away. The record and turns stay. The next turn rebuilds from the folder. Resets all apply and update baselines of the removed copy. Stored once per close. put_away is present when AgentBus put the copy away by itself: the on and the event that queued it, when it was queued and when it came due.", []string{"workspace"}},
	{WorkspaceKept, "AgentBus came to put a sandbox workspace's copy away and kept it: workspace names it, and put_away holds the on and the event that queued the put-away, when it was queued, when it came due, and why it kept the copy (putting copies away was turned off, a put-away for a later event decides instead, other work waiting or running, the workspace used since, work not applied or discarded, no copy, or the record could not be read). Nothing was removed. Stored once per put-away.", []string{"workspace", "put_away"}},
	{TurnStarted, "A person's message started a turn. Stored once per turn id; text holds their words; agent and folder name the run's agent and folder, empty when no run started the session. A turn started by POST /api/v0/turns has its turn.started stored by the API, with the prompt as sent, before its agent starts.", nil},
	{ModelRequested, "The harness sent a request to the model: model and tools_offered.", nil},
	{ModelResponded, "The model replied: status, text, stop_reason, usage, latency_ms, cost_usd, or error.", nil},
	{ToolRequested, "The model asked for a tool: tool_id, tool_name, tool_input.", []string{"tool_id"}},
	{ToolCompleted, "A tool's result reached the model: tool_id, tool_output, tool_error. Match it to its call by tool_id, never by turn.", []string{"tool_id"}},
	{TurnCompleted, "The turn ended, from the model's stop reason: outcome and stop_reason. " +
		"For traffic pointed at the gateway by hand, the turn id is a hash of the conversation, and a turn with no turn.completed is running, " +
		"waiting on tools (stop reason tool_use), stopped (an error or a non-200 reply), or left behind (a later turn started).", []string{"outcome"}},
	{EventsDropped, "Events were lost because the output did not keep up: dropped, and sessions or all_sessions.", []string{"dropped"}},
	{SessionRegistered, "Older stores only: a launcher or a run registered the session, with agent and folder. Nothing writes it now.", []string{"session"}},
	{RunStarted, "A run's child is starting: hold names it; agent and folder say what and where; turn is the turn it runs. Stored just before the child starts.", []string{"session", "hold"}},
	{RunEnded, "A run's turn ended: its child exited, or could not start. hold, exit_code, resume_id, turn, state (finished, failed or stopped), error when it failed (the agent's own error line from its stderr, or plain words in its place for a failure its harness knows, else its exit code, or why it could not start), stopped when the person stopped the turn, error_tail (the last lines the agent wrote to stderr, when it exited with an error), and unsettled when the gateway could not confirm all of the turn's calls were stored. Also stored at start, with unsettled and state failed, for a child the previous service left running, and by control for a turn whose runner stopped answering.", []string{"session"}},
	{SchemaUnexpected, "The vendor sent a shape the adapter does not know: unexpected, as names only. Show it as a quiet warning.", []string{"unexpected"}},
	{ReplayFailed, "A model request of a replayed turn (a turn.started with replay_from) could not be given the earlier turn's history: error says why, such as the harness's request layout not being one the gateway knows. The gateway refused that request with 400 and did not send it to the vendor, so the turn fails rather than continue without its history. Show it as an error.", []string{"error"}},
	{HistoryShortened, "Own replay shortened the history it put in front of the turn, to keep it within the history budget (budget of GET /api/v0/settings/compaction, a share of the model's window, 0.4 by default). history holds the step: from (the earlier turn whose history it is), model, window_tokens, budget_tokens, before_tokens and after_tokens (rough counts, AgentBus's own, not the vendor's), results_shortened and inputs_shortened (long tool results and tool-call inputs made a stub that says where the full text is), turns_summarised (the oldest turns folded into AgentBus's summary), turns_left_out (the oldest logs left out of the summary for one line, so it stays bounded), newest_shortened (the newest earlier turn was shortened too: the budget wins over keeping it whole) and turns_whole (the newer turns left whole). Stored on the turn's first main request, once per step (again if the service restarted inside the turn). Each step is frozen: later turns carry the shortened history byte for byte, so the prompt cache keeps working. The record keeps every byte of every call; the turn's stored requests are what the model saw. Show it as a quiet line.", []string{"history"}},
	{HistoryCompacted, "The gateway's verdict on one model call of a turn's main thread: its harness had compacted its context, so the request was not in the layout a fresh process sends (the turn's prompt where a fresh process puts it). It went on as the harness sent it (codex's under the session's thread id). Stored on each such call, with its exchange, in any turn, replayed or not. Own replay builds the next turn from the record, from the turn's last call before it, never from the harness's summary. In a replayed turn this event is what says a call was compacted; in a turn with no earlier history, whose stored requests are the agent's own, own replay applies the same layout test to the stored request, so a record from before 0.6.7 reads the same.", []string{"exchange"}},
	{ToolRefused, "The gateway refused a tool call before the agent could run it, because its command matched a forbid entry of permissions.toml: tool_id, tool_name, tool_input, rule (the entry), and text (what the model is told in place of the output). The call's tool.requested is still stored, and its tool.completed carries that text. Show it as a warning.", []string{"tool_id", "rule"}},
	{ApprovalRequested, "The gateway refused a tool call because its command matched an ask entry of permissions.toml, and recorded it for a person: approval (its id), tool_id, tool_name, rule (the first ask entry), commands (every ask command in the reply) and rules (the ask entries they matched); text lists the commands one per line. The turn ends with the call refused; nothing is held. Control derives the pending list from the record, a requested with no approval.decided for the same approval id, so it works when the gateway that refused runs on a separate runner. GET /api/v0/approvals lists it and POST /api/v0/approvals/{id} answers it: allow moves the entries from ask to allow and continues the session so the agent can run them; refuse records the decision. The call's tool.requested is still stored, and its tool.completed carries the refusal. Show it as a prompt to allow or refuse.", []string{"approval", "tool_id", "commands", "rules"}},
	{ApprovalDecided, "A recorded ask command was answered, written by control when a person answers: approval ties it to its approval.requested, rule is the first ask entry, and text says how it ended (allowed or refused) and who answered. Show it beside the request.", []string{"approval"}},
	{ToolsListed, "Control served the turn's tool list: its agent's tool server (POST /api/v0/mcp, [agent_api] tools in permissions.toml) answered tools/list for the turn's token. Stored once per turn, at the first tools/list, however often the agent asks. server is the tool server's name, as tool names carry it (claude calls a tool mcp__agentbus__<name>); tools are the names it served, which the turn's token may call; tools_hidden are the harness's own tools the turn does not get, each with why. A turn with no tools.listed got no tool server, or its agent never asked for the list. Names only: never an input, a value or a token.", []string{"server"}},
	{ContextAdded, "What AgentBus gave the turn besides the person's words, the same for every harness: added, each piece with its kind, bytes and text. Kinds: agents_md (the folder's AGENTS.md, when the harness does not read it by itself), instructions (instructions.md, filled in), asked_by (who asked an asked agent for the work), mcp_instructions (the tool server's own instructions, when the turn has it) and, in records from before 0.5, commands (the shell commands the person ran in the session since its previous turn, with their output). Stored once per turn, after its turn.started, just before its agent starts. Absent when nothing was added. Each text is as the agent got it: AGENTS.md is bounded at 32 KiB and instructions.md at 64 KiB.", []string{"added"}},
	{NetworkRefused, "A turn's agent runs closed off from the network ([network] of permissions.toml), and asked its runner's proxy for a host the policy does not allow: host (host:port as asked) and error (why). The connection was refused. Each refusal is its own event. Show it as a warning.", []string{"host"}},
	{CallRefused, "A turn's agent sent its vendor a call that control adds no credential to, because it is not a call the vendor's adapter records: call (method and vendor path), status and error (why). " +
		"Refused before sending (a path, method or query the adapter does not read): status 403, the vendor got nothing and the credential went nowhere. " +
		"Refused after (a reply in a format the adapter does not read): the call's model.responded says so too, and the reply was not passed on. Show it as a warning.", []string{"call"}},
	{VendorCalled, "A call of the harness's own to its vendor besides its model calls, such as agy's loadCodeAssist, which the vendor's adapter names as one the harness needs, and which got the turn's credential: call (method and vendor path), status, latency_ms, and error when it failed. Its body is not read or kept.", []string{"call"}},
	{CommandQueued, "Retired in 0.5; only in older records. The person asked for a shell command in the session's copy (the ! of 0.4): command holds its id and line. No turn.", []string{"session", "command"}},
	{CommandStarted, "Retired in 0.5; only in older records. The session's runner started the queued command: command holds its id and line. No turn.", []string{"session", "command"}},
	{CommandFailed, "Retired in 0.5; only in older records. The command did not run, or its runner stopped answering while it ran: command holds its id and line, error says why. No turn. Show it as an error.", []string{"session", "command", "error"}},
	{CommandRan, "Retired in 0.5; only in older records. The person ran a shell command in the session's copy, with no model call (the ! of 0.4): command holds its id, the line, exit_code, timed_out, stdout and stderr (each at most 64 KiB, the start kept), output_bytes (all the command wrote) and duration_ms. No turn. Show it as a shell entry in the conversation.", []string{"session", "command"}},
	{SubscriptionConnected, "Control kept a token from the person's own login for a subscription (POST /api/v0/subscriptions/{id}/connect): agent is the harness. No session. The token is never in the record.", []string{"agent"}},
	{SubscriptionFailed, "A subscription's connect ended without a token: agent is the harness, error says why, such as a wrong code or the login waiting too long. No session. Show it as an error.", []string{"agent", "error"}},
	{RunnerEnrolled, "A runner or a connector enrolled with control, by a sign-in (agentbus login) the person approved, or as the built-in runner of agentbus up: runner is its id. No session.", []string{"runner"}},
	{RunnerGone, "A runner or a connector stopped asking control for work for a minute: runner is its id. It may come back; a turn it was running whose lease runs out ends as runner_lost. No session. Show it as a warning.", []string{"runner"}},
	{RunnerRemoved, "The person removed a runner that was offline (POST /api/v0/runners/{id}/remove): runner is its id. Its token works no more and it is no longer listed; its sessions, jobs and their events stay, but its sessions take no more turns or reviews. No session.", []string{"runner"}},
	{SettingsChanged, "The settings changed, through the API or by the one-time import of the settings files: setting is permissions, instructions or agents; keys names what changed (a policy key such as limits.max_depth, text for the instructions, an agent id or id.field such as a_1234.model); settings_version is the settings version the change made. On an import, file is the file the setting came from. A build that changed a default writes the old value into the person's policy on its first start: a change of permissions with no file, whose keys are those defaults. Names only: never a value, so never a secret. The change and this event are stored together: the settings database keeps the event until the record has it. No session.", []string{"setting", "settings_version"}},
	{TurnChanged, "What the turn changed in its session's copy, from its start to its end. Stored by control once it accepts the turn's end, at most once per turn: changes holds the changed files (path, change, size, sha256 at the end, sha256_before at the start) and, for small text files, each one's unified diff (at most 1 MiB for the whole turn; diffs_cut says some were left out) and content. A binary file has its path, size and hashes only. The file list can be cut: truncated says it is not complete, because files were left out (past 1,000, or when the whole was too large to send; count still counts them) or because the copy held more files than a checkpoint keeps (then count misses some too). GET /api/v0/turns/{id}/changes lists it, and /diff summarises it. Absent for a turn that ran in no folder; for a turn in a direct workspace (sandbox false), whose changes went straight into the person's folder and AgentBus keeps no record of them; and for turns from before 0.5, whose changes are in their job. A direct turn from before 0.6.5 may still have one. From 0.6.6 changeset and checkpoint name what an apply up to the turn writes; changes is absent when the turn's end sent no file list.", []string{"session", "turn"}},
	{TurnApplied, "An apply of the session's turns to the person's folder ended (POST /api/v0/turns/{id}/apply): turn is the turn asked for, and applied holds every turn whose changes it carried, who asked (by: the person or an agent, never a token), the result (applied, refused or failed), the files written or removed, and on a refusal the files that refused it, each with why. Stored once per apply. Pre-workspace records only; the workspace reader reads it as an apply of the turn's workspace. Show a refusal as a warning.", []string{"session", "turn", "applied"}},
	{WorkspaceCreated, "A workspace was created, before any turn of it ran: workspace is its id, folder_ref (and folder on a local agentbus up) the folder it runs in, machine the connector that owns the folder, and sandbox the mode (true for a copy behind a wall, false for the person's real folder; always present). It is the single source of the workspace's folder, machine and mode. Stored once per workspace, keyed on the event id wc_<workspace>.", []string{"workspace", "sandbox"}},
	{WorkspaceApplied, "An apply of a workspace's changes to the person's folder ended (POST /api/v0/workspaces/{id}/apply): workspace and session name it, turn is the turn applied up to, and applied holds every turn whose changes it carried, the from and to changesets, who asked (by: the person or an agent, never a token), the result (applied, refused or failed), the files written or removed, and on a refusal the files that refused it, each with why. Stored once per apply. Show a refusal as a warning.", []string{"workspace", "applied"}},
	{WorkspaceDiscarded, "The unapplied changes of a sandbox workspace were dropped (POST /api/v0/workspaces/{id}/discard): workspace and session name it, and discarded holds the changeset it was reset to (the last applied one, or base when never applied), the turns whose file changes were dropped, and who asked. The conversation stays in the record. Stored once per discard.", []string{"workspace", "discarded"}},
	{FolderRegistered, "A folder a person typed was checked and registered on its machine's runner (POST /api/v0/workspaces with folder): folder_ref is the reference its machine issued, and machine the connector that holds it. The path is not carried; it is kept off control's disk. No session. Stored once per folder reference and machine, as the folder is first registered, so a folder becoming known is in the record and not only in the folder listing (GET /api/v0/folders).", []string{"folder_ref", "machine"}},
	{WorkspaceUpdated, "The person's folder as it is now was merged into a sandbox workspace's copy (POST /api/v0/workspaces/{id}/update), so the session's work sits on top of it and the next Apply writes only that work: workspace and session name it, and updated holds the result (updated, merged or conflict), on a clean update or a merge the new baseline changeset (from, the folder now) and the merged changeset (to) with the folded turns, on a merge the files left with conflict markers (unresolved), on a conflict the files that conflicted, and who asked. Nothing is written to the folder; the copy's private git does the three-way merge. Stored once per update.", []string{"workspace", "updated"}},
	{WorkspaceSnapshot, "The applied state of a workspace whose record was written before 0.6.6, when its outcome events did not yet carry all of it: workspace names it, and snapshot holds its appliable turns with their changesets and checkpoints, how far a successful apply went and what it wrote, the update in force and whether a merge left conflict markers. Written once, when control starts, from what control held then. It stands for every turn.changed, apply, discard and update of the workspace before it; later ones add to it.", []string{"workspace", "snapshot"}},
	{TurnEnded, "A turn job ended, whatever its outcome: control writes it once per turn job for every ended turn (finished, failed, stopped, cancelled or its runner lost), so a reactor (a delivery) learns a turn ended from the record, not from the scheduler's job table. It is keyed on the turn and carries no session -- a reactor trigger, not part of a session's event stream. state is how it ended and error says why. The changes and the reply are not here: those are turn.changed and the job's result.", []string{"turn", "state"}},
	{ChangesPreviewed, "What applying a workspace's changes to the person's folder would do now, the dry run of POST /api/v0/workspaces/{id}/apply computed on the folder's machine, nothing written: previewed holds the files it would write or remove (with diffs), the conflicts it would refuse, the turn it would apply up to and the turns it would carry. workspace names the workspace it is of (its session field is empty, so it never enters the session namespace). Stored so GET /api/v0/workspaces/{id}/changes serves and pages it from the record, as of this event's time; a new one is stored when a preview runs again (the workspace changed, or a fresh one was asked for). The file list can be cut (truncated) when too many to send; count still counts them all.", []string{"workspace", "previewed"}},
	{PlanUsage, "How much of a subscription's plan is used in one of its windows, as the vendor said in the headers of a model reply that carried control's credential for that subscription: plan holds the subscription (the harness, such as claude), the connect its credential came from, the window, its length in minutes, the percent used, when it resets and the vendor's status word. " +
		"exchange names the call it was read from. Stored when the reading changes, and again every 10 minutes at most while calls keep it the same. " +
		"The latest per subscription, connect and window is kept; GET /api/v0/subscriptions shows those of the subscription's current connect. Only control's own gateway stores it: a runner's record is refused it. Read from a fixed list of the vendor's usage headers only; no other header. No session.", []string{"plan"}},
	{SessionCreated, "A session was created in a workspace with POST /api/v0/sessions, before any turn of it: session is its id, workspace the workspace its turns run in, and title its name when one was given. A session groups turns and owns only its title and archived view; its folder and mode are its workspace's. Stored once per session, keyed on the event id sc_<session>. A session may be created and left with no turn, which is harmless.", []string{"session", "workspace"}},
	{SessionRenamed, "The session was given a name with PATCH /api/v0/sessions/{id}: title is the new name, old_title the one it had before (absent when it had none). The latest names the session in GET /api/v0/sessions; earlier names stay in the record.", []string{"session", "title"}},
	{SessionArchived, "The session was archived or un-archived with PATCH /api/v0/sessions/{id}: archived true hid it from the default list, false (absent) un-hid it. The latest decides. Only a view: nothing is removed and the session still takes turns.", []string{"session", "archived"}},
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
	// CacheWrite30mTokens and CacheWrite12hTokens are OpenAI's two write
	// lifetimes, from input_tokens_details.cache_write_tokens_details. Each has
	// its own price.
	CacheWrite30mTokens int `json:"cache_write_30m_tokens,omitempty" doc:"The part of cache_write_tokens with a 30 minute lifetime, when the vendor said."`
	CacheWrite12hTokens int `json:"cache_write_12h_tokens,omitempty" doc:"The part of cache_write_tokens with a 12 hour lifetime, when the vendor said."`
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
	// CostUSD is what the vendor says it charged for the call, in US
	// dollars: OpenRouter's usage.cost. Nil when it said nothing.
	CostUSD *float64 `json:"cost_usd,omitempty" doc:"What the vendor says it charged for the call, in US dollars, such as OpenRouter's usage.cost. Absent when it said nothing; then the cost comes from the price table."`
	// CostPartial means CostUSD leaves out part of the charge: an OpenRouter
	// call on the person's own provider key (BYOK) whose reply gave
	// OpenRouter's fee but not the provider's charge.
	CostPartial bool `json:"cost_partial,omitempty" doc:"cost_usd leaves out part of the charge, such as the provider's charge on an OpenRouter call with your own key (BYOK) that OpenRouter did not report. The call's cost is then incomplete."`
}

// WorkspaceFolderEntry is one folder of a workspace with several, on its
// workspace.created.
type WorkspaceFolderEntry struct {
	Name      string `json:"name" doc:"The folder's name: its place under the copy's top, and the group its files are listed under. Set when the workspace is made."`
	Folder    string `json:"folder,omitempty" doc:"The folder's path, kept only on the machine that owns it."`
	FolderRef string `json:"folder_ref" doc:"The folder's reference (fld_ and 16 hex digits)."`
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
	// Owner is the owner whose authority the event happened under: the
	// record's owner id (store.Owner), set by the store on every event it
	// keeps, beside the actor (Turn, Origin). It is history: a later change of
	// access never rewrites it.
	Owner string `json:"owner,omitempty" doc:"The owner whose authority this happened under: the id of the one owner of this record (own_ and 16 hex digits), made once. Every event carries it beside who acted: an agent's turn (turn, origin), or the person. A later change of access never rewrites it. Absent on events stored before 0.6.x."`

	Model        string   `json:"model,omitempty" doc:"On model.requested: the model asked for. On a run's turn.started and run.started: the model the run was started with, absent for the harness's default; a session's next turn that names no agent and no model keeps it."`
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
	// Rule, on tool.refused, approval.requested and approval.decided. A
	// refusal names the forbid entry that matched. An approval names the first
	// entry an always would move to allow: a matched ask entry, or a suggested
	// tool name or folder when none matched.
	Rule string `json:"rule,omitempty" doc:"On tool.refused: the forbid entry of permissions.toml that matched, such as git push. On approval.requested and approval.decided: the first entry an always would move to allow, a matched ask entry or a suggested tool name or folder when none matched."`
	// Approval, Commands and Rules, on approval.requested and approval.decided:
	// the held reply a person answers. Approval is its id, the same on the
	// request and its decision, and the id in POST /api/v0/approvals/{id};
	// control derives the pending list from the record (a requested with no
	// decided), so it needs no per-gateway memory. Commands and Rules are on
	// the request only.
	Approval string   `json:"approval,omitempty" doc:"On approval.requested and approval.decided: the held reply's id, the same on both, and the id in POST /api/v0/approvals/{id}."`
	Commands []string `json:"commands,omitempty" doc:"On approval.requested: every ask command in the reply, in order, deduplicated. A cockpit shows one card with a row per command; an allow or a refusal covers them all."`
	Rules    []string `json:"rules,omitempty" doc:"On approval.requested: the ask entries of permissions.toml the commands matched, deduplicated. An allow moves each from the ask list to the allow list."`
	// Server, Tools and ToolsHidden, only on tools.listed: the tool
	// server's name, the tools it served the turn, and the harness's own
	// tools hidden from the turn.
	Server      string       `json:"server,omitempty" doc:"On tools.listed: the tool server's name, as the harness puts it in tool names: agentbus."`
	Tools       []string     `json:"tools,omitempty" doc:"On tools.listed: the names of the tools the server served the turn, in its order. Absent when it served none."`
	ToolsHidden []HiddenTool `json:"tools_hidden,omitempty" doc:"On tools.listed: the harness's own tools the turn does not get, each with why. Absent when none are hidden."`
	// History, only on history.shortened: the step own replay took.
	History *HistoryStep `json:"history,omitempty" doc:"On history.shortened: how own replay shortened the history it put in front of the turn."`
	// Added, only on context.added: each piece AgentBus gave the turn.
	Added []Added `json:"added,omitempty" doc:"On context.added: each piece AgentBus gave the turn besides the person's words, in the order the agent got them."`
	// Call, only on call.refused and vendor.called: method and vendor path.
	Call string `json:"call,omitempty" doc:"On call.refused and vendor.called: the call's method and vendor path, such as POST /v1internal:loadCodeAssist. Never its query or body."`
	// Host is what a closed-off agent asked to reach.
	Host string `json:"host,omitempty" doc:"On network.refused: the host and port the agent asked for, such as example.com:443."`
	// Command, only on the retired command.* events of records from before
	// 0.5: the person's shell command and how it ended.
	Command *Command `json:"command,omitempty" doc:"On command.ran: the person's shell command, how it ended and its output."`
	// Runner, only on runner.enrolled and runner.gone: the runner's id.
	Runner string `json:"runner,omitempty" doc:"On runner.enrolled and runner.gone: the runner's id."`
	// Setting, Keys and File, only on settings.changed: which setting
	// changed, the names of what changed, and on an import the file it came
	// from. Never a value.
	Setting string   `json:"setting,omitempty" doc:"On settings.changed: permissions, instructions or agents."`
	Keys    []string `json:"keys,omitempty" doc:"On settings.changed: the names of what changed, never a value: policy keys such as limits.max_depth, text, agent ids or id.field. Absent when the import found nothing changed from the defaults."`
	File    string   `json:"file,omitempty" doc:"On settings.changed of the one-time import: the file the setting came from, permissions.toml, instructions.md or agents.json. Before 0.5: the file written."`
	// SettingsVersion, on settings.changed and a run's turn.started: the
	// settings version.
	SettingsVersion int64 `json:"settings_version,omitempty" doc:"On settings.changed: the settings version the change made. On a run's turn.started: the settings version the turn started under. It moves up by one with every change; absent before 0.5."`
	// RulesVersion, on a run's turn.started: the version of the rules (the
	// permissions) the turn took when it started. Everything the turn does is
	// decided by them: its commands, its tools and its calls to the API.
	RulesVersion string `json:"rules_version,omitempty" doc:"On a run's turn.started: the version of the permissions the turn took when it started, as GET /api/v0/settings/permissions sends it in ETag. Everything the turn does is decided by these rules: its commands, its tools and its calls to the API. A change of the permissions applies from the next turn. Absent before 0.6.x."`
	// Title and OldTitle, only on session.renamed: the new name and the one
	// it replaced.
	Title    string `json:"title,omitempty" doc:"On session.renamed: the session's new name."`
	OldTitle string `json:"old_title,omitempty" doc:"On session.renamed: the name the session had before, absent when it had none."`
	// Archived, only on session.archived: true archived the session, false
	// un-archived it. The latest session.archived decides. Absent (false) is
	// an un-archive.
	Archived bool `json:"archived,omitempty" doc:"On session.archived: true hid the session from the default list, false un-hid it. The latest one decides."`

	Dropped  int      `json:"dropped,omitempty" doc:"On events.dropped: how many events were lost. Never 0."`
	Sessions []string `json:"sessions,omitempty" doc:"On events.dropped: every session that lost events."`
	// AllSessions, only on events.dropped: more sessions lost events than the
	// report can list, so every session counts as affected. Sessions is empty.
	AllSessions bool `json:"all_sessions,omitempty" doc:"On events.dropped: too many sessions to list, so every session counts as affected."`

	// Harness and Folder, on turn.started and run.started: the harness a
	// run started, such as claude, and the folder it started in. On
	// turn.started they are empty for a session no run started, such as one
	// pointed at the gateway by hand. The JSON name stays agent, for the
	// records already kept.
	Harness string `json:"agent,omitempty" doc:"On turn.started and run.started: the run's agent, such as claude. Empty on a turn of a session no run started."`
	Folder  string `json:"folder,omitempty" doc:"On turn.started and run.started: the folder the run's agent started in. Empty on a turn of a session no run started."`
	// FolderRef: the reference of the person's folder the session was
	// started in, issued by its connector. The path stays there.
	FolderRef string `json:"folder_ref,omitempty" doc:"On turn.started, run.started, workspace.created and folder.registered: the reference (fld_ and 16 hex digits) of the folder, as its connector issued it. Absent for a session started before references, or in a folder no connector owns."`
	// Workspace, Machine and Sandbox name a workspace and its mode.
	// Workspace is on workspace.created, workspace.applied,
	// workspace.discarded and every turn.started (the workspace the turn ran
	// in). Machine is the connector that owns the workspace's folder. Sandbox
	// is the workspace's mode, only on workspace.created: a pointer so its
	// false (the person's real folder) is always present, told apart from a
	// kind that carries no mode at all.
	Workspace string `json:"workspace,omitempty" doc:"On workspace.created, workspace.applied, workspace.discarded and turn.started: the workspace the turn ran in (ws_ and 16 hex digits)."`
	Machine   string `json:"machine,omitempty" doc:"On workspace.created and folder.registered: the connector that owns the folder (con_ and 16 hex digits). Absent when no connector owns the folder, as for hook or gateway traffic."`
	Sandbox   *bool  `json:"sandbox,omitempty" doc:"On workspace.created: the workspace's mode, true for a copy behind a wall and false for the person's real folder. Always present on workspace.created; absent on every other kind."`
	// Folders, only on workspace.created of a workspace with several folders:
	// each folder, in order, with its name. Folder and FolderRef are then
	// empty, so no reader takes one folder for the whole workspace. A
	// workspace with one folder keeps it in Folder and FolderRef, as before.
	Folders []WorkspaceFolderEntry `json:"folders,omitempty" doc:"On workspace.created of a workspace with several folders: each folder, in order. Absent for a workspace with one folder, which names it in folder and folder_ref."`
	// AgentID, Access and KeyEnv, on a run's turn.started and run.started:
	// the saved agent the run started, how it reaches its model, and for an
	// OpenRouter agent the name of the variable that holds its key. Never
	// the key itself. A session's next turn reads them back.
	AgentID string `json:"agent_id,omitempty" doc:"On a run's turn.started and run.started: the saved agent the run started, as GET /api/v0/agents names it. Absent for a turn started by harness name alone."`
	Access  string `json:"access,omitempty" doc:"On a run's turn.started and run.started: how the agent reaches its model: subscription, openrouter or api. Absent means subscription."`
	KeyEnv  string `json:"key_env,omitempty" doc:"On a run's turn.started and run.started of an OpenRouter agent: the name of the environment variable that holds the key. Never the key."`
	Hold    string `json:"hold,omitempty" doc:"On run.started and run.ended: names one run's child."`
	// ExitCode, only on run.ended from a run: the child's exit code, or
	// 128 plus the signal that ended it. Nil for a child closed by a service
	// restart.
	ExitCode *int `json:"exit_code,omitempty" doc:"On a run's run.ended: the child's exit code, or 128 plus its signal."`
	// ResumeID, only on run.ended from a run or a turn: the vendor's own
	// session id, read from the child's stdout (claude's session_id, codex's
	// thread_id). It is what the session's next turn continues.
	ResumeID string `json:"resume_id,omitempty" doc:"On a run's run.ended: the vendor's own session id, which the next turn continues."`

	// ErrorTail, only on a run's run.ended of a child that exited with
	// an error and was not stopped: the last lines it wrote to stderr,
	// bounded, with anything shaped like a secret hidden.
	ErrorTail string `json:"error_tail,omitempty" doc:"On a run's run.ended of an agent that exited with an error: the last lines it wrote to stderr, at most 20 lines and 2 KiB, with anything shaped like a token or key replaced by [hidden]. Absent when it exited 0, was stopped, or wrote nothing there, and when all it kept is one line whose start was cut (it may begin inside a secret)."`
	// State, only on a run's run.ended: how the turn ended, as the
	// turn list says it (finished, failed or stopped). With failed, Error
	// says why (ErrorLine).
	State string `json:"state,omitempty" doc:"On a run's run.ended: how the turn ended: finished (its agent exited 0), stopped (someone stopped it) or failed (anything else). A failed turn's error says why: the agent's own error line from its stderr, else its exit code, or why it could not start. Absent on records from before 0.5.7."`

	// Changeset and Checkpoint, only on turn.changed: what an apply up to the
	// turn writes, kept apart from Changes (the file list), which a finish may
	// not carry. The record owns which turns are appliable: a turn is appliable
	// only with a changeset.
	Changeset  string `json:"changeset,omitempty" doc:"On turn.changed: the copy at the turn's end, from its base, as kept in control's store: what an apply up to the turn writes, unless the turn heads an update in force, whose merge it writes then. In an isolated copy only. Absent when it could not be kept, so the turn cannot be applied; and on turns recorded before 0.6.6, whose workspace's workspace.snapshot holds it."`
	Checkpoint string `json:"checkpoint,omitempty" doc:"On turn.changed: the copy's checkpoint at the turn's end. With a file list, the same as changes.to."`
	// Snapshot, only on workspace.snapshot: the workspace's applied state
	// when control took it.
	Snapshot *applied.AppliedSnapshot `json:"snapshot,omitempty" doc:"On workspace.snapshot: the workspace's applied state when control took it -- its appliable turns with their changesets, how far a successful apply went and what it wrote, the update in force -- standing for every apply, discard, update and turn.changed of the workspace before it."`
	// Changes, only on turn.changed: what the turn changed.
	Changes *TurnChanges `json:"changes,omitempty" doc:"On turn.changed: every file the turn added, modified or deleted, with diffs."`
	// Applied, on turn.applied and workspace.applied: what the apply did.
	Applied *Applied `json:"applied,omitempty" doc:"On turn.applied and workspace.applied: the turns applied, the from/to changesets, who asked, the result and the files."`
	// PutAway, on workspace.closed and workspace.kept: a put-away's why.
	PutAway *PutAway `json:"put_away,omitempty" doc:"On a workspace.closed AgentBus asked for itself, and on workspace.kept: the on and the record event that queued the put-away, when it was queued and came due, and on workspace.kept why the copy was kept."`
	// Discarded, only on workspace.discarded: what the discard dropped.
	Discarded *Discarded `json:"discarded,omitempty" doc:"On workspace.discarded: the changeset the workspace was reset to, the turns dropped, and who asked."`
	// Updated, only on workspace.updated: what the update from the folder did.
	Updated *Updated `json:"updated,omitempty" doc:"On workspace.updated: the result, the new baseline (from) and merged (to) changesets on a clean update, the folded turns, the conflicting files on a conflict, and who asked."`
	// Previewed, only on changes.previewed: the apply dry run's result.
	Previewed *Previewed `json:"previewed,omitempty" doc:"On changes.previewed: what applying the workspace's changes would do to the folder now, listed and paged by GET /api/v0/workspaces/{id}/changes."`
	// Plan, only on plan.usage: the reading.
	Plan *Plan `json:"plan,omitempty" doc:"On plan.usage: how much of the subscription's plan is used in one window."`

	// ReplayFrom, only on turn.started: the earlier turn of the session whose
	// history the gateway puts in front of every model request of this turn,
	// in place of the vendor's own resume. Empty when the turn continues by
	// the vendor's resume, or is a session's first.
	ReplayFrom string `json:"replay_from,omitempty" doc:"On turn.started: the session's earlier turn whose history the gateway replays in front of every model request of this turn, in place of the vendor's resume. Absent for a first turn and for a turn continued by the vendor's resume."`
	// Unsettled, only on a run's run.ended: the gateway could not
	// confirm that all of the turn's model calls were stored (it gave up
	// waiting, or a queue was full), so the turn may be missing calls.
	Unsettled bool `json:"unsettled,omitempty" doc:"On a run's run.ended: the gateway could not confirm that every model call of the turn was stored, so the turn may be missing calls. Own replay does not continue from such a turn."`
	// Stopped, only on a run's run.ended: the person stopped the turn
	// (POST /api/v0/turns/{id}/stop) while its child ran.
	Stopped bool `json:"stopped,omitempty" doc:"On a run's run.ended: the person stopped the turn with POST /api/v0/turns/{id}/stop while its child ran. The session's next turn continues after it, told that the person stopped it."`
	// Continuity, only on a next turn's turn.started: how it reaches the
	// session's earlier turns, "replay", "resume" or "transcript".
	Continuity string `json:"continuity,omitempty" doc:"On the turn.started of a session's next turn started by POST /api/v0/turns: replay (own replay, see replay_from), resume (the vendor's resume_id) or transcript (the turn's harness differs from the previous turn's, so it got the earlier turns as plain text, see context). Absent on a first turn."`
	// Context, only on the turn.started of a next turn: the earlier turns as
	// plain text when its continuity is transcript (before 0.5, then the
	// person's shell commands since the previous turn). The agent's prompt
	// was Context followed by Text; Text stays the person's own words.
	Context string `json:"context,omitempty" doc:"On a turn.started whose continuity is transcript: the session's earlier turns as plain text, each person's message and the agent's final reply, oldest first. Before 0.5 it could also hold the shell commands the person ran since the previous turn (command.ran), after any transcript. The agent got context followed by text as its prompt; text is the person's own words."`
	// RecoveredFrom and RecoveredReason, only on a next turn's turn.started:
	// the turn before it did not finish (it was interrupted, its last model
	// call failed, or it never reached the model), and this turn continues
	// after it anyway. The replayed history ends with a note saying so.
	RecoveredFrom   string `json:"recovered_from,omitempty" doc:"On a replayed turn's turn.started: the session's previous turn, which did not finish (interrupted, its last model call failed, or it never reached the model). This turn continues after it, and the model is told what happened. Absent when the previous turn finished."`
	RecoveredReason string `json:"recovered_reason,omitempty" doc:"With recovered_from: one sentence saying how that turn ended, such as its last model request getting status 529. The same words the model is told."`
	// Unkept, only on a run's turn.started: settings its agent has no way to
	// keep, which the turn runs without, each one plain sentence.
	Unkept []string `json:"unkept,omitempty" doc:"On a run's turn.started: the settings this turn's agent has no way to keep, so it ran without them, each in one plain sentence, such as agy's subagents. Show it as a quiet warning. Absent when it kept them all."`
	// IdempotencyKey and RequestHash, only on the turn.started of a turn
	// started by POST /api/v0/turns with idempotency_key: the client's key,
	// and the SHA-256 of the request's fields, so a retry with the same key
	// finds this turn and a different request under it is refused.
	IdempotencyKey string `json:"idempotency_key,omitempty" doc:"On turn.started: the idempotency_key POST /api/v0/turns was sent with. A retry with the same key returns this turn and starts nothing."`
	RequestHash    string `json:"request_hash,omitempty" doc:"With idempotency_key: the SHA-256, in hex, of the request's fields. A request with the same key and other fields is refused."`
	// Origin, on a run's turn.started: the turn was not typed by the
	// person. Another agent's turn asked for it through the API, or AgentBus
	// started it to deliver the result of work this session asked for. On a
	// session.created or workspace.created: the agent's turn that made it.
	Origin *Origin `json:"origin,omitempty" doc:"On a run's turn.started: who started this turn when the person did not. On session.created and workspace.created: the agent's turn that made it (kind asked). On a program's own event: the agent's turn that wrote it. Absent for what the person started or made."`
	// Replayed, only on model.requested: how many messages of the earlier
	// turn's history the gateway put in front of this request.
	Replayed int `json:"replayed,omitempty" doc:"On model.requested: how many messages of the earlier turn's history the gateway put in front of this request. Absent when it put none."`

	// Data, only on a program's own event (kind x.<program>.<name>): the JSON
	// object it sent.
	Data json.RawMessage `json:"data,omitempty" doc:"On a program's own event: the JSON object it sent."`

	Unexpected *Unexpected `json:"unexpected,omitempty" doc:"On schema.unexpected: the unknown shape."`
}

// Command is one shell command the person ran in a session's copy
// (command.ran, retired in 0.5: only in older records).
type Command struct {
	ID   string `json:"id" doc:"The command's id, as the 0.4 route answered it: cmd_ and 16 hex digits."`
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

// TurnChanges is what one turn changed (turn.changed).
type TurnChanges struct {
	Isolated bool   `json:"isolated" doc:"The turn ran in an isolated copy, between two checkpoints: POST /api/v0/turns/{id}/apply can put its changes in the person's folder."`
	From     string `json:"from,omitempty" doc:"The checkpoint the turn started from. In an isolated copy only."`
	To       string `json:"to,omitempty" doc:"The checkpoint the turn ended with. The same as from: the turn changed nothing."`
	Count    int    `json:"count" doc:"The files the turn added, modified or deleted; with truncated, see there."`
	// Truncated: Files is not every change.
	Truncated bool `json:"truncated,omitempty" doc:"files is not every change: files were left out past 1000, or to fit the runner's finish, and count still counts them; or the copy held more files than a checkpoint keeps, and count misses some too."`
	// Partial: taken while the turn may still have run.
	Partial bool `json:"partial,omitempty" doc:"Taken while the turn may still have run, so it may miss later changes."`
	// DiffsCut: some files' diffs were left out, as all diffs together
	// passed 1 MiB.
	DiffsCut       bool          `json:"diffs_cut,omitempty" doc:"Some text files have no diff here: all the turn's diffs together passed 1 MiB, and the later ones were left out (each marked diff_left_out)."`
	ContentLeftOut bool          `json:"content_left_out,omitempty" doc:"Some small text files have no content here, to keep the event small."`
	Error          string        `json:"error,omitempty" doc:"The workspace could not be read at the end of the turn."`
	Files          []ChangedFile `json:"files" doc:"Each changed file, sorted by path."`
}

// ChangedFile is one file a turn changed.
type ChangedFile struct {
	Path   string `json:"path"`
	Change string `json:"change" doc:"added, modified or deleted."`
	Size   int64  `json:"size" doc:"The file at the end; for a deleted file, at the start."`
	SHA256 string `json:"sha256" doc:"The file's SHA-256 at the end; for a deleted file, at the start."`
	// SHA256Before is the file at the start; empty for an added file.
	SHA256Before string  `json:"sha256_before,omitempty" doc:"The file's SHA-256 at the turn's start. Absent for an added file."`
	Diff         *string `json:"diff,omitempty" doc:"The file's unified diff, from the turn's start to its end, for small text files whose earlier content is kept."`
	DiffLeftOut  bool    `json:"diff_left_out,omitempty" doc:"The content changed and no diff is here: a binary file, a link, a file over 256 KiB, past the 1 MiB of all diffs, or its earlier content is no longer kept."`
	Content      *string `json:"content,omitempty" doc:"The whole file at the end, when it is small UTF-8 text."`
}

// Plan is one reading of a subscription's plan usage in one window, from a
// model reply's headers.
type Plan struct {
	Subscription string `json:"subscription" doc:"The subscription the call used: the harness whose sign-in control added, such as claude or codex."`
	Connection   string `json:"connection,omitempty" doc:"Names the connect of the subscription the call's credential came from: its connected_at. A reading is shown only while that connect is current, so a reply on an earlier sign-in never shows under a newer one."`
	Window       string `json:"window" doc:"The vendor's name for the window: 5h or 7d for claude, primary or secondary for codex."`
	// WindowMinutes is the window's length: fixed for claude's named
	// windows, as codex says for its own.
	WindowMinutes int        `json:"window_minutes,omitempty" doc:"The window's length in minutes, such as 300 (5 hours) or 10080 (a week). Absent when the vendor did not say."`
	UsedPercent   float64    `json:"used_percent" doc:"How much of the window is used, 0 to 100, as the vendor says. It can pass 100 when the plan is over its limit."`
	ResetsAt      *time.Time `json:"resets_at,omitempty" doc:"When the window resets, as the vendor says. Absent when it did not say."`
	Status        string     `json:"status,omitempty" doc:"The vendor's own word for the window, such as allowed or rejected (claude). Absent when it sent none."`
}

// Applied is what one apply did (turn.applied and workspace.applied).
type Applied struct {
	Turns []string `json:"turns" doc:"Every turn whose changes the apply carried to the folder, oldest first: the turns after the workspace's last apply, up to the one asked for."`
	// From and To are the changeset range the apply carried, on
	// workspace.applied: from the last applied (or discarded) baseline to the
	// turn's end. Empty on a pre-workspace turn.applied.
	From      string        `json:"from,omitempty" doc:"On workspace.applied: the baseline changeset the apply started from (the last applied or discarded one, or base). Absent on a pre-workspace turn.applied."`
	To        string        `json:"to,omitempty" doc:"On workspace.applied: the changeset the apply carried up to. Absent on a pre-workspace turn.applied."`
	By        *By           `json:"by,omitempty" doc:"Who asked for the apply. Absent for an apply made before 0.5."`
	Result    string        `json:"result" doc:"applied, refused (a file in the folder changed, or the turn cannot be applied: error says why), or failed (the apply did not finish)."`
	Files     []string      `json:"files,omitempty" doc:"The files the apply wrote or removed in the person's folder, sorted."`
	Count     int           `json:"count,omitempty" doc:"How many files the apply wrote or removed. files may list fewer, with truncated."`
	Truncated bool          `json:"truncated,omitempty" doc:"files or conflicts was cut to keep the event small."`
	Pruned    bool          `json:"pruned,omitempty" doc:"The checkpoints before the turn's end were removed."`
	Conflicts []RefusedFile `json:"conflicts,omitempty" doc:"On a refusal: each file in the folder that is neither as it was at the turn's start nor as the turn left it, with why. Nothing was written."`
	Error     string        `json:"error,omitempty" doc:"Why it was refused or failed."`
}

// RefusedFile is one file that refused an apply.
type RefusedFile struct {
	Path    string `json:"path"`
	Why     string `json:"why"`
	Markers bool   `json:"markers,omitempty" doc:"The file still holds the conflict markers an update's merge left (<<<<<<< this session ... >>>>>>> your folder), rather than having changed in the folder: the agent combining it in a turn clears it, an update does not."`
}

// PutAway is why AgentBus put a workspace's copy away, or kept it
// (workspace.closed, workspace.kept).
type PutAway struct {
	On     string    `json:"on" doc:"The on that queued the put-away: the built-in one over workspace.applied or workspace.discarded."`
	Event  string    `json:"event" doc:"The record event the on fired on: the apply or discard that settled the workspace."`
	Queued time.Time `json:"queued" doc:"When the put-away was queued."`
	Due    time.Time `json:"due,omitempty" doc:"When it came due: the event's time plus the quiet time of the settings ([workspaces] put_away_after_minutes)."`
	Kept   string    `json:"kept,omitempty" doc:"On workspace.kept: why the copy was kept."`
}

// Discarded is what one discard dropped (workspace.discarded).
type Discarded struct {
	// ResetTo is the checkpoint the workspace's copy was reset to: the last
	// applied turn's end checkpoint, or "base" when the workspace was never
	// applied.
	ResetTo string `json:"reset_to,omitempty" doc:"The checkpoint the workspace's copy was reset to: the last applied turn's, or base when never applied."`
	// DroppedTurns are the unapplied turns whose file changes were dropped,
	// oldest first. Their conversation stays in the record.
	DroppedTurns []string `json:"dropped_turns,omitempty" doc:"The unapplied turns whose file changes the discard dropped, oldest first. The conversation stays."`
	By           *By      `json:"by,omitempty" doc:"Who asked for the discard, by the kind of token it came with. Never the token itself."`
}

// Updated is what one update from the folder did (workspace.updated). On a
// clean merge From is the new baseline changeset (the folder's current state,
// which the next Apply diffs against) and To the merged changeset (the copy's
// new head); FoldedTurns are the turns whose changes the merge folded into To,
// so they are no longer applied on their own. On a conflict Conflicts names the
// files and nothing changed.
type Updated struct {
	Result      string   `json:"result" doc:"updated (the folder was merged into the copy), merged (a merge: as updated, with the files in unresolved left holding conflict markers for the agent) or conflict (the same lines changed on both sides; nothing changed)."`
	From        string   `json:"from,omitempty" doc:"On a clean update: the new baseline changeset, the folder as it is now. A later Apply diffs the session's work against it."`
	To          string   `json:"to,omitempty" doc:"On a clean update: the merged changeset the copy now holds."`
	FoldedTurns []string `json:"folded_turns,omitempty" doc:"The turns whose changes the merge folded into the copy's new head, oldest first; they are no longer applied on their own."`
	HeadTurn    string   `json:"head_turn,omitempty" doc:"On a clean update: the workspace's latest unapplied turn, kept as the copy's head. An apply up to it writes the merged changeset (to). Absent when nothing was unapplied."`
	// TheirsCheckpoint and MergedCheckpoint are the copy's own checkpoints of
	// from and to: the merge base of the next update, and the reset point of a
	// discard after the merge is applied.
	TheirsCheckpoint string   `json:"theirs_checkpoint,omitempty" doc:"On a clean update: the copy's checkpoint of the folder as it was read (from)."`
	MergedCheckpoint string   `json:"merged_checkpoint,omitempty" doc:"On a clean update: the copy's checkpoint of the merge (to)."`
	By               *By      `json:"by,omitempty" doc:"Who asked for the update, by the kind of token it came with. Never the token itself."`
	Conflicts        []string `json:"conflicts,omitempty" doc:"On a conflict: the folder-relative files that conflicted, sorted. Nothing changed; the agent can be asked to resolve them in a turn."`
	Unresolved       []string `json:"unresolved,omitempty" doc:"On a merge: the folder-relative files that changed on both sides, sorted, now in the copy with both sides between conflict markers."`
	Unmergeable      []string `json:"unmergeable,omitempty" doc:"On a merge that changed nothing (result conflict): the conflicting files conflict markers cannot show -- not text, a link, or removed on one side -- sorted."`
	Error            string   `json:"error,omitempty" doc:"Why the update failed."`
}

// Previewed is an apply's dry run (changes.previewed): what applying the
// session's changes would do to the person's folder now, nothing written.
type Previewed struct {
	// Turn and Applies are the session's appliable cursor when the preview was
	// taken: the turn it goes up to, and how many apply jobs the session had.
	// The route reuses the preview only while the session's current cursor
	// still equals these, so a new appliable turn or any apply (even a refused
	// one) retakes it, while a not-yet-ended turn or a failed upload -- neither
	// of which is appliable -- does not.
	Applies   int             `json:"applies,omitempty" doc:"How many apply jobs the session had when the preview was taken; part of the cursor it is reused against."`
	Turn      string          `json:"turn,omitempty" doc:"The turn an apply would go up to; POST /api/v0/workspaces/{id}/apply applies the workspace's changes up to it."`
	Turns     []string        `json:"turns,omitempty" doc:"Every turn whose changes that apply would carry, oldest first."`
	Count     int             `json:"count" doc:"Every file the apply would write or remove, and every conflict, counted whole; files may list fewer, with truncated."`
	Truncated bool            `json:"truncated,omitempty" doc:"files is not every file: too many to send, so later ones were left out; count still counts them, so count minus the files listed is how many are not here."`
	DiffsCut  bool            `json:"diffs_cut,omitempty" doc:"Some text files have no diff: all diffs together passed 1 MiB and the later ones were left out (each diff_left_out)."`
	Files     []PreviewedFile `json:"files" doc:"Each file the apply would write or remove, and each conflict, sorted by path."`
	Conflicts []RefusedFile   `json:"conflicts,omitempty" doc:"Each file in the folder that would refuse the apply, with why. Those files are in files too."`
}

// PreviewedFile is one file an apply would change (changes.previewed).
type PreviewedFile struct {
	Path        string  `json:"path"`
	Change      string  `json:"change" doc:"added, modified or deleted, against the folder now."`
	Added       int     `json:"added" doc:"Lines the change adds. 0 when no diff could be made."`
	Removed     int     `json:"removed" doc:"Lines the change removes. 0 when no diff could be made."`
	Diff        *string `json:"diff,omitempty" doc:"The unified diff from the folder now to what the apply would write, for small text on both sides."`
	DiffLeftOut bool    `json:"diff_left_out,omitempty" doc:"The content changes and no diff is here: binary, a link, over 256 KiB, or past the 1 MiB of all diffs."`
}

// By says who asked for something, by the kind of token it came with. Never
// the token itself.
type By struct {
	Kind    string `json:"kind" doc:"person (the main token) or agent (a turn's own token)."`
	Session string `json:"session,omitempty" doc:"agent: the session of the turn whose token asked."`
	Turn    string `json:"turn,omitempty" doc:"agent: the turn whose token asked."`
}

// The kinds of a By.
const (
	ByPerson = "person"
	ByAgent  = "agent"
)

// HiddenTool is one of a harness's own tools a turn does not get.
type HiddenTool struct {
	Name string `json:"name" doc:"The tool's name, as the harness names it, such as Task."`
	Why  string `json:"why" doc:"Why the turn does not get it, such as replaced by AgentBus tools."`
}

// HistoryStep is one step of own replay's history budget (history.shortened).
type HistoryStep struct {
	From             string `json:"from" doc:"The earlier turn whose history this turn carries."`
	Model            string `json:"model,omitempty" doc:"The model the window is for, from the turn's request."`
	WindowTokens     int    `json:"window_tokens" doc:"The model's assumed context window in tokens (the context view's window_tokens)."`
	BudgetTokens     int    `json:"budget_tokens" doc:"The history budget in tokens: the budget share of window_tokens. Over it, the history is shortened to about half of it."`
	BeforeTokens     int    `json:"before_tokens" doc:"The history's size before the step: a rough count (bytes / 2.66, an image counted as 1,600), not the vendor's."`
	AfterTokens      int    `json:"after_tokens" doc:"The history's size after the step, counted the same way."`
	ResultsShortened int    `json:"results_shortened,omitempty" doc:"Tool results over 2 KiB made a stub: what it was, how it began, and where the agent fetches it back."`
	InputsShortened  int    `json:"inputs_shortened,omitempty" doc:"Tool-call inputs over 2 KiB made a stub the same way."`
	TurnsSummarised  int    `json:"turns_summarised,omitempty" doc:"The oldest turns now in AgentBus's summary: the person's words and the model's last words verbatim, AgentBus's notes, a line per tool call. An earlier summary is kept word for word inside it."`
	TurnsLeftOut     int    `json:"turns_left_out,omitempty" doc:"The oldest turns' logs this step left out of the summary, for one line naming how many turns and their range: the summary stays bounded however long the session runs. Their records stay in full."`
	NewestShortened  bool   `json:"newest_shortened,omitempty" doc:"The newest earlier turn was shortened too, because the history did not fit the budget otherwise: the budget wins over keeping it whole."`
	TurnsWhole       int    `json:"turns_whole,omitempty" doc:"The newer turns besides the newest that are not in the summary (their long tool output may be stubs)."`
}

// Added is one piece of what AgentBus gave a turn besides the person's
// words (context.added).
type Added struct {
	Kind  string `json:"kind" doc:"agents_md, instructions, asked_by, mcp_instructions, or commands in records from before 0.5."`
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
	// its previous turn (command.ran), with their output. Retired in 0.5:
	// only in older records.
	AddedCommands = "commands"
)

// Origin says who started a turn when the person did not.
type Origin struct {
	Kind string `json:"kind" doc:"asked: another agent's turn started this turn through the API with its turn token. result: AgentBus started this turn to deliver the result of work this session asked for; text is that result. Its prompt starts [AgentBus] This message is from AgentBus, not from the person, and names the agent, the turn, its state and session, the changed files (at most 50), a review's verdict and findings, and the reply (at most 8 KiB)."`
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
