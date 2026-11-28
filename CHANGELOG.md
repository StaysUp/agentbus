# Changelog

All notable changes to AgentBus. Full notes for each release are on the
[releases page](https://github.com/staysup/agentbus/releases).

## Unreleased

### Changed
- **An on watches a session, not one turn.** `POST /api/v0/ons` takes
  `{"event":"turn.completed","filter":{"session":"S"}}`. Every turn of S that ends from then on has
  its reply delivered to a session, your own by default. It stays until you remove it. So a
  coordinator that watches a builder's session hears each of its turns, including the later turn
  that carries a review the builder asked for. A turn token may watch and deliver only within its
  reach. An on may not deliver in a circle (409). A second on for the same pair returns the first.
- **ask_agent and review_turn register that on themselves.** Each opens a session, registers an on
  over it for the asking session, then starts the turn. The result of any later turn of that session
  comes back too. You no longer need to register an on by hand for each review.
- **Registering an on is free; what it delivers counts.** Each turn an on delivers is a turn the
  on's maker asked for, counted under Turns per request and Turns per request tree. It stays at the
  maker's level under Depth, since depth counts asking, not hearing back: a builder at the default
  Depth of 2 can ask for a review, hear back, and ask again. So a chain of ons stops where a chain of asks does. One delivery stays free: your
  own turn hearing back in the session you started, so a coordinator's ons and results no longer
  use up its tree. A delivery past a cap is not made; `delivery.refused` in its session says why.

### Fixed
- **A shortened session keeps its standing rules.** When the history budget summarised a Claude
  Code session's first turn, the summary also replaced the context Claude Code puts in that
  message: `~/.claude/CLAUDE.md`, the memory index and the repo's `AGENTS.md`. The session then ran
  without them. Every step now keeps that context byte for byte at the front of the first message,
  as it already kept codex's context items and OpenCode's system message. A session that already
  lost it gets the fresh process's copy back on its next turn. `history.shortened` carries
  `head_tokens`, the size of what is never shortened. `tools/budgetcheck` shows per turn whether
  the context is still there.

### Removed
- **The on over one turn, and `delivery` on `POST /api/v0/turns`.** Both answer 410 with a message
  saying what to send instead: register an on over the session first, then start the turn. A
  command's result (`POST /api/v0/commands`) is still delivered by an on over that command, which
  control registers; `GET /api/v0/ons` lists it with `turn`, as before. An on over a session is
  listed with `filter` in place of `turn`.

### Changed
- **Restarting `agentbus up` no longer stops running turns.** `up`'s built-in runner is now a
  process of its own, started by `up`, as a runner on another machine is. Stopping `up` (Ctrl-C, or
  a restart to upgrade it) leaves the runner and its turns running. They finish and reach the
  record once `up` is back, after a few seconds or after minutes. A model call made while `up` is
  away waits for it, up to two minutes. Before, every running turn ended with exit 143,
  `runner_stopped`.
  - When `up` comes back from another build, or with another option for its runner
    (`-replay=false`), the runner of the earlier build finishes its running
    turns, takes no new ones, and then leaves. `up` then starts a runner of its own build. Turns
    asked for in between wait in the queue until then.
  - The runner leaves on its own when no agent of its runs and `up` is gone, so a stopped `up`
    leaves no process behind. A turn's end it could not send is kept in `finishes/`, and the next
    runner sends it when `up` is started again.
  - The runner keeps its own record in `runner/events.db`, a copy of `up`'s the first time, and its
    log in `logs/runner.log` (its own output in `logs/runner.out`). It shares the rest of `up`'s
    data folder: the folders, the session copies and its identity.
  - Its agents call the runner's own gateway, which relays to `up`. `up`'s own address no longer
    serves model paths (404), and `POST /api/v0/turns` no longer returns `base_url` on `up`, as on
    `server`. A turn's stand-in sent on another turn's path is refused by the runner's gateway
    (403) and recorded nowhere, on every runner now, not only `up`'s.
  - Control names a runner's turn's folder in its record from the turn's workspace, and gives a
    runner the saved agents' names with each turn's start, so a session that changes agent still
    names the earlier ones.

### Fixed
- **When a turn ended, and how, is decided once, by control.** A turn's end is its job's end on
  control: a finish control took, or an attempt it found lost. `GET /api/v0/turns` reads the turn's
  state from there once its job has ended, and a run's `run.ended` says only how its agent ended.
  Before, a turn whose agent finished but whose changes control never got read `finished`, with
  no changes to apply. This follows Buildkite and Temporal: the server records a job's end once
  and refuses a second; the agent learns it from the server's answer.
- **A turn that ended while control was away stayed "running"**, and a control restart longer
  than the 30-second lease ended every running turn with `runner_lost`. Control renews every
  running lease when it starts. A runner keeps no clock of its own over a turn: it stops one only
  when control answers that the turn is no longer its own. So a turn goes on through a restart of
  `up` or `server` of any length, and through a dropped link shorter than the lease.
- **A turn control found lost while its runner could not reach it no longer shares its copy.**
  The next turn of that workspace waits until the runner has heard and stopped the lost one's
  agent. In a copy everything waits for everything earlier, a lost turn or command still
  stopping among them, with one exception: a turn runs beside a command (`run_command`).
  A command, a discard, an update, a close or a put-away waits for all of it.
- **A turn's end is never dropped before control answered it.** A runner keeps it in
  `finishes/` until control took it or answered that the turn is no longer current. The turn's
  record goes before its finish: a runner that leaves while control does not answer the record
  keeps the end for the next runner, which sends both. A record control answers with a server
  error three times is given up, as Buildkite gives up a job's failed log chunks: the finish goes,
  and control closes the run whose end it did not get.
- **A turn never ends finished unless control holds its changes.** A reply cut after its
  headers, or no answer, is tried again as the finish is. If control keeps refusing the changes,
  or the copy cannot be read, the turn ends failed (`changes_not_kept`) and says so.
- **A turn taken just before a restart ran 30 seconds late.** Control now queues such a turn
  again as soon as it starts, since nothing was spent on it, and a runner takes it at once.
- **A finished turn whose last record never reached control blocked its session**
  ([#98](https://github.com/staysup/agentbus/issues/98)). Control now closes the turn's open run
  with the attempt, and the turn reads as its job ended. A run ends once: the record keeps the
  first `run.ended` of a run. Thanks to ayush585 for the report and the two tests.
- **A turn whose agent could not start left its reason only in the runner's own record.** Its
  `run.ended` now reaches control's record, as a started turn's does.
- **A runner stopped while control did not answer a turn's start never stopped.** It tried the
  start again without pause. It now drops the start, which control queues again when it is back.
- **A turn could edit up's built-in runner's record.** `runner/events.db` and `runner/build` are
  now among the data folder's files no turn may edit.
- **A file moved over another of the same size and time is no longer lost**
  ([#89](https://github.com/staysup/agentbus/issues/89), [#97](https://github.com/staysup/agentbus/pull/97)).
  A checkpoint trusts a file's size, time and mode only when its ctime is also older than the last
  look. The system sets ctime on every write, rename, link and chmod, and nothing can set it back.
  Before, `mv new.go old.go` in an overlay copy kept `old.go`'s old content in the record, and Apply
  removed `new.go` and said applied. An edit that put the time back (`touch -r`, `cp -p`,
  `rsync -t`) was missed the same way. A Copy workspace records its checkpoints through git, whose
  stat cache compares ctime to the second, so such an edit made in the same second as the copy
  was missed there too. It now holds git's cache to the same rule, and hashes again any file whose
  ctime or time is within 2 seconds of the copy's last record. Thanks to `kunalgoesbyken` (#97).
- **Apply no longer writes over a file the person moved in**. The same rule now holds in your
  folder. Where a copy's Base recorded a file by size, time and mode only (an overlay copy), Apply
  took a file still showing them as untouched and wrote over it. So when you moved another file of
  the same size and time over it during the session, its content was lost. Apply now trusts that
  only when the file's ctime is older than when the copy began reading your folder. Otherwise it
  reads the file, and refuses the apply as changed when it cannot show the file is the Base's. The
  same holds when a copy reads a file's Base content from your folder. A changeset now records that
  time (`base_taken`); one made before 0.6.8 has none, so such a file refuses rather than being
  trusted.
- **Edits to committed build output reach Review and Apply**
  ([#87](https://github.com/staysup/agentbus/issues/87)). Before, an edit to a committed file
  under `dist/`, `build/`, `target/` or another built-in folder, or to a file added with
  `git add -f` past your `.gitignore`, never reached Review or Apply, and Apply said applied. A
  Copy workspace now asks git at every checkpoint, and on your side of every Update, which files
  are the work, of the repo each file is in: your folder's own, each nested repo and each
  submodule, each found the way git finds it. A file that repo tracks is always recorded, so one
  removed in a turn and made again in a later one is recorded again rather than applied as removed.
  What else is left out is that repo's own rules only: its `.gitignore` files, its
  `.git/info/exclude` and its `core.excludesFile` (a relative one read from its work tree, as git
  does), else yours. The built-in list (`node_modules/`, `dist/`, `build/`, `target/` and the rest)
  still applies at your folder's top, but git applies it, less any built-in folder git finds a
  tracked file in. So a new file beside a committed one reaches Apply in `dist/` and in `Dist/`
  alike, a `node_modules/` your repo does not ignore stays out, and a parent repo's rules no longer
  reach into a nested repo, as `git status` there does not read them. A folder that holds its own `.git` is that repo's
  to decide, even where a rule of the repo above leaves the folder out; a repo the agent makes in
  the copy is asked as it is now. Submodules opened as the
  folder, worktrees, split indexes and SHA-256 repos are read too; an index git cannot read fails
  the checkpoint instead of dropping its files. A tracked file is found under the name your folder
  lists it by, so a case rename of a force-added `config.local` to `Config.local` is a rename, and a
  name your disk keeps in another Unicode form than git's index is found too. Only another spelling
  of the same name stands in for it, never a hard link under another name. This holds for
  copies made from 0.6.8 on; start a new session for an older one.
- **A case-only rename reaches the folder on a disk that ignores case**
  ([#76](https://github.com/staysup/agentbus/issues/76)). On such a disk (macOS by default, ext4
  with casefold) a look-up of `button.tsx` finds `Button.tsx`, so a checkpoint took the old name
  as still there. Renaming `button.tsx` to `Button.tsx`, or `comp/` to `Comp/`, left Review empty,
  and Apply said applied with the folder unchanged. A name now counts only as its folder lists
  it. The rename shows in Review, applies, is put back by a Discard, and comes in with an Update
  when the person makes it. This holds for Copy workspaces made since 0.6.6.
- **A hard-linked file no longer takes extra disk space in a Copy workspace**
  ([#93](https://github.com/staysup/agentbus/issues/93)). It was copied once
  per name (a 32 MB file with 8 names took 257 MB); now it is copied once.

## 0.6.7 (2026-11-27)

### Added
- **An agent hands a long command to AgentBus, and its turn ends.** On Claude Code 2.1.332 a
  background command keeps `claude -p` running until the command ends, so a 10 to 15 minute gate
  held the turn, and the person's messages waited behind it. Coordinators ran gates with `nohup`
  instead, which nothing recorded and nothing reported. Now an agent calls the `run_command` tool
  (or `POST /api/v0/commands`) and ends its turn. The command is a job: it runs in the turn's
  direct workspace (the folder itself), on the runner that holds it, under
  the turn's command rules, once the turn has ended. While it runs, a turn the person sends does
  not wait for it. When it ends, a new turn in the session carries the command, how it ended, its
  exit code and the end of its output (16 KiB). `command.ended` records it, its delivery is an on
  (`GET /api/v0/ons`, event `command.ended`), and `GET /api/v0/commands/{id}` and
  `POST /api/v0/commands/{id}/stop` read and stop it. A command the rules would hold for a person
  is refused, since no one is there to answer. It counts as work the turn asked for, under the
  limits. A command never gets more than the turn that asked for it may do: AgentBus has no wall
  of its own for a command yet, so it is refused, saying why, when the turn is limited in a way the
  command would escape (edits forbidden or read-only, or its harness's own sandbox: Claude Code in
  a sandbox copy, Codex always). The runner decides it once, at the turn's launch, from the limits
  the harness starts with (`harness.CommandLimit`), and records it on the turn's `turn.started`
  (`commands_run`, `command_limit`), which a runner sends to control before the turn's agent
  starts, so control never reads ahead of it; control refuses by that record, and a turn whose
  runner records no limit (an agentbus from before this) takes no command. A run control saw
  start always ends there: when a turn's attempt ends, control closes any run of it whose end the
  runner did not send (its agent failed to start, or the link lost the end), so the session's
  next turn starts.
- **AgentBus keeps each turn's history within a budget, by itself.** Own replay used to put the
  whole session in front of every turn, so a long session grew until its harness compacted. Now,
  at each turn's start, the history it builds fits a budget: 40% of the model's window by default,
  well below any harness's compaction point. Under the budget nothing changes. Over it, older turns
  are shortened in one step down to half the budget, oldest first: long tool results (over 2 KiB)
  become a stub that says what it was, how it began and the API call that fetches it back; then
  long tool-call inputs the same way; then the oldest turns become AgentBus's own summary (the
  person's words and the model's last words, each cut past 2,000 bytes, AgentBus's notes, a line
  per tool call). When the summary itself grows too big, a step leaves the oldest turns out of it
  for one line naming how many and their range, so a session of any length stays within the
  budget. The three newest turns go in whole while that is enough. When even that does not fit,
  the newest turn is shortened too: the budget wins, and every turn starts within it. Images are
  counted by their block (about 1,600 tokens each), never guessed from long text. Each step is frozen: the next turns carry its bytes unchanged, so the prompt cache
  keeps working, and a turn's log, once written, stays word for word until it is left out. No model
  is called for it. The record keeps every byte, and the turn's stored requests are what the model saw.
  `history.shortened` records each step. The budget is `budget` in
  `GET/PUT /api/v0/settings/compaction`; control sends it to every runner with its settings, so a
  runner on another machine keeps the same budget. Claude Code, Codex and OpenCode alike.
- **A workspace's last action.** `GET /api/v0/workspaces/{id}` (and the list) adds `last_action`:
  the newest outcome of an apply, discard, update or close, as the record holds it: a
  quick look at what happened last. `GET /api/v0/workspaces/{id}/outcome?event=` reads one
  action's outcome by the event its reply named (404 until it has run): the read a caller polls,
  since a later action replaces `last_action`.
- **`workspace.failed`.** A discard, update or close that ended without doing its work records it:
  the action, why, and who asked. The runner refused it, its runner stopped answering while it
  ran, or its runner was removed before it took it. Before, such a job recorded nothing, and only
  a caller still waiting on the route heard why. It takes the id the action's success event would
  have had, so one request has one outcome. A refused apply is still `workspace.applied` with
  result `refused`.

### Changed
- **Turns you send start at once.** A runner used to run 4 turns at once, whoever started them, so
  builders a coordinator started could fill every place and hold your own messages in the queue
  (10+ minutes on 2026-11-26). Now your turns have a lane of their own and are not counted. They
  wait only for a turn already running in their workspace (one copy runs one turn) and their
  session's earlier turns. Within a workspace your turn goes ahead of the turns agents asked for
  that have not started yet, such as `ask_agent` builders queued in the coordinator's workspace; no
  job passes a discard, update or close. The limit holds only for delegated work, the turns
  agents start (in any session, the person's own included: who asked decides it) and the replies
  delivered into sessions agents started. It is 8 by default, set by
  `[limits] max_delegated_at_once` in the settings (1 to 64), and a change holds for the next turn
  a runner takes, with no restart. `GET /api/v0/runners` says how it stands: `delegated` with
  `at_once`, `busy` and `waiting`, and each runner's own (its `waiting` counts the turns bound to
  it by its copy or its machine). Control counts the places; a runner runs
  every job it takes, and the engine no longer refuses a fifth turn with 429.
- **`agentbus up -max-runs` is retired.** It is ignored, with a note saying where the limit is now.
  `agentbus runner -capacity N` stays, as a machine's own ceiling on delegated turns; without it
  the settings decide.
- **Runner protocol 6.** A take names its lane (`folders`, `delegated` or `interactive`) in place
  of `folders`. A runner and control of different releases refuse each other with a plain message:
  upgrade both.
- **A harness that compacts inside a turn is let through.** Own replay used to refuse a main
  request the harness had compacted, which stopped the turn. Now, once a request of the turn has
  carried the history, a compacted one goes on as the harness sent it (Codex's under the session's
  thread id, as the turn's other requests). The gateway judges every main request once, by the
  layout test it forwards by, in every turn, and records the verdict on the call as
  `history.compacted`. That one test is the only place a compaction is decided: in a turn that
  carries a history the recorded verdict says, and in a turn with none own replay applies the same
  test to the stored request, which is the agent's own. Nothing depends on the harness's summary: the next turn is built from the
  record, from the turn's last request before the compaction, then a note that the turn went on,
  how many calls followed and the model's last words. A request not in the fresh layout before any
  request carried the history is still refused.
- **A Copy workspace puts itself away.** Once its work is all applied or discarded and the
  workspace has been quiet for 30 minutes, AgentBus removes the copy itself, as Close does.
  Sessions and the record stay, and the next turn makes a fresh copy. A copy with unapplied work
  is never touched. If anything happened in the workspace since (a turn, an apply, a discard, an
  update, a preview, a close), or work is waiting in it, the put-away does nothing. The record says
  which: `workspace.closed` with `put_away` (the on and event that queued it, when it was queued
  and when it came due), or `workspace.kept` with why. The quiet time is a setting,
  `[workspaces] put_away_after_minutes` (30 by default, 0 is off). Turning it off also stops a
  put-away already waiting. When several wait at once, the one for the latest event decides.
- **A job may wait until a time.** A job can carry a time before which no runner takes it
  (`not_before`). Until then it holds no place in its workspace's lane, so a turn sent meanwhile
  runs at once. The put-away is the first job that uses it.
- **An on may watch the record and queue a job.** Besides "a turn completed, deliver its reply",
  an on can now watch a kind of event on the record and queue a job in that event's workspace. It
  keeps a cursor in the record and fires once per event. The put-away is control's first built-in
  on, over `workspace.applied` and `workspace.discarded`. `POST /api/v0/ons` still registers only
  a turn's on.
- **List the ons.** `GET /api/v0/ons` lists the ons still waiting to react, oldest first: each
  one's id, the turn it watches, the session it delivers to and the turn that registered it.
  `session` keeps the ons that deliver to that session. The main token sees every on; a turn
  token sees the ons it may remove: those its own turn registered and those that deliver to a
  session it may read. Finding them used to take a read of `jobs.db`.
  The ons a turn's ask records with it (`delivery` on `POST /api/v0/turns`, as ask_agent does)
  are listed too. One that fired or was removed drops out.
- **Every answer of a turn.** `GET /api/v0/turns/{id}` adds `replies`: each answer the turn gave,
  oldest first, with its text and time. A turn can answer more than once, as when Claude Code
  resumes after its own background command ends; `reply` still holds the last. Before, the
  earlier answers showed only in the turn's events.
- **Apply, discard, update and close answer at once.** Each queues its job and answers 202 with
  state `queued` and `event`: the id the record gives its outcome. None waits up to 30 seconds for
  the job to run any more. Poll
  `GET /api/v0/workspaces/{id}/outcome?event=` with it until it answers 200: `workspace.applied` (result
  applied, refused with each file in `conflicts` and why, or failed), `workspace.discarded` (with
  `dropped_turns`), `workspace.updated` (updated, merged or conflict, with the files),
  `workspace.closed`, or `workspace.failed`. Asking for the same apply while it waits or runs
  makes no second one: it answers 202 with the same event. Nothing to do still answers 200 with
  `already` true, and a refusal before anything is queued (another action waiting, a closed
  copy, an offline machine) keeps its status. **Breaking:** a refused apply is no longer a 409
  with its files; the files are on the outcome. Discard's reply has no `dropped_turns` and
  Update's has no `updated`, `conflicts`, `unresolved` or `unmergeable`: those are on the outcome
  too. `GET /api/v0/turns/{id}/diff`'s `apply` names its outcome's `event`.
- **A Claude Code Copy session reaches GitHub and the package hosts, and writes your caches.**
  Copy limits where an agent writes, not what it reads. `gh` and GitHub, the npm, PyPI, Go, Yarn
  and crates.io hosts are on the sandbox's list, Go can check TLS, and your cache folders are
  writable as the README promised. The copy allows no local sockets (`allowAllUnixSockets` is
  false), so Docker, whose bind mounts would write outside the copy, does not work there: use a
  Direct session. Socket grants in your own managed, MDM or user Claude Code settings still apply
  in a Copy session; 0.6.8 closes that.
- **A malformed `session` query is 400 for every caller.** On `GET /api/v0/events` and
  `GET /api/v0/turns`, a `session` sent twice, empty or not shaped as a session id is refused
  with 400, the owner's token included. A turn token sending it twice got 403 before.

### Fixed
- **A file changed since the last checkpoint is read again, even with its old size and time**
  ([#89](https://github.com/staysup/agentbus/issues/89)). A rename over a file of the same size
  and time, or `touch -r`, made a checkpoint think it unchanged, and Apply said applied without it.
  A file now also needs a ctime older than the last look. Thanks to Kunal Roy Choudhury
  (`kunalgoesbyken`, #97).
- **A session whose newest turn's history cannot be rebuilt goes on by transcript**
  ([#100](https://github.com/staysup/agentbus/issues/100)). If `up` and its runner both died
  mid-turn, every later turn of the session was refused with 409. Thanks to Kunal Roy Choudhury
  (`kunalgoesbyken`, #101).
- A test now proves Apply refuses a truncated changeset and writes nothing. Thanks to Rodrigo
  Boratto (`rwrz`, #109).
- **The owner's `GET /api/v0/events?session=` opened a stream of no session** instead of
  refusing. The door checked the query only for a token that does not reach everywhere.
- **A reply the main token delivered into a session another tree had continued joined that
  tree.** A session's tree is now its first turn's: a session the person started has none, even
  after another tree's turn delivered into it.
- **A compaction from before 0.6.7 no longer leaks the harness's summary into the next turn.** In a
  turn with no earlier history, whose stored requests are the agent's own, own replay now applies
  the gateway's own layout test to the stored request. So a record written before
  `history.compacted` existed, or a call the gateway could not judge after a restart, is rebuilt
  from before the compaction. Before, such a session failed to continue (before 0.6.7) or carried
  the summary (0.6.7).
- A history step that changed nothing is no longer recorded as `history.shortened`; the
  compaction note says "1 more model call".
- **A retry of a turn that had ended answered `running`.** `POST /api/v0/turns` sent again with the
  key of a turn that already ended now answers how it ended (`finished`, `failed` or `stopped`), as
  `GET /api/v0/turns/{id}` does, with no `base_url`.
- **An on over a turn with no job answered "no such turn".** `POST /api/v0/ons` for a turn you may
  read but that no job of this service ran (an older turn, or one recorded at the gateway) now
  answers 409 and says why: nothing reports that turn's end, so no on could react to it. A turn you
  may not read still gets what any such turn gets.
- **An OpenRouter call on your own provider key (BYOK) was recorded as $0 and complete**
  ([#94](https://github.com/staysup/agentbus/issues/94)). On a BYOK call OpenRouter's `usage.cost`
  holds only its own fee, often 0, and the provider's charge is in
  `cost_details.upstream_inference_cost`. The call's cost is now both, in all three formats (Chat
  Completions, Responses and Anthropic Messages). A BYOK reply without the provider's charge
  records the fee as an incomplete cost, with `usage.cost_partial`, so a tree's cost limit no longer
  reads the call as free and complete. `is_byok` is no longer reported as an unexpected field.
- **A close that failed part way could make an Apply delete your files**
  ([#96](https://github.com/staysup/agentbus/issues/96)). When Close could not remove one file of a
  Copy workspace's copy (a file you locked in Finder was enough, since the copy keeps the lock), it
  had already removed the others. The workspace stayed open, the next turn ran in what was left, its
  changes listed your files as deleted, and Apply deleted them. Since a settled copy now puts
  itself away, this could happen with no one pressing Close. Now a copy exists only while its
  record does: the record is written last when a copy is made and taken away first when one is
  removed, so a turn never runs in a half-made or half-removed copy, and nothing is applied from
  one. Close clears locks and closed modes on the copy's own files and links (never on yours), so a
  command in the copy cannot make it unremovable. A close that cannot move the copy out of its place puts the
  record back: the copy stays whole, the workspace stays open (no `workspace.closed`), and a close
  sent again tries again. What a close moved aside but could not remove is removed when the runner
  starts again, and a copy a stopped Open left with no record is removed by the next close. This
  holds for the copy, overlay and git backends: a git worktree is moved aside the same way, then git
  forgets it, and one a stopped `git worktree add` left locked is unlocked and removed by the next
  turn or close.
- **A stop now stops a copy that is being made** ([#95](https://github.com/staysup/agentbus/issues/95)).
  While a Copy workspace's first turn made its copy, a stop of the turn answered 202 but the turn
  kept running, and Ctrl-C to `agentbus up` (or a stopping `agentbus runner`) did not exit until the
  copy was done. Now every step of making a copy stops: the copy within one file or one 32 MiB
  piece of a big file, the hashes of its first checkpoint within one 32 MiB piece, a git worktree's
  `git worktree add` and its carried edits, and an overlay's snapshot. A copy whose stop came at any
  step, its last included, never writes its record, so its turn never starts. A stop fails a
  checkpoint; it never leaves a file out as if it could not be read. A stopped turn reads
  stopped; a runner that stops fails the turn with `runner_stopped`. A copy made in one clonefile
  call on macOS is still quick and is not stopped part way.

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
