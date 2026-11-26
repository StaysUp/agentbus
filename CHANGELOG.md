# Changelog

All notable changes to AgentBus. Full notes for each release are on the
[releases page](https://github.com/staysup/agentbus/releases).

## 0.6.6 (2026-11-26)

### Added
- **Close a workspace.** `POST /api/v0/workspaces/{id}/close` ends a Copy workspace's copy for good:
  the copy, its checkpoints and its applied state. Sessions and the record stay, and a session's
  next turn gets a fresh copy. A close waits behind any turn, apply, discard or update in that
  workspace. The cockpit lists each machine's open workspaces on its runner page, with Close.
- **The owner on the record.** Every stored event carries the owner's id (`owner`, one per data
  folder) beside the agent that acted. Events are history: changing access never rewrites them.
- **One rule set per turn.** A turn runs under the rules saved when it starts: its commands, its
  tools and its API calls alike, and `turn.started` records which (`rules_version`). An edit to
  the rules applies from the next turn. A start sent again, the copy's preparation and a reopened
  store all keep the turn's saved rules.
- **OpenCode on xAI**, with the xAI API key set in Settings.
- **A session in several folders.** `POST /api/v0/workspaces` takes `folders` (2 to 8, on one
  machine, Copy only). The copy holds each folder side by side under its name, and the agent works
  at its top. Review lists every folder's files under its name. Apply checks every folder before it
  writes any, and a conflict in one refuses the whole apply. Discard and Update cover every folder.
  A file the agent leaves beside the folders, in none of them, refuses the apply until it is moved
  or discarded.

### Changed
- **A turn is queued first.** `POST /api/v0/turns` answers at once, and the `wait` parameter is
  gone: it held the reply up to 30 seconds and refused an agent's token without `wait=false`. A
  turn that cannot start, such as one whose program is missing on the runner, is recorded as
  failed with its reason.
- **One auth check.** Every route asks the caller's grant one question, whether an action may
  happen in a place, and the grant answers. Who asked whom is stored once.
- **An agent reads the sessions of the workspaces in its reach**, by one rule, and may watch any
  turn it may read. A watch an agent registers counts under the job limits.
- **An approval answered already replies 409**, as one still being answered does. 404 means no
  approval by that id.
- **An agent may do what you may, narrowed by three things only.** Where it works (a Copy session
  reaches its own workspace and the copies its session made; a Direct session reaches its machine),
  your brakes (Agents may apply, the command and tool rules, the limits on depth, jobs and cost),
  and it never changes its own brakes (settings, saved agents, tools, runners, keys, subscriptions,
  approvals). So an agent may now discard, update or close a copy it made, list the sessions,
  workspaces and approvals it may read, continue or stop a session in its reach, archive one, and
  have a reply delivered to any session in its reach. A Direct agent may make a Direct workspace on
  its machine. A program event an agent writes names it as the origin.

### Removed
- **The file routes and folder grants.** `POST /api/v0/files/read`, `/list` and `/write` are gone,
  with the folder grants they used and the "always for this folder" answer that made one. Their
  tools were hidden on 2026-11-23 and nothing called the routes. An agent reaches files through its
  CLI's own tools. A record made before keeps its old grants table; nothing reads it.

### Fixed
- **A delivered reply escaped its request tree's limits.** A turn could make an empty session and
  have a reply delivered there. The delivered turn started a new tree at depth zero, with fresh
  depth, job and cost limits. A reply a turn asks for now stays in that turn's tree, whatever
  session it lands in.
- **The limits on asked work are counted by turn, in one place.** Every turn now records the
  request tree it belongs to when it is made, and the depth, job and cost limits are checked where
  any turn is made: asked work, a reply delivered to the asker, a reply an on delivers. Two gaps
  this closes: a turn watching itself kept getting new turns past the cost limit, and continuing
  another session counted that session's whole earlier cost against the asker's tree. Cost is
  now the tree's own turns only.
- **An empty `?session=` on the event stream is refused.** It was read as naming no place, so an
  agent's token got every session's events, other copies' diffs among them. Every route now
  refuses a missing or empty place, for every token.
- **Edits inside a repo nested in the folder were lost.** A committed repo inside a session's folder
  was recorded as a commit pointer only, so the agent's edits inside it never reached Review or
  Apply, and nothing said so. A new session's copy now records a nested repo's files as its own,
  found again at every checkpoint, so they are in Review, Apply, Discard and Update like any other.
  A repo the folder's `.gitignore` ignores stays out, as before. Sessions started earlier keep
  their old behaviour.
- The gateway decides a codex `exec` command known only at run time as `$cmd`. Thanks to Kofi
  Akpor (`akpor-kofi`, #80).

## 0.6.5 (2026-11-24)

### Added
- **Update from my folder.** When a Copy session's copy has fallen behind your folder (another session applied,
  or you edited), Update brings your folder into the copy by a three-way merge, so the session's work sits on top
  and the next Apply writes only that work. It never touches your folder.
- **Merge.** When Update finds files changed on both sides, Merge puts both sides into those files in the copy and
  asks the agent, in one goal sentence, to combine them. Apply refuses a file that still holds conflict markers.
- **Review the whole workspace.** `review_turn` with no turn reviews everything not yet applied, across turns.
  The turn instructions now point agents at the full API description.
- OpenCode sessions continue from AgentBus's own record, like Claude Code and Codex. Thanks to Alfred
  (`alfred-pithu`, #73).
- Codex cache-write tokens are read into usage and cost. Folder registration is recorded as `folder.registered`
  (the reference and the machine, never the path).
- A folder typed as `~` or `~/...` is the runner's home. Thanks to Suraj (`Suraj-Upadhyay`, #84).
- Codex's web search is recorded as a service call. Thanks to `akpor-kofi` (#78).

### Changed
- **Rules allow everything by default except subagents.** The default Allow list is now `*` (every command) and
  `tool:*` (every tool), Ask is empty, and Forbid holds `tool:subagent`. Forbid wins over Ask, Ask over Allow. A
  command or tool on none of the lists is held for a person. The redundant `tool:edit` default is gone.
- **The `unlisted` setting is retired.** An older `permissions.toml` or stored policy that still sets it keeps
  working, with the key left out.
- **Direct keeps no record of file changes.** A Direct session works in your folder as it is; AgentBus no longer
  hashes the folder at each turn. Its Changes tab says so.
- The folder tools `read_file`, `list_folder` and `write_file` are hidden from agents, Settings and the turn
  view. The HTTP routes stay for a later release.
- When a runner stops answering mid-turn, the turn ends with "The runner stopped answering; send the turn again."
- `-listen` on a busy port says to stop the other process or pick another port. Thanks to Alfred (#69).
- A source build without a packed cockpit says how to get one. Thanks to Alfred (#67).

### Breaking
- The events `session.opened` and `session.closed` are renamed `run.started` and `run.ended`: they mark one
  run of an agent, not the session. `session.registered` is retired. Old stores are read as before.
- The runners API no longer reports `sandbox`. It described the whole-agent wall retired in 0.6.4 and decided
  nothing; that wall's code is removed. A runner of an older build may still send the field; control ignores it.

### Fixed
- **Apply lost files on a case-only rename or through a link** (#65). An apply now looks each path up as the
  checkpoint lists it. Thanks to `atropos0902` for the report and the fix (#72).
- The gateway suggests no allow rule for a line the shell cannot parse. Thanks to `akpor-kofi` (#79).
- `tools/bench` applies through `POST /api/v0/workspaces/{id}/apply`.
- A test helper that read a queued run reply as finished made the integration tests fail under load.

## 0.6.4 (2026-11-23)

### Added
- **Use any model.** Claude Code can run on any model OpenRouter offers; the agent stays Claude
  Code, the model behind it changes.
- **OpenCode** joins as a harness, on an OpenRouter key.
- AgentBus records OpenRouter's Chat Completions format, with its cost.
- **Direct, Copy or Locked, per session.** Copy (the default) works in its own copy of your folder:
  commands write only inside the copy, reads stay open, the network and local services work as
  normal. Direct works in your folder itself. Locked comes later.
- **Workspaces, Review, Discard.** A Copy session's workspace holds its copy; read the changes, ask
  another model to check them with Review, then Apply or Discard.
- **One sign-in path (Connect) in every mode.** On `agentbus up` a turn runs in your real home, so
  your setup (`CLAUDE.md`, hooks, plugins, skills, `gh`, `aws`, `ssh`, `gitconfig`) comes along.
- A turn's record comes back in one read.

### Changed (breaking)
- Apply and preview act on a workspace: `POST /api/v0/workspaces/{id}/apply` replaces
  `POST /api/v0/turns/{id}/apply`, and `GET /api/v0/workspaces/{id}/changes` replaces
  `GET /api/v0/sessions/{id}/changes`.
- New routes: `POST /api/v0/sessions`, `POST /api/v0/workspaces`, `GET /api/v0/workspaces`,
  `GET /api/v0/workspaces/{id}`, `POST /api/v0/workspaces/{id}/discard`.
- The `[sandbox]` mode and `[network]` contain/allow settings are retired with the whole-process
  wall. An older `permissions.toml` or stored policy keeps working with those keys left out. The
  `isolation` and `home_files` settings stay.
- At startup, sessions are migrated to workspaces, idempotently and without deleting or moving
  anything on disk. Back up your data folder first.

### Fixed
- The file tools (`read_file`, `list_folder`, `write_file`) for a session whose folder is on another
  machine no longer act on control's own disk; they are refused with a 409. Thanks to Ahmed
  (`ahmed-elnashar`) for the fix (#56).
- `agentbus up` says which data folder it uses, and whether it is new.
- The install script prints the exact PATH line for your shell, with a literal `~`.
- Plainer words for two known failures (an agy sign-in that needs a retry, a sandbox the machine
  cannot build).
- Ask-flow cleanups.

### Cockpit
- A Direct / Copy / Locked selector in New Session (Locked disabled, "coming later").
- Review beside Apply in the changes panel; workspace status; Discard on a Copy session.
- Settings drops the Isolation group; it is now Subscriptions, Commands, Limits and Config.

## 0.6.3 (2026-11-20)

### Added
- **Archive replaces close.** Archiving hides a session from the list without deleting anything; it
  can be unarchived, and works even while a turn runs. Closing no longer deletes a session's copy.
- The record works as an event log that readers follow from where they left off; an event a reader
  keeps failing on is set aside rather than blocking the rest.

### Cockpit
- Archive and Unarchive replace Close; the list hides archived sessions, and Show archived brings
  them back.
</content>
