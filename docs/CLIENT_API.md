# The client API

Status: **v0**, as built in AgentBus 0.2. Paths under `/api/v0` may still change before 1.0.
What 0.2 changed for a 0.1 client is listed under [Versions](#versions).

**The service describes this API itself**, without the token and without internet: an interactive
reference at `/docs`, the OpenAPI 3.1 description at `/api/v0/openapi.json` (generated from the
route table and the Go types when the service starts, `api/routes.go` and `api/spec.go`), and one plain file for coding agents at `/docs/llms.txt`
(`docs/llms.txt`). `/docs` and everything under it is answered by the service, like `/api`, and never
forwarded to a vendor. There are no client SDKs: generate one from the description.

A cockpit is any program that uses this API: a browser app, a terminal app, a script. The framework
defines how a cockpit connects and never serves a screen. A cockpit hosted by someone else later
uses the same API.

## Where it listens

The client API shares the gateway's address: `agentbus up` starts one service on
`127.0.0.1:8787`. `-listen` takes another IP and port, such as `0.0.0.0:8787` in a container.

One program runs three ways. The API is the same in each; what is behind it differs:

| Command | Behind the API | Jobs |
|---|---|---|
| `agentbus up` | the job queue, the record, the gateway and a built-in runner | made and run here |
| `agentbus server` | the job queue, the record and the gateway | made here, run by runners on other machines |
| `agentbus runner -server <url>` | its own record and gateway, for its agents only | taken from the server; this API has no job routes (404) |

A runner's turn is recorded by the runner's own service. The server gets the job's result: how the
turn ended, its reply, its cost and the files it changed. Sending the runner's record to the server
comes later.

| Path | Who calls it | Sign-in |
|---|---|---|
| `/s/<session>/v1/...` and `/v1/...` | the agents | the vendor's, passed through untouched |
| `/api/...` | cockpits and programs | **our token** |
| `/docs/...` | people and coding agents | none |
| `/` | a browser | none: `GET` and `HEAD` redirect (302) to `/docs` |

- **A program is a cockpit that also acts**: it starts turns (`POST /api/v0/turns`) and writes its
  own events (`POST /api/v0/events`), with the same token.
- **The whole `/api` namespace is ours.** The paths `/api` and `/s/<session>/api`, and every path
  under either, are answered by the service and never forwarded to a vendor. An unknown API path
  gets 404 locally.
  Without this, a request for a version that does not exist, such as `/api/v1/events`, would reach
  the vendor with the token in its headers.
- **A path that is not plain is refused with 400 before any routing**, for every request, agents'
  included: a `.` or `..` segment, an empty segment, a percent-encoded dot, slash or backslash, or a
  request line not in origin form. No agent or cockpit sends such paths; refusing them is what keeps
  the namespace rule above a simple prefix check. This 400 carries no CORS header.
- **Loopback by default.** `-listen` must be an IP address, not a name. Of the loopback addresses
  only `127.0.0.1` and `[::1]` are accepted.
- **On loopback, a request whose `Host` is not `127.0.0.1:<port>`, `localhost:<port>` or
  `[::1]:<port>` is refused** with 403. That stops a web page on another site from reaching the API
  through DNS rebinding.
- **On any other address, such as `0.0.0.0`, every `Host` is accepted.** Other machines can reach
  the service there, and send any `Host` they like, so the check would add nothing. The token on
  every API request is the guard. There is no HTTPS: use an SSH tunnel or a private network.
- **The agents' paths answer this machine only** when the service is not on loopback: a request to
  a vendor path (`/v1/...` or `/backend-api/codex/...`, with or without `/s/<session>` or
  `/s/<session>/t/<turn>` in front) from another machine gets 403, before anything is forwarded.
  Any other path outside `/api` and `/docs` gets 404 from every caller, as on loopback. The agents run on the service's machine, and
  `AGENTBUS_URL` and the vendor base URL a turn gets are `http://127.0.0.1:<port>` on `0.0.0.0`.
  "This machine" means the caller's address: loopback, or the address the request arrived on. A
  reverse proxy on this machine makes every caller look local, so a proxy in front of the service
  should pass on `/api` and `/docs` only.
- **Any browser origin may call the API by default.** CORS mainly guards cookies, and AgentBus has
  none: every API call needs the token in the `Authorization` header, which a page from another
  origin does not have. So with no `-origin`:
  - an `OPTIONS` preflight to an `/api/` path from any origin is answered `204` **without a token**,
    with `Access-Control-Allow-Origin: *`, `Access-Control-Allow-Methods: GET, POST`,
    `Access-Control-Allow-Headers: Authorization, Content-Type, If-Match` (`If-Match` for a settings
    `PUT`; a settings reply exposes `ETag`). A preflight carries no token, so
    requiring one would stop every browser request before it starts. `Authorization` is named
    because `*` in `Allow-Headers` does not cover it.
  - the real request still needs the token. A missing or wrong one gets 401.
  - **every response carries `Access-Control-Allow-Origin: *`**, errors included (401, 404, 409),
    and streams too. There is no `Access-Control-Allow-Credentials`: a browser client sends no
    cookies, and does not need to.
  - `*`, not the request's own `Origin` echoed back: the answer is the same for every origin, so
    it needs no `Vary` and caches cleanly.
- **`-origin` is an optional lock.** `agentbus up -origin http://localhost:5173` allows that one
  origin only:
  - its preflight to an `/api/` path is answered as above, with `Access-Control-Allow-Origin:
    <origin>` and `Vary: Origin`.
  - every response to that origin carries `Access-Control-Allow-Origin: <origin>`, errors and
    streams included. Without it the browser hides the response even after a successful preflight.
  - a preflight from any other origin gets 403, and responses to it carry no
    `Access-Control-Allow-Origin`.
- A preflight to a path outside `/api/` gets 403 in both modes.
- **The model gateway paths** (`/v1`, `/backend-api/codex`) send no CORS headers. Only agents on
  this machine call them, never a browser.

## The token

- `agentbus up` creates a random token on first start and stores it in the data folder (below) as `token`, with
  mode 0600. It stays the same across restarts until it is deleted.
- Every `/api/` request except an allowed preflight sends `Authorization: Bearer <token>`. A missing
  or wrong token gets 401.
- The token never appears in a URL: URLs land in logs and browser history. A browser cockpit reads
  the stream with `fetch`, which can send the header, not with `EventSource`, which cannot.
- The token is never written to an event, a log, or an error.

### The turns token: start, stop and read turns, nothing else

A program that only starts and follows turns should not hold the main token. It uses the
**turns token** instead. The runners of the job queue do not use it: they take the enrollment
token and their own runner tokens ([The runner protocol](#the-runner-protocol)).

- `agentbus up` makes it on first start, beside `token`: the file `turns_token` in the data folder,
  mode 0600, `abq_` and 64 hex characters. It stays the same across restarts.
- To replace it, stop the service, delete the file and start again. The old one then gets 401.
- It works on these operations only: `POST /api/v0/turns`, `GET /api/v0/turns`,
  `GET /api/v0/turns/<id>`, `GET /api/v0/turns/<id>/events` and `POST /api/v0/turns/<id>/stop`.
  It sees every session's turns, as the main token does.
- Everything else gets **403** with `the turns token may only start, stop and read turns; this
  route takes the main token`.
- Like the main token, it never reaches a turn's child and is never written to an event, a log
  or an error.

### The turn token: what a turn may read, and how it asks other agents

Every turn started by `POST /api/v0/turns` gets two variables in its child's environment, for claude
and codex alike:

- `AGENTBUS_URL`: the service's own address, such as `http://127.0.0.1:8787`. On `0.0.0.0` it is
  still `http://127.0.0.1:<port>`.
- `AGENTBUS_TOKEN`: a token for **this turn only**, `abt_` and 64 hex characters (256 random bits).

The main token never reaches a child. An `AGENTBUS_URL` or `AGENTBUS_TOKEN` in the service's own
environment is replaced.

**A turn token works inside its request tree**, by default. `[agent_api]` in
[permissions.toml](#permissionstoml) can narrow it to the turn's own session, and turn each kind of
call below off. The tree is the person's session (its root) and
every session that work asked for from it started, at any depth. With the token an agent reads the
tree, and asks other agents: it starts turns on any saved agent, continues the sessions it started,
asks for reviews, and reads their results. A turn never waits for that work. **Each result comes back
to the asking session as a later turn** (see "Results come back as the next turn" below).

| Operation | Limit |
|---|---|
| `GET /api/v0/turns?session=<id>`, `GET /api/v0/turns/<id>`, `GET /api/v0/turns/<id>/events` | a session in the tree; without `session`, its own |
| `GET /api/v0/sessions/<id>/events`, `GET /api/v0/sessions/<id>/search` | a session in the tree |
| `GET /api/v0/calls/<exchange>/context`, `.../pieces/<n>` | a call of a session in the tree |
| `GET /api/v0/jobs?session=<id>`, `GET /api/v0/jobs/<id>`, `GET /api/v0/attempts/<id>/events` | a job of a session in the tree; without `session`, its own |
| `GET /api/v0/agents`, `GET /api/v0/agents/<id>`, `GET /api/v0/harnesses` | none |
| `POST /api/v0/turns`, new session | `agent` and `prompt`; `folder` is the turn's own folder or one inside it (the person's folder when absent); always isolated, so `isolate: false` gets 403 |
| `POST /api/v0/turns` with `session` | a session the tree started; never its own, never the person's (the root) |
| `POST /api/v0/jobs/<id>/review` | a job of a session in the tree |
| `POST /api/v0/jobs/<id>/cancel`, `POST /api/v0/turns/<id>/stop` | only work the tree asked for |
| `PATCH /api/v0/sessions/<id>` (rename) | a session in the tree |
| `GET /api/v0/turns/<id>/diff` | a turn of a session in the tree; without `session`, its own |
| `POST /api/v0/turns/<id>/apply` | a turn whose job ran in a session in the tree, its own session included |

The writes are refused for a **read-only** turn (`read_only: true`): its token only reads.

Each row is also a switch of `[agent_api]`, read for every request: `read` (the reads of the first
five rows, and the diff), `list_agents`, `start` (a new session), `continue` (with `session`), `review`,
`cancel`, `rename` and `apply`. Each is on unless the file says `false`; a call switched off gets **403** naming the
switch. `scope = "session"` keeps the token to its own session: it reads only that session's turns,
calls and jobs, reviews only its jobs, cancels only what its own turns asked for, renames only its
own session, applies only its own session's turns, and continues no session. A `permissions.toml` that cannot be used refuses every turn token's request with **503**.

**What a turn token never does**, each a **403** with a plain reason, whatever `[agent_api]` says:
read or change the settings (every `/api/v0/settings` route takes the main token), save, edit or remove agents, see or make a token (no reply holds one), make a
job with `POST /api/v0/jobs` (it starts work with `POST /api/v0/turns`), close a workspace, write a
program event, read the snapshot `GET /api/v0/sessions`, the stream `GET /api/v0/events` or
`GET /api/v0/workspaces`, read `GET /api/v0/version`, and read or write any session outside its
tree. The description and the docs need no token at all.

**The caps on asked work** are `[limits]` in [permissions.toml](#permissionstoml), read for every
request, so a change applies to the next one. They keep a chain of agents from running away. Past
one, the request gets **403** saying which and naming its key:

- **`max_depth`, default 2.** Work the person's turn asks for is at depth 1, work that work asks for
  at depth 2, and nothing deeper. A delivered result has the depth of the work it carries, so a
  result of depth-1 work may ask once more, and a result of depth-2 work may not.
- **`max_jobs_per_turn`, default 10**: turns and reviews one turn asks for, ever.
- **`max_jobs_per_tree`, default 30**: everything asked for from one person's session, at every
  depth.
- **`max_cost_usd_per_tree`, default 0 (off)**: once the model calls of every session in the tree
  have cost this many US dollars in all (`cost_usd`, from the built-in price table), the tree asks
  for nothing more. Work already running finishes, and its results are still delivered.

0 for one of the first three refuses all asked work of that kind.

**Asking works where the service runs its turns itself (`agentbus up`).** With `agentbus server`
and a separate `agentbus runner`, a turn's token reaches the runner's own address, which keeps no
queue and no saved agents, so asking answers 404 there for now.

**It stops working when the turn's child process exits**, even if a process it left behind still
holds its output open, and before the turn's `session.closed` is
stored; after that it gets 401. The service keeps turn tokens in memory only, under their SHA-256,
so a restart leaves no token working. At most one token per running turn exists at once.

From inside a turn, with plain `curl`:

```sh
curl -s -H "Authorization: Bearer $AGENTBUS_TOKEN" "$AGENTBUS_URL/api/v0/turns"
curl -s -H "Authorization: Bearer $AGENTBUS_TOKEN" "$AGENTBUS_URL/api/v0/sessions/<session>/search?q=login"
curl -s -H "Authorization: Bearer $AGENTBUS_TOKEN" "$AGENTBUS_URL/api/v0/agents"
curl -s -X POST -H "Authorization: Bearer $AGENTBUS_TOKEN" -H "Content-Type: application/json" \
  -d '{"agent":"deepseek","prompt":"Add a test for parse()"}' "$AGENTBUS_URL/api/v0/turns"
```

The session id is in the turn's own base URL, `<service>/s/<session>/t/<turn>`, which the agent
already has (`ANTHROPIC_BASE_URL` for claude). `GET /api/v0/turns` without `session` also names it.

### Results come back as the next turn

When a turn or review a turn asked for ends (finished, failed or cancelled), AgentBus starts the
next turn of the **asking session**, on that session's own agent, after the turn running there now.
Its prompt is the result, and it starts `[AgentBus] This message is from AgentBus, not from the
person`. It names the agent, the job and its state, the session and turn the work ran in, its folder
(each new session gets its own isolated copy, so results of a fan-out can be compared), the changed files
(count and paths, at most 50), a review's verdict and findings, and the work's final reply (at most
8 KiB). Each result is its own turn; three asked turns bring three.

Such a turn's `turn.started`, and its row in `GET /api/v0/turns`, carry `origin`:
`{"kind":"result","job":…,"job_kind":"turn"|"review","session":…,"turn":…,"agent":…}`. A turn
another turn asked for carries `{"kind":"asked","session":…,"turn":…}`, naming who asked. A turn the
person started has no `origin`. The service checks for ended work every second. On the job,
`links.asker`, `links.chain` and `links.result_of` say the same.

## The data folder

`$AGENTBUS_HOME` if set, otherwise `$XDG_DATA_HOME/agentbus`, otherwise `~/.local/share/agentbus`.
It holds `events.db` (the store: events, and since this version every model request's body, see
`GET /api/v0/calls/<exchange>/context`), `jobs.db` (the job queue: jobs, attempts, runners and
workspaces, in a file of its own so the record knows nothing about jobs), `token`, `turns_token`
(above), `enroll_token` (adds a runner, [The runner protocol](#the-runner-protocol)),
`runner.json` (the built-in runner's id and token, mode 0600), `worktrees/` (each isolated
session's copy or git worktree, and in `worktrees/.isolation/` its checkpoints), `permissions.toml` (below), `instructions.md` (below), `agents.json`
(the saved agents), `bin/agentbus-api` (below), `logs/` (below), and `events.db.lock`,
which one service holds while it runs, so a second `agentbus up` on the same folder refuses to
start. No version in the name.

### permissions.toml

What a turn may do without a person to ask, and what its token may ask for. A turn has no one to
click "allow", so without this file a claude turn is refused anything its settings would ask about,
such as running `go test`.

```toml
read_folders = ["~/.agents", "~/Sites/agentbus-notes"]
allow = ["go test", "make test", "git commit"]
forbid = ["git push", "git checkout"]
edit = "session"
subagents = true

[limits]
max_depth = 2
max_jobs_per_turn = 10
max_jobs_per_tree = 30
max_cost_usd_per_tree = 0.0

[agent_api]
scope = "tree"
read = true
list_agents = true
start = true
continue = true
review = true
cancel = true
rename = true
apply = true
```

- `agentbus up` makes the file when it is missing, mode 0600, with empty lists, every other value
  written out at its default, and a comment on each. It never writes an existing file.
- **The API reads and writes it too**: `GET` and `PUT /api/v0/settings/permissions`
  ([Settings](#settings)). Edit the file by hand or through the API, whichever you like.
- **A runner on another machine reads the server's policy**, never its own file
  ([The runner protocol](#the-runner-protocol)).
- **Every turn reads it when it starts, and the API reads it for every request a turn token
  sends.** An edit applies to the next turn and the next request. No restart.
- A key left out reads as its default, so an older file with the three lists only keeps working.
  A missing file is every default.
- **A broken file stops the turn.** The answer is 400, it names `permissions.toml` and the problem,
  with the line when there is one, and nothing is stored. A turn never runs without the policy.
  A file that cannot be read, or a link to a file that is gone, stops the turn the same way. An
  unknown key or a value out of range is broken too.
- `read_folders`: absolute paths, or starting with `~/`. Each must be an existing folder on the
  machine that runs the turn, where `~` is that machine's home. No comma, parenthesis or glob
  character (`*?[]{}`).
- `allow` and `forbid`: a command and its first arguments, such as `go test`. No comma,
  parenthesis or star, no two spaces in a row, no leading `-`.
- `isolation`: where a new session's turns run. `"auto"` (the default) is the fastest copy the
  machine can make: `"copy"` on macOS, `"overlay"` on Linux where the runner can mount one, else
  `"copy"`. `"overlay"` is an overlay mount on Linux, `"copy"` a copy of its folder, `"git"` a git
  worktree in a repo and a copy elsewhere, `"none"` the folder itself. See [Isolation](#isolation).
- `share_read_only`: folder names at the top of a session's folder, such as `"node_modules"`, that
  a copy links to one shared read-only copy of instead of copying. One name each: no `/`, not
  `.git`. Empty by default; a turn cannot write in them. See [Isolation](#isolation).
- `edit`: where a turn may create and change files without asking. `"session"` (the default) is
  the turn's own session folder: its isolated copy, or the folder itself when the session is not
  isolated. `"none"` is nowhere. A read-only turn never edits, and `forbid` still wins.
- **No turn edits the data folder's own files**: `permissions.toml`, `instructions.md`,
  `agents.json`, `token`, `turns_token`, `enroll_token`, `events.db`, `jobs.db`, `runner.json` and
  `bin/agentbus-api`. claude gets a deny rule on each, which wins over `edit` and over the person's
  own settings. codex has no way to keep one file of its folder out of its sandbox, so a codex
  turn whose folder holds the data folder runs read-only, and the service log says
  `run.edit … left_out=data_folder_inside`. This covers the tools that edit files. A command in
  `allow` that runs any code, such as `python3`, `node` or `sh`, can still write these files: allow
  only commands you would let change your policy.
- `subagents`: `true` (the default) or `false`. `false` turns off the agent's own subagents for
  every turn, on the command line only (the table below). The person's `~/.claude/settings.json`
  and `~/.codex/config.toml` are never edited.
- `[limits]`: the caps on work agents ask for (above, "The caps on asked work"). Whole numbers
  from 0 for the first three; dollars from 0 for the cost, where 0 is off.
- `[agent_api]`: what a turn's token may call (above, "Turn tokens"). `scope` is `"tree"` or
  `"session"`; the others are `true` or `false`.

What each list becomes:

| List | claude | codex |
|---|---|---|
| `read_folders` | `--allowedTools Read(//<folder>/**),Grep(//<folder>/**),Glob(//<folder>/**),…` | nothing: codex's sandbox reads every folder already |
| `allow` | `--allowedTools Bash(<cmd>:*),…` | nothing yet |
| `forbid` | the gateway's guard, and `--disallowedTools Bash(<cmd>:*),…` | the gateway's guard |
| `edit = "session"` | `--allowedTools Edit(//<folder>/**)`, and `--disallowedTools Edit(//<file>),…` for the data folder's files | `--sandbox workspace-write -c sandbox_workspace_write.network_access=true` |
| `subagents = false` | `--disallowedTools Task,Agent,Workflow`: `Task` is the subagent tool of Claude Code 2.1.319, `Agent` its newer name, and `Workflow` runs scripts that start subagents | `--disable multi_agent --disable multi_agent_v2`, before `resume` on a next turn |
| `edit = "none"` | `--disallowedTools Edit,Write,NotebookEdit`: a deny, so the person's own settings (an `acceptEdits` mode, an Edit allow rule) cannot approve an edit either | `--sandbox read-only`, which has no network |

- `read_folders` grants reading only. It is not `--add-dir`: that makes a working folder, which
  under the person's `acceptEdits` mode a turn could also write to (Claude Code 2.1.319).
- That holds in Claude Code's default and `acceptEdits` modes. Under the person's own `auto`
  mode, Claude Code may still write in a `read_folders` folder. AgentBus does not change the mode.
- All of `--allowedTools` is one comma-joined value.
- A `read_only` turn stays read-only: `allow` does not widen it. `read_folders` and `forbid` still
  apply.
- A `forbid` wins over an `allow`, and over the person's own Claude Code settings.
- The flags are in the turn's header line in `logs/runs/<session>.log` (below), so a turn shows
  what it was given.

**How the gateway's guard works.** It applies `forbid` to every model reply of a turn started
through `POST /api/v0/turns`, for claude and codex.

- It reads each shell call: claude's `Bash` tool, and codex's `exec` tool, a JavaScript program
  whose `tools.exec_command({cmd: "…"})` calls each hold a command (codex-cli 0.164.0). A
  function tool with a `cmd` or `command` argument is read the same way.
- It parses the command as shell (with `mvdan.cc/sh`) and matches every simple command in it
  alone: in pipelines, `&&` and `||` chains, subshells, functions, loops, `$(…)` and backquotes.
  A command matches an entry when its words start with the entry's words. The program matches by
  its base name, so `/usr/bin/git push` matches `git push`. For git, global options such as
  `-C <path>`, `-c k=v` and `--git-dir=…` are skipped first, so `git -C repo push origin main`
  matches `git push` and `git log --grep push` does not. Redirections are not words, so
  `git push>/tmp/log` is a push.
- Looked through: `sudo`, `env` (with `-S "…"`), `xargs`, `nice`, `time`, `VAR=value` in front;
  `sh -c "…"` and `bash -e -c "…"` with any options before the script; `eval`; a here-document
  or a here-string fed to a shell. Comments and the text of a here-document fed to another
  program are not commands.
- A command the parser cannot read is refused when any run of its words reads as an entry. That
  errs on the side of refusing.
- In codex's script, every `cmd: "…"` string counts, even one the script only stores. That errs
  on the side of refusing.
- A matching call is renamed in the reply to `AgentBusRefused_<name>`, so the agent has no such
  tool and does not run it. On the turn's next request the gateway puts the real name back and
  replaces that call's output with: `Refused by the AgentBus gateway: the forbid rule "git push"
  in permissions.toml matched this command. The command did not run.`
- A call is held from its first event to its last, and nothing else is held. codex runs a call as
  soon as it ends, not at the end of the reply.
- Streamed and unstreamed replies are both guarded. Claude Code sends an unstreamed request after
  a stream sits idle for 300 seconds.
- Every refusal is stored as a `tool.refused` event with `tool_id`, `tool_name`, `tool_input` and
  `rule`. The call's `tool.requested` is still stored, and its `tool.completed` carries the
  refusal text.
- Measured with Claude Code 2.1.319 on sonnet and codex-cli 0.164.0 (2026-11-08): both models
  accepted the refusal and did not try another way, even when told to.

**The limits, plainly:**

- **The guard reads text. It is not a sandbox.** It cannot see a command built while the agent
  runs (`"git " + "push"`), one run from a script the agent wrote earlier, or one run by an agent
  whose model calls do not pass through AgentBus. For a hard rule, also take away what the
  command needs: for "never push", give the agent no push credentials, or a remote it can only
  read.
- A reply the vendor compresses is not guarded. The gateway asks for none, and Anthropic and
  OpenAI send none.
- Nothing asks a person yet. A call is refused or let through. Holding a call while a person
  decides is the next step: it needs keep-alive events, since each agent gives up on a reply idle
  for 300 seconds.
- codex has no `allow`: it has no per-command flag.
- codex's own `--add-dir` makes a folder writable, which `read_folders` never means, so it is not
  used (codex-cli 0.164.0).
- `read_folders` approves claude's Read, Grep and Glob tools only. A shell command that reads
  there, such as `cat`, is still refused: Claude Code limits those to its working folders, and
  `allow` cannot change that (Claude Code 2.1.319). Only the person's own settings can add a
  working folder.
- `forbid` names shell commands. It does not stop the same thing done another way, such as
  reading `.env` with the Read tool.
- A command on neither list is refused when claude's settings would ask.

**The store keeps full request bodies.** A model request carries everything the agent sent: file
contents, tool output, the memory files in its system prompt. Much of it already reached the store
as event text; the bodies add the rest, never headers. They are kept forever and unencrypted, like
the events. A retention setting and redaction of known secret shapes are planned, and will apply
to them too.

## The store and the cursor

- Every event gets a `seq` when the store writes it: 1, 2, 3 …, without gaps, within one store.
- Every store has a random `store` id, made when the store file is created. A restart keeps the
  store, so the id and the numbering continue.
- **A cursor is the pair `store` and `seq`.** A cockpit sends both. If the `store` it sends is not the
  current one (the file was deleted, or it is another machine's), the service answers **409** with
  the current `store`, and the cockpit starts over from the snapshot. Without this, a cockpit holding
  `seq` 1042 against a fresh store would silently skip its first 1042 events.
- **A store restored from a backup is a new store.** Restoring goes through the service, which
  gives the restored file a new `store` id, so every cockpit starts over. Copying a store file by
  hand is not supported: its numbering can go backwards under an unchanged id.

## Starting up: snapshot, then stream

A cockpit starts in two steps, and misses nothing between them:

1. `GET /api/v0/sessions` returns the sessions **and** the `seq` the snapshot was taken at.
2. `GET /api/v0/events?store=<store>&after=<seq>` streams everything after that point.

An event that lands between the two calls is in the stream, not in the snapshot, so it is neither
lost nor counted twice.

## Endpoints

All responses are `application/json` unless stated.

### `GET /api/v0/sessions`

```json
{"store":"st_9f2c…","seq":1041,
 "sessions":[{"id":"abc","agent":"claude","client":"claude-cli/2.1.318","folder":"/Users/ana/Sites/x",
   "started":"2026-11-05T09:12:03Z","last":"2026-11-05T09:40:11Z","open":true,
   "turns":7,"usage":{"input_tokens":1200,"output_tokens":5400,"cache_read_tokens":310000,
   "cache_write_tokens":40000},"cost_usd":0.31,"cost_complete":true,"resumable":false,
   "title":"Fix the login bug on the settings page"}]}
```

Sessions are newest activity first. Every field is computed from stored events up to `seq`:
- `agent` and `folder` come from the session's latest `turn.started` that names them, else from the
  run's own `session.opened`, so a session whose child has not reached a model call yet still shows
  them. Both name the same run, and the latest of the two is read. A session pointed at the gateway
  by hand, with no run, has both empty. Stores from before 2026-11-05 may hold a
  `session.registered` with them instead, which still counts.
- `first_agent` is the agent of the session's first `turn.started` or run that names one. It stays
  when a later turn runs on another agent, so a list grouped by it keeps a session in one place.
  Absent for a session no run started.
- `open`: true while a run's child for this session is running: a `session.opened` whose `hold`
  has no `session.closed` yet (below). A session no run started is never open.
- `resumable`: the session has a `resume_id` (on a run's `session.closed`, below) and is not
  `open`. A cockpit enables its prompt box on it; `POST /api/v0/turns` with `session` accepts it.
  A claude session continued by own replay can still be refused with 409 when its newest turn's
  history cannot be rebuilt (see `POST /api/v0/turns` with `session`). A turn that was interrupted
  or failed does not cause this: the next turn continues after it.
- `title`: the `title` of the session's latest `session.renamed` (`PATCH /api/v0/sessions/<id>`,
  below). Without one, the `text` of the session's first `turn.started` that has text, the
  person's own words. Over 120 characters (runes) that text is cut to 119 on a rune boundary plus
  `…`, so it is never longer than 120. A `turn.started` with `probe: true` (Claude Code's startup
  quota check) never gives it. Absent when the session has neither.
- `started` and `last`: the time of the session's first and latest event. A `session.renamed` does
  not count, so a rename does not move a session up the list.
- `cost_complete` is false if any of these is true: a call's output count was incomplete; a
  `model.responded` had no `usage` (absent, which is not the same as zeros); a model had no price;
  any `model.responded` lacks `cost_complete: true` (which covers the others, and events stored
  before cost was wired); or an `events.dropped` event names this session or says `all_sessions`.
- `cost_usd` sums `cost_usd` over the session's `model.responded` events.

### `PATCH /api/v0/sessions/<id>` — rename a session

```sh
curl -s -X PATCH -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"title":"Login fix"}' $B/api/v0/sessions/abc
# {"seq":1042,"id":"evt_7a1b…","title":"Login fix","old_title":"Fix the login bug on the settings page"}
```

It stores a `session.renamed` event with `title`, the new name, and `old_title`, the name the
session had just before (its latest rename, else its first message as `title` above; absent when it
had none). From then on `GET /api/v0/sessions` names the session by its latest rename. Earlier names
stay in the record: every rename is one of the session's events, so
`GET /api/v0/sessions/<id>/events` lists them all, in order.

- The body is one JSON object with `title` and nothing else. Spaces at either end are dropped.
  What is left must be 1 to 120 characters (runes), on one line: a newline, tab, Unicode line or
  paragraph separator, or other control character is refused. Each refusal is **400** with the reason; nothing is stored.
- **404** for a session the store has no events for.
- A rename is not activity: the session's `last` stays, so the list keeps its order.
- With the main token, any session. With a turn token, a session in its scope (its request tree,
  or its own session with `scope = "session"`), unless `rename = false` in `[agent_api]`. A
  read-only turn's token may not rename. The turns token may not.
- The answer is the stored event: `seq`, `id`, `title` and `old_title`.

### `GET /api/v0/events?store=<store>&after=<seq>&session=<id>`

A live stream, `text/event-stream`. Each message is one event:

```
id: st_9f2c…/1042
event: tool.requested
data: {"seq":1042,"id":"evt_…","kind":"tool.requested", …}
```

- It sends every stored event with `seq` greater than `after`, in order, then stays open for new
  ones. Each event is sent once per connection.
- `store` and `after` are required together; omit both to start with new events only.
- `session` limits the stream to one session's events, **plus every `events.dropped` that lists
  that session or says `all_sessions`**, so a cockpit watching one session still learns its costs are incomplete.
- A comment line (`: ping`) every 15 seconds keeps idle connections open.

### `GET /api/v0/sessions/<id>/events?store=<store>&after=<seq>&limit=<n>`

One session's stored events, oldest first. `limit` defaults to 1000, at most 10000. A page also
stops before its event JSON passes 8 MiB, but always holds at least one event, so a single larger
event still pages forward; `next_after` is then the last seq in the page.

```json
{"store":"st_9f2c…","events":[{"seq":12, …},{"seq":15, …}],"next_after":15}
```

`next_after` is the cursor for the next page, or `null` when there are no more events right now.
`store` and `after` go together, as on the stream; leaving both out starts from the beginning. The
events include every `events.dropped` that lists the session or says `all_sessions`, as the stream's
session filter does.

### `POST /api/v0/turns` — start a turn

**One endpoint starts a turn.** Without `session`, the turn opens a new
session; with `session`, it is the next turn of that session (below). **Every turn runs as a
job** ([Jobs](#jobs)): this makes a turn job in the queue and answers once a runner has started
its turn. The jobs API does the same with more choices: a worktree, a review, a cancel. A program reads the work from
the event stream, or from `GET /api/v0/turns/<turn>/events`. It was `POST /api/v0/runs` until
2026-11-05, renamed with no alias: the turn is the one resource, and a run is only the
process that does it.

```json
{"agent":"codex","folder":"/Users/ana/Sites/x","prompt":"Review this diff: …","model":"gpt-5.5","read_only":true,"idempotency_key":"job-42"}
```

- **`agent` is a saved agent's id** ([Agents](#agents)). Its harness, access and model start the
  turn; a `model` sent here is used in place of the agent's. `claude` and `codex` are the ids of
  the two agents a new data folder starts with, so a 0.1 client that sends them gets the same
  turn as before. When the person has removed one of those two, the name still starts that harness
  on its own subscription.
- **Isolation is automatic.** A new session runs in its own copy of `folder`, made as the
  `isolation` setting says ([Isolation](#isolation)), under `worktrees/` in the data folder. It
  starts from the folder exactly as it is, uncommitted edits included, and nothing the turns do
  reaches the folder. The session's `folder` in its events is the copy's folder.
  `"isolate": false`, or the setting `"none"`, uses the folder as it is.
  `isolate` with `session` is refused: a session keeps its workspace.

The reply is **202** `{"id":"abc","turn":"t_5d0c19a2e7b3f481","base_url":"http://127.0.0.1:8787/s/abc/t/t_5d0c19a2e7b3f481","job":"job_1f…"}`,
sent as soon as the agent has started. `id` is the session, `turn` the new turn, `job` the turn
job that runs it. The agent's work arrives later, as events of that session, **every one carrying
`turn`** (below).

- **The turn is the job's attempt**: `turn` is the attempt's id. The queue names the session when
  the job is made.
- **A turn no runner starts within 30 seconds** answers **202** with `"queued":true`, the `job`
  and the session `id`, and no `turn` or `base_url`. The job stays queued and runs when a runner
  is free; its turn then shows in the session. A retry with the same `idempotency_key` names the
  turn once it has started.

- **The run is the turn.** The queue makes the turn id when a runner takes the job: the attempt's
  id, `t_` and 16 random hex characters (the same shape as the gateway's hashed ids). The runner
  puts it in the agent's base URL,
  `<service>/s/<session>/t/<turn>`. The gateway reads it from every request on that URL, so each
  model call the agent makes, and every event read from it, carries that turn, however its
  conversation looks: a one-shot agent's whole loop is one turn, even when it compacts mid-turn.
  A probe (`probe: true`) still has no turn.
- **The turn's `turn.started` is stored by the service before the reply**, with `text` the prompt
  as sent, `agent` and `folder`. The `turn.started` the gateway reads from the agent's first model
  call has the same session and turn, so the store keeps the first one only. The turn is in
  `GET /api/v0/turns` from the moment the reply is sent.
- `session.opened` and `session.closed` carry the same `turn`.

- **The service makes a new session id**, then starts the person's own agent binary, unmodified,
  non-interactively, as its own child. The child's
  folder is `folder` and its stdin is empty. **Only one thing from its stdout reaches an event, the
  vendor's own session id** (`resume_id`, below): the reply text and the work arrive as events
  through the gateway. Its stdout and stderr together go to the run's own log file
  (`logs/runs/<session>.log`, under [Logs](#logs)) and nowhere else.
- **The base URL reaches the agent through the child only**: Claude Code gets
  `ANTHROPIC_BASE_URL` in the child's environment; codex gets `-c openai_base_url=…`.
- **An OpenRouter agent** ([Agents](#agents)) reaches OpenRouter through the service, under
  `/openrouter` of its base URL, so its turns are in the record like any other. Claude Code gets
  `ANTHROPIC_BASE_URL=<base>/openrouter`, the key in `ANTHROPIC_AUTH_TOKEN`, an empty
  `ANTHROPIC_API_KEY`, `CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1`, and the model in
  `ANTHROPIC_MODEL`, `ANTHROPIC_DEFAULT_{OPUS,SONNET,HAIKU,FABLE}_MODEL`,
  `ANTHROPIC_SMALL_FAST_MODEL` and `CLAUDE_CODE_SUBAGENT_MODEL`. codex gets a provider of its own,
  set by `-c` for this run only: `model_provider=agentbus_openrouter`, its `base_url`
  (`<base>/openrouter/v1`), `env_key` (the agent's `key_env`), `wire_api="responses"` and
  `supports_websockets=false`; no `openai_base_url`. The service forwards
  `/openrouter/v1/messages` (Anthropic Messages) and `/openrouter/v1/responses` (the Responses API)
  to `https://openrouter.ai/api/v1/…` with the agent's own `Authorization` header. No format is
  translated. The key is read from the runner's environment when the turn starts, and is never
  stored or logged; events carry the variable's name in `key_env`. Anthropic does not support
  Claude Code on non-Claude models through a gateway; OpenRouter says it is guaranteed only with
  Anthropic's own models.
- **What runs**, with the prompt always last, after `--`, so it is never read as a flag:
  - claude: `claude -p --output-format json --append-system-prompt <hint> [--model M] -- <prompt>`
  - codex: `codex -c openai_base_url=… exec --skip-git-repo-check --json --sandbox <mode> -c developer_instructions=<hint> [-c sandbox_workspace_write.network_access=true] [--model M] -- <prompt>`
- **The instructions tell the agent its history is there, and that it can ask other agents.** They
  are the text of `instructions.md` in the data folder, read when each turn starts, with
  `{session}` replaced by the session id. `agentbus up` writes the file when it is missing, with the
  default text below; it never writes an existing file. A missing file gives the default, an empty
  file gives no instructions, and a file over 64 KiB, not UTF-8 or holding a NUL byte stops the
  turn with 400, as a broken `permissions.toml` does. `GET` and `PUT /api/v0/settings/instructions`
  read and write it ([Settings](#settings)); a runner on another machine uses the server's. Keep the
  text the same within a session: own replay and the prompt cache rely on it. The default is four lines:

  ```
  Your session's earlier turns are stored by AgentBus. To look something up, run:
  agentbus-api GET "/api/v0/sessions/<session>/search?q=<text>"
  with <text> URL-encoded. Each hit's "read" is a path you can GET the same way, for the whole of what matched.
  You can also ask other agents with the same command: start a turn on any saved agent, continue one, or ask for a review. Do not wait for them: each result comes back to you as a later turn. How: agentbus-api GET /docs/llms.txt, section "Ask other agents".
  ```

  `agentbus-api METHOD PATH [BODY]` is the API client. It is a link to the running program, kept
  at `<data folder>/bin/agentbus-api` and made at every start, and that folder goes first on each
  turn's `PATH`. It reads `AGENTBUS_URL` and `AGENTBUS_TOKEN` itself, so the command holds no
  variable: Claude Code refuses a command with a variable in it unless the person approves it, and a
  turn has no one to ask. The name never changes when the program moves, so a session's earlier
  calls still work. A claude turn that may write gets `Bash(agentbus-api:*)` in `--allowedTools`;
  `forbid` in `permissions.toml` still wins. When the link cannot be made, the service log says
  `api_client.link_failed`, and the instructions still name `agentbus-api`.

  It goes through the agent's own instruction flag, never into the prompt: claude adds it at the
  end of its system prompt, codex puts it in its developer message (as a TOML string, the way `-c`
  reads values). Because it never changes within a session, own replay recognises each turn's
  requests as before, and the vendor's prompt cache still holds the earlier turns (measured
  2026-11-07, Claude Code 2.1.319 and codex-cli 0.164.0: turn 2 continued by replay with no
  `replay.failed`, and a later turn ran the search when asked).
  **If the person set `developer_instructions` for codex themselves**, as a top-level key in
  `$CODEX_HOME/config.toml` (`~/.codex/config.toml` by default; a relative `CODEX_HOME` is read
  from the turn's folder, as codex reads it) or a `.codex/config.toml` in the
  folder or any folder above it, a codex turn gets no hint: a `-c` value would replace theirs. The service
  log then says `run.hint … left_out=codex_developer_instructions_set`. Only that key is looked
  for; the file is never written.
- **`read_only` is false unless the body says true.**
  - claude gets `--tools Read,Grep,Glob --allowedTools Read,Grep,Glob`, plus the `read_folders`
    rules of `permissions.toml` in the same `--allowedTools` value. `--tools` is what removes
    every other tool. `--allowedTools` alone only approves tools without asking, and the person's
    own settings could approve more.
  - codex gets `--sandbox read-only`.
  - With `read_only` false or absent, claude keeps its own permission handling. No flag bypasses
    it, so a headless run is refused anything its settings would ask the person about, except
    what `permissions.toml` allows ([The data folder](#the-data-folder)).
  - With `read_only` false or absent and `edit = "session"` (the default), claude gets
    `Edit(//<folder>/**)` in `--allowedTools`, so it edits its own folder without asking.
  - With `read_only` false or absent and `edit = "session"`, codex gets `--sandbox workspace-write -c
    sandbox_workspace_write.network_access=true`: it can edit the folder (and its temporary
    folders) and reach the network, as Claude Code can under typical settings, so it can call the
    service with its turn token. The sandbox is always named, because `codex exec` on its own runs
    read-only. Both are set per turn on the command line; the person's `~/.codex/config.toml` is
    never edited. `danger-full-access` is never used.
  - So, for codex: write mode can edit the folder and reach the network; read-only mode
    (`--sandbox read-only`) can do neither.
- **The run marks the session open while its child runs**: `session.opened` just before the child
  starts, carrying `agent`, `folder`, `turn` and a `hold` id new for each child; `session.closed` with the
  same `hold` when it exits. If `session.opened` cannot be stored the answer is 500 and no child
  starts. A child that fails to start after it, or whose `turn.started` cannot be stored, gets its
  `session.closed` with `error`. That `session.closed`
  carries `exit_code`: the child's exit code, or 128 plus the signal that ended it, and `resume_id`
  when the child's stdout named its session:
  - claude: the `session_id` of the `"type":"result"` object that `--output-format json` prints.
  - codex: the `thread_id` of the `{"type":"thread.started"}` line that `--json` prints first
    (codex-cli 0.164.0, 2026-11-05).
  - Each stdout line is read on its own; a line over 1 MiB is skipped, so a claude reply over
    about 1 MiB leaves the run without a `resume_id`. Only an id shaped
    `^[A-Za-z0-9][A-Za-z0-9-]{0,63}$` is kept.
- **Every `turn.started` of the session carries the run's `agent` and `folder`**, filled in by the
  service from the session's latest `session.opened` as the gateway stores it.
- **A session is open while any of its holds is opened and not yet closed**, counted by `hold` id.
- **When the service starts, it closes every hold left open by its previous process**, storing
  `session.closed` (with no `exit_code`, and `unsettled: true`) for each: that process can no
  longer report their exit, nor confirm that their calls were stored.
  **The service never watches process ids**: a process id can be reused by an unrelated process.
- **At most 4 turns running at once**, unless `agentbus up -max-runs <n>` sets another number
  (1 to 64): it is the built-in runner's capacity. One more waits in the queue.
- **Every turn queues.** A turn sent while the session has a turn running, or turns still waiting
  (a person's message or a delivered result), is never refused: its job waits behind them and runs
  next, one at a time, in the order they were sent. The reply comes at once: **202** with
  `"queued":true`, the `job`, the session `id`, `ahead` (how many of the session's turns are before
  it) and no `turn`. Its `turn.started` is stored when it starts; `GET /api/v0/jobs/<id>` follows it
  meanwhile, and `POST /api/v0/jobs/<id>/cancel` takes it back out.
- **`idempotency_key` makes a retry safe.** It is optional: 1 to 128 characters of `A-Z`, `a-z`,
  `0-9` and `. _ : -`. A client whose request timed out sends the same body again.
  - It is the turn job's key. The turn's `turn.started` carries it as `idempotency_key`.
  - A later request with the same key and the same fields answers **202** with that turn's `id`,
    `turn` and `base_url`, plus `"existing":true`. It makes no job and starts nothing, even after
    the turn has ended. Requests sent at once with one key make one job.
  - The same key with other fields (`session`, `agent`, `folder`, `prompt`, `model` or
    `read_only`) gets **409**.
  - A request refused before it is queued (a 400 from the checks below) uses no key. A job whose
    turn the runner could not start keeps it: a retry gets the same refusal.
  - Keys are compared across the whole service, not per token. Use a new key for each new turn.
- **When the service stops**, it sends each running child SIGTERM, then SIGKILL after 5 seconds,
  and stores each `session.closed` before the store closes. One turn is stopped the same way with
  [`POST /api/v0/turns/<id>/stop`](#post-apiv0turnsidstop--stop-a-turn). A run that started while the service
  was stopping gets 503.
- **The token never reaches the child**: not in its environment, its arguments or a log.
- Refused with **400** and fixed wording. The first four are checked before anything is queued;
  the rest by the runner, whose refusal comes back with its status:
  - an `agent` that is not a saved agent's id, nor `claude` or `codex`
  - a folder that is not absolute, or over 4096 bytes; the runner refuses one that is not an
    existing directory on its machine
  - an empty prompt, one over 256 KiB, or one holding a NUL byte (no argument can carry one)
  - a model that does not match `^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$` (OpenRouter's names, such
    as `deepseek/deepseek-v4.1-flash`, fit). A model is passed only as the value of `--model`,
    never as a flag of its own.
  - a body that is not one JSON object with only these fields, or over 2 MiB
  - the service's own environment already sends this agent elsewhere (`ANTHROPIC_BASE_URL` for
    claude, `OPENAI_BASE_URL` for codex). The run would send that endpoint's credentials to the
    vendor.
  - the agent's program (`claude` or `codex`) is not on the service's `PATH`. Nothing is stored,
    the service log gets a `run.refused agent=<agent> program=<program> reason=not_on_path` line,
    and the reply names the agent and how to fix it, without quoting `PATH`:
    `agent claude cannot start: the program claude is not on the service's PATH. Install Claude
    Code and run claude once to sign in, then try again. If it is installed, start agentbus from a
    shell where claude runs.` For codex it says `Install the Codex CLI`. It is 400 like the
    endpoint refusal above: both are the service's own setup, which the person fixes. `GET
    /api/v0/harnesses` shows which harnesses the service finds before a turn is sent.
  - an OpenRouter agent whose `key_env` is not set, or empty, where the turn runs: `this agent
    reads its OpenRouter key from <name>, which is not set where the turn runs; set it and
    restart agentbus`. Nothing is stored.
- **500** when the store cannot be written, or the agent's program is found but will not start.

**A note for Linux:** one argument there is at most 128 KiB, so a longer prompt fails to start (500).
macOS has no such per-argument limit.

#### `POST /api/v0/turns` with `session` — the next turn of a session

```json
{"session":"abc","prompt":"Now fix the second one","read_only":true}
```

The reply is **202** `{"id":"abc","turn":"t_91ee…","base_url":"http://127.0.0.1:8787/s/abc/t/t_91ee…"}`,
the same shape as a new session's first turn, with the session's own id and a new turn id. It is
sent as soon as the agent has started. The turn's work arrives as events of the same session,
carrying the new turn id, since the agent's base URL names both.

- It starts the session's agent again, in the session's folder, both read as `GET
  /api/v0/sessions` reads them (the latest turn or run that names them), unless the body names
  another agent (switching agent, below). How the new turn reaches the earlier ones depends on the
  agent:
  - **claude: own replay** (the default). The child starts fresh, with no `--resume`:
    `claude -p --output-format json [read-only flags] [--model M] -- <prompt>`. The turn's
    `turn.started` names the earlier turn it continues in `replay_from`. On every model request of
    the turn's main thread, the gateway puts that turn's whole history in front: the messages of
    its last request as the vendor received them, then that call's reply rebuilt from the stream,
    with tool calls, tool results and thinking blocks (signatures included) as sent and received.
    The model sees what `--resume` would have shown it, and the history is read from cache. Each
    such request's `model.requested` carries `replayed`, the number of history messages put in front.
    **Which requests are the main thread's** is one rule. The main thread's identity is the system
    prompt (its blocks after the first, the billing header) of the turn's first request in the fresh
    layout: the prompt as the last text block of its first message, then Claude Code's system
    message. It is fixed by that request and never changed. After it, a request is the main thread's
    exactly when its system prompt is that one, whatever its messages or tools say.
    `replay_from` is always the session's newest turn, never an older one in its place. A turn's
    calls are read from its `model.requested` events, in request order, so a call whose reply event
    was lost is still seen. The history comes from that turn's newest main-thread call, whose
    request must still carry the turn's prompt:
    - **It finished** (a stored `model.responded` with status 200 and no `error`, a stop reason
      other than `tool_use` or `pause_turn`, and a stored reply): its request's messages, then its
      reply. A `model.responded` with an `error`, such as a stream that ended before
      `message_stop`, is not finished whatever its stop reason says.
    - **It did not finish.** The turn was interrupted (its agent was stopped, or the service
      restarted mid-turn, which stops every running agent), or its last model call failed (400,
      429, 529, a dropped connection). The history is what the turn did, made valid for the API,
      then one note. Its last request's messages come first. A reply that asked for tools follows,
      with a `tool_result` marked `is_error` for each `tool_use`, saying the turn was interrupted
      before the result came back. A reply that was cut off, paused or did not arrive whole is
      left out, and so is a failed call's (it has none). The note is the history's last message, an assistant message
      that starts `[Note from AgentBus, not written by the model]`. It says the turn did not finish
      and how (the stop, the status, the exit code), that it may have changed files in part, and to
      check the folder before going on. A call the agent closed before the vendor replied, as when
      its turn is stopped mid-call, is stored with no `status` and the error `the agent closed the
      connection before the vendor replied`; the note says the turn was stopped while that request
      was in flight, with that error, and names no status.
    - **It never reached the model** (its agent failed before any model call: it could not start,
      or exited with an error): the previous turn's history, then the person's message of this
      turn, then a note that it was not acted on and why. A session's first turn has no previous
      one. A turn like this that exited 0 did run, but none of its calls are stored, as in a session
      from before calls were stored: it has no history.
    In the last two cases the next turn's `turn.started` carries `recovered_from` (that turn) and
    `recovered_reason` (how it ended, the words the note uses). The recovered history is stored as
    part of the next turn's requests, so later turns replay it unchanged and read it from cache.
    When no history in the session holds Claude Code's environment message (no earlier turn reached
    the model), the new turn's own system message is kept whole rather than cut to its hook texts.
    There is no history, and no older call or turn stands in, when the turn may be missing events
    (an `events.dropped` naming its session after it started, or `unsettled` on its
    `session.closed`, which includes a turn the previous service left running when it stopped
    without its usual shutdown), when its last main-thread request was compacted, when a reply that
    finished is not stored, or when a stored message cannot be laid out.
    A session with no history that never used replay, such as one from before replies
    were stored, continues by `--resume <resume_id>` instead. Every next turn's `turn.started` says
    which it used in `continuity`: `replay` or `resume`.
    If a request carries the turn's prompt but not in the layout the gateway knows (Claude Code
    2.1.319's, measured 2026-11-06), or the history cannot be read, the gateway does not send it
    without the history: it answers the agent 400, stores `replay.failed` with `error`, and the turn
    fails. A request whose system prompt is not the main thread's (a subagent's, even one given the
    person's words verbatim, or a title) goes on unchanged. A main-thread request not in the fresh
    layout, such as one after Claude Code compacted the conversation, is refused: the gateway has no
    capture of a compacted request, so it cannot place the history in it. Before the main thread's
    first request, only a request with no tools and no prompt passes (the quota check).
    `agentbus up -replay=false` turns own replay off for claude and codex: claude then continues by
    `--resume <resume_id>` and codex by `exec resume`. A session that was already continued by replay is refused there with **409**
    (`… it was continued by own replay, and its resume_id holds only its last turn …`): its
    `resume_id` names only its last turn's own conversation, so resuming it would lose the earlier
    turns.
  - **codex: own replay** (the default), by the same rules as claude's above: the same newest
    turn, the same main-thread rule, the same refusals and the same note. The child is a fresh
    `codex -c openai_base_url=… exec --skip-git-repo-check --json --sandbox <mode> [-c sandbox_workspace_write.network_access=true] [--model M] -- <prompt>`,
    with no `resume`. What differs is codex's layout (codex-cli 0.164.0, measured 2026-11-06):
    - **History.** codex sends the whole conversation as `input` items, with `store: false` and no
      `previous_response_id`. A turn's history is its last request's `input` items as the vendor
      received them, then that call's reply: the `output` items of `response.completed`, each
      item's bytes as sent, so encrypted reasoning goes back byte for byte. Only a completed reply
      is kept.
    - **The splice.** On every request of the main thread, the gateway sends the history, then the
      fresh request's items from its prompt on (the prompt and the turn's own tool loop). The prompt
      is the first user message whose codex content kinds say `user.text`; it is found by item
      kinds, never by text. The fresh request's context items before it (the tools item, developer
      messages, AGENTS.md and the environment) are dropped: the history holds the session's first
      copy. A context item that differs from the history's newest item of its kind (such as new
      permissions after `read_only` changed) is kept, right before the prompt. The body goes on
      plain, without `Content-Encoding`.
    - **The thread id.** codex names a fresh process's thread in `prompt_cache_key`,
      `client_metadata` and five headers. The gateway writes the history's thread id (the session's
      first) over the fresh one in every top-level field but `input`, and in every header. Without
      it the vendor reads only the prefix every thread shares from cache.
    - **The main thread** is the thread the turn's first fresh-layout request names. A request of
      another thread (a subagent) goes on unchanged.
    - **Refused with `replay.failed`**, as for claude: a main-thread request not in the fresh layout
      (anything but context items before the prompt, or a `compaction` item anywhere), a request
      before the main thread's first one that has tools or the prompt, and a turn whose main-thread
      identity is not in memory while it already has stored main-thread calls (its stored requests
      carry the session's thread id, not the agent's, so they cannot say which thread is main).
      The answer is an OpenAI error, status 400, `code` `agentbus_replay_failed`.
    - **A turn that did not finish** gets the same treatment as claude's. A reply that asked for
      tools gets a `function_call_output` or `custom_tool_call_output` for each `function_call` or
      `custom_tool_call`, saying the turn was interrupted before the result came back. A reply that sets
      `end_turn: false`, on the response or on its message, did not finish either. The note is a developer message, last,
      that starts `[Note from AgentBus, not written by the model]`. A reply that asked for a call
      whose result the gateway cannot write (`tool_search_call`, `local_shell_call`, `shell_call`,
      or a new type) or that compacted the conversation gives no history (409).
    A codex session with no history that never used replay, such as one from before codex replies
    were kept, continues by `codex … exec … --sandbox <mode> [-c …network_access=true] resume --json [--model M] -- <resume_id> <prompt>`.
    `codex exec resume` takes no `--sandbox` of its own; given before `resume` it reaches the
    resumed turn (measured on codex-cli 0.164.0, 2026-11-05, from the turn's recorded sandbox policy).
- Fields: `session` and `prompt` (required), `agent`, `read_only` (false unless true), `model`
  (optional). A turn with no `agent` and no `model` runs on the model of the session's latest
  turn, which its `turn.started` names in `model` (absent for the harness's default).
- `folder` comes from the session, so leave it out. If the body sends one, it must be the session's
  own (compared after removing a trailing `/` and the like), or the turn is refused with 400.
- **Switching agent.** `agent` names a saved agent: this turn runs on it, with its access and its
  model unless `model` is sent, in the session's folder (its worktree, for an isolated session).
  Later turns that name no agent stay on it: the session's agent is its latest turn's. Naming the
  agent the session's latest turn ran on is the same as naming none: the turn keeps the session's
  model. This is decided when the turn starts, so a turn queued behind a switch is compared with
  the turn just before it. Each turn's
  `turn.started` and `session.opened` carry the agent that ran it (`agent`, `agent_id`, `access`).
  How the switched turn reaches the earlier ones:
  - **On the same harness** as the session's latest turn (Claude Code to Claude Code, on a
    subscription or on OpenRouter): by own replay, as above. When the request's model differs
    from the model of the history's last request, the history is laid out for it: thinking blocks
    go (their signatures belong to the model that made them), and so do a system message's
    `tool_addition` and `tool_removal` blocks and its fields other than `role` and `content`, such
    as `output_config`. OpenRouter refused each of those on a DeepSeek model (Claude Code 2.1.319,
    2026-11-09). For codex, reasoning items go.
  - **On another harness**, or with codex on OpenRouter (its requests lack the item kinds the
    codex format reads, codex-cli 0.167.0): the history is in a layout the new agent cannot take,
    so the child starts fresh with the earlier turns as plain text in front of the person's words.
    That text gives each turn's number, its agent (the saved agent's name, else the harness) and
    model, the person's message and the text of the reply to its last model call, oldest first;
    tool calls are left out. It is at most 48 KiB, each message cut to 4 KiB and each reply to
    8 KiB, newest turns kept; the prompt and the text together stay within 256 KiB. The turn's
    `turn.started` has `continuity: "transcript"`, the person's words in `text` and the earlier
    turns' text in `context`; the agent's prompt was `context` then `text`. The next turn on the
    same harness continues it by replay as usual. A vendor `resume_id` from before such a turn is never
    resumed: it lacks the turns since, and may be the other vendor's. With replay off, a turn whose
    newest `resume_id` is from before one gets the text instead.
- Each turn is a run: `session.opened` when the child starts, `session.closed` with `exit_code` and
  the new `resume_id` when it exits (the vendor can change it). `session.closed` is stored only
  once the gateway has written all of the turn's model calls (their events and kept replies, which
  are written after the agent has its last bytes), at most 30 seconds after the exit. So a turn
  that reads closed shows all of its calls. When the gateway cannot confirm that (it gave up, or
  its queues were full), `session.closed` carries `unsettled: true`. Same stdout rule, same 4-run bound
  shared with runs, same stopping behaviour, same token rule.
- Refused:
  - **404** `no such session`: the store has no events for it.
  - A turn sent while one runs is not refused: it is queued behind it (above). Only a hold the
    record shows open with no job of this service behind it (a turn this service did not start)
    gets **409** `a turn is already running in this session`, at once, before any job is made.
  - **409** `this session cannot be continued from here`: it has no `resume_id` (with replay off,
    or by the fallback above), such as a session pointed at the gateway by hand, or one whose agent
    is not `claude` or `codex`. With own replay, the version adds `: ` and the reason the newest
    turn's history cannot be rebuilt, naming the turn, such as `turn t_… may be missing events
    (…), so its history cannot be rebuilt` or `turn t_…'s last main-thread request x-… no longer
    carries the turn's prompt, as after a compaction, …` (above). It is sent when the session used
    replay before (its `resume_id` names only its last turn) or has no `resume_id`. A turn that was
    interrupted or failed is never a reason. With replay off, a session that was continued by
    replay is refused too (above).
  - **400** with fixed wording: a `session` not shaped `^[A-Za-z0-9_-]{1,64}$`, which includes
    `""` and `null` (only a body without `session` starts a new session); a `folder` that is not
    the session's own; an `agent` that is not a saved agent (as for a new session); and, as runs, an empty prompt, one over 256 KiB or
    holding a NUL; a bad model; a body that is not one JSON object with only these fields, or over
    2 MiB; the service's own environment sends the agent elsewhere; the agent's program is not on
    the service's `PATH` (nothing is stored; wording as for runs, above).
  - **429** past 4 runs; **503** while the service stops; **500** when the agent's program will not
    start (its `session.opened` gets a `session.closed` with `error`).

`POST /api/v0/sessions/<id>/messages`, which did this before 2026-11-05, is removed. `/sessions`
takes only `GET`.

### `GET /api/v0/turns?session=<id>&limit=<n>&before=<seq>` — the turns

```json
{"store":"st_9f2c…","seq":1041,
 "turns":[{"id":"t_91ee…","session":"abc","agent":"claude","folder":"/Users/ana/Sites/x",
   "prompt":"Now fix the second one","reply":"","model":"claude-opus-5-5","model_calls":1,
   "started_seq":1032,"started":"2026-11-05T09:40:02Z",
   "state":"running","usage":null,"cost_usd":0,"cost_complete":true},
  {"id":"t_5d0c…","session":"abc","agent":"claude","folder":"/Users/ana/Sites/x",
   "prompt":"Review this diff: …","reply":"Two findings. The first is in …","reply_truncated":true,
   "model":"claude-opus-5-5","model_calls":7,"started_seq":980,"started":"2026-11-05T09:12:03Z",
   "ended":"2026-11-05T09:20:41Z","state":"finished","outcome":"finished","exit_code":0,
   "usage":{"input_tokens":1200,"output_tokens":5400,"cache_read_tokens":310000,"cache_write_tokens":40000},
   "cost_usd":0.31,"cost_complete":true}],
 "next_before":null}
```

**Turns are what the cockpit reads; a session is the label that groups them.** A turn is what a
`turn.started` with a `turn` id starts. Newest first, by that `turn.started`'s seq. Every field is
computed from stored events up to `seq`, so a client streams from `store` and `seq` exactly as after
the sessions snapshot.

- `session=<id>` lists one session's turns. `limit` is 1 to 1000, 100 when absent. **A page also
  stops before its rows pass 4 MiB of JSON**, but always holds at least one turn. When more turns
  are older than the page, for either reason, `next_before` is the `before` for the next page; else
  `null`. Page until it is `null` and every turn is reached.
- `prompt` is a preview: the first 500 characters (runes, never a split one) of the person's words
  from the turn's `turn.started`. `prompt_truncated: true` says it was cut; it is absent when not.
  **The whole prompt is that `turn.started`'s `text`**, the first event of
  `GET /api/v0/turns/<id>/events`. A list of many 256 KiB prompts would otherwise be hundreds of MB. `agent` and `folder` come from the same `turn.started`, empty for a
  turn not started through `POST /api/v0/turns`, and so does `agent_id`, the saved agent that ran
  the turn (absent for a turn started by harness name alone). Turns of one session may name
  different agents.
- `reply`, `model` and `model_calls` let a list show a turn without reading its events. The turn's
  last model call is its `model.requested` with the highest seq. `reply` is the `text` of that
  call's `model.responded` (the one with the same `exchange`), cut to 500 characters by the same
  rule as `prompt`, with `reply_truncated: true` when cut. It is empty while that reply has not come,
  when it carried no text (an error), and when the turn has no calls; an earlier call's text is never
  used in its place. `model` is the `model` that last `model.requested` asked for (a turn can use
  several; the vendor's own name for the model is on the `model.responded`), empty with no calls.
  `model_calls` counts the turn's `model.requested` events. Both previews count toward the 4 MiB
  page bound.
- `state`:
  - a turn started through `POST /api/v0/turns`: `running` until its agent's `session.closed`, then
    `finished` on `exit_code` 0, else `failed` (another code, an agent that would not start, or one
    the service closed on restart, which has no `exit_code`). `ended` is that `session.closed`'s time.
  - any other turn (traffic pointed at the gateway by hand): `finished` once it has a
    `turn.completed`, else `running`. `ended` is its latest `turn.completed`.
- `stopped` is `true` when the person stopped the turn with `POST /api/v0/turns/<id>/stop`, absent
  otherwise.
- `outcome` is the latest `turn.completed`'s, when there is one. A one-shot agent can end several
  model calls with `turn.completed`; `state` is what says the turn is over.
- `usage`, `cost_usd` and `cost_complete` follow the session rules above, over the turn's own
  `model.responded` events. An `events.dropped` naming the turn's session makes `cost_complete`
  false.
- Events with a turn id and no `turn.started` are not listed: such as a hashed id codex's compaction
  started mid-turn in hand-pointed traffic (see [Turns](#turns)). Their events are still in
  `/turns/<id>/events`.

### `GET /api/v0/turns/<id>?session=<id>` — one turn

```json
{"store":"st_9f2c…","seq":1052,
 "turn":{"id":"t_5d0c…","session":"abc","agent":"claude","folder":"/Users/ana/Sites/x",
   "prompt":"Review this diff: …","reply":"Two findings.","model":"claude-haiku-4-5","model_calls":3,
   "started_seq":980,"started":"2026-11-05T09:12:03Z","ended":"2026-11-05T09:12:41Z",
   "state":"finished","exit_code":0,"idempotency_key":"job-42",
   "usage":{"input_tokens":1200,"output_tokens":540,"cache_read_tokens":0,"cache_write_tokens":0},
   "cost_usd":0.01,"cost_complete":true}}
```

One turn, with the same fields as a row of `GET /api/v0/turns`. A program polls it to wait for a
turn to end: `state` leaves `running`, and `ended`, `exit_code`, `stopped`, `unsettled` and `error`
say how it ended. To follow it live, stream `GET /api/v0/events` from `store` and `seq`.

- The same turn id can be in two sessions (only in traffic pointed at the gateway by hand).
  `session=<id>` picks that session's turn; without it the newest is answered.
- **404** when no `turn.started` carries the id.
- A turn token may read the turns of its request tree only, as on `GET /api/v0/turns`.

Three fields are added to every turn row, here and in the list:

- `unsettled: true` when the turn's `session.closed` says the gateway could not confirm that every
  model call was stored, as for a turn the service closed on restart. Own replay does not continue
  from such a turn.
- `error` when the turn's agent could not start, from its `session.closed`. Its `state` is `failed`.
- `idempotency_key`, when the turn was started with one.

### `POST /api/v0/turns/<id>/stop` — stop a turn

Stops one running turn, the way the service stops every turn when it shuts down. The reply is
**202** `{"id":"abc","turn":"t_5d0c19a2e7b3f481"}` (`id` is the session), sent at once.

- **A stop cancels the turn's job.** The runner hears it on its next heartbeat, within 2 seconds,
  and stops the turn. The job ends `cancelled`.

- **What ends:** the turn's agent and every process it started. The service sends SIGTERM to the
  agent's process group and to every process its tree counts (a tool in a process group or session
  of its own included), then SIGKILL after 5 seconds to whatever still runs. Processes outside the
  turn's tree are never signalled.
- **What is recorded:** the turn's `session.closed` carries `stopped: true` with the agent's
  `exit_code` (143 for SIGTERM, 137 for SIGKILL). It is stored once the tree has ended, as for any
  turn. `GET /api/v0/turns` shows the turn with `stopped: true` and `state` `failed` (`finished`
  only if the agent still exited 0). The service log gets `run.stop session=… turn=…`, and the
  turn's `run.exit` line says `stopped=true`.
- **What the next turn sees:** the session's next turn continues by own replay as after any turn
  that did not finish: what the stopped turn did, an error result for each tool call that got no
  result, then a note. The note and the next turn's `recovered_reason` end with `the person
  stopped this turn` (in place of the exit code's words), such as `it stopped while a tool was
  running; the person stopped this turn`. A turn stopped after its last reply had finished, before
  its agent exited, continues as a finished turn, with no note: the model had done the work, and
  the stop is on its `session.closed` and in the turn list.
- **Twice:** a stop while the turn is already stopping gets **202** again and sends nothing more.
  Two stops at once both get 202; the turn is stopped once.
- **409** when the turn's job has ended, or the turn is not a job of this service (a turn of
  traffic pointed at the gateway by hand, or one from before the job queue).
- **404** when no job and no stored event carries the turn id, and when the service keeps no job
  queue.
- **The main token or the turns token.** A turn token may stop only a turn of work its request tree
  asked for; any other turn, its own included, gets **403**.
- No body is read.

### `GET /api/v0/turns/<id>/diff?session=<id>` — what one turn did

```json
{"turn":"t_5d0c19a2e7b3f481","session":"abc","agent":"claude","agent_id":"claude","model":"claude-haiku-4-5",
 "state":"finished","started":"2026-11-09T09:12:03Z","ended":"2026-11-09T09:12:41Z","duration_ms":38000,
 "job":"job_7f…",
 "files":{"isolated":true,"from":"base","to":"t_5d0c19a2e7b3f481","kind":"snapshot","count":2,
   "changes":[
     {"path":"a.txt","change":"modified","size":12,"sha256":"…",
      "diff":"--- a/a.txt\n+++ b/a.txt\n@@ -1,2 +1,2 @@\n one\n-two\n+2\n"},
     {"path":"logo.png","change":"added","size":5120,"sha256":"…","diff_left_out":true}]},
 "model_calls":{"count":3,"usage":{"input_tokens":1200,"output_tokens":540,"cache_read_tokens":0,"cache_write_tokens":0},
   "cost_usd":0.01,"cost_complete":true,"models":[{"model":"claude-haiku-4-5","calls":3}],
   "calls":[{"exchange":"x-9f2c01ab-1","model":"claude-haiku-4-5","status":200,"usage":{…},"cost_usd":0.003,"latency_ms":900}]},
 "tools":{"count":2,"calls":[{"id":"toolu_1","name":"Bash","input":{"command":"go test ./..."},"commands":["go test ./..."],"error":false},
   {"id":"toolu_2","name":"Edit","input":{…}}]},
 "commands":["go test ./..."],
 "apply":{"state":"none"}}
```

Everything the record holds about one turn, in one answer, for the cockpit and for a reviewer.

- **`files`** comes from the turn's job result: each file it added, modified or deleted, with size
  and SHA-256. In an isolated copy each changed text file has its own unified `diff`, from the
  turn's start checkpoint to its end one, made when the turn ended. A file's earlier content is the
  checkpoint's saved copy, or for the first turn the person's folder while that file is still as it
  was. **`diff_left_out: true`** marks a file whose content changed and has no diff here: binary, a
  link, over 256 KiB, past the 512 KiB budget of all diffs together, or its earlier content no
  longer kept. In a git work tree `git_diff` is git's own diff of the tracked files. `files` is
  `null` while the turn runs, and for a turn no job of this service ran. `files.isolated` says
  whether `POST /api/v0/turns/<id>/apply` can apply it.
- **`model_calls`**: the count, usage and cost as `GET /api/v0/turns/<id>` has them, calls per
  model, and each call (at most 500) with its `exchange`, for `GET /api/v0/calls/<exchange>/context`.
- **`tools`**: each tool call (at most 1000), with its input when it is at most 4 KiB
  (`input_left_out` past that), the shell commands it runs, `error` from its `tool.completed`, and
  `refused_rule` when the gateway's guard refused it.
- **`commands`**: every shell command the agent asked to run, in order, read the way the guard reads
  them (Claude Code's `Bash`, codex's `exec` and `exec_command`). A command built at run time, or
  run from a script, is not seen. Network calls and processes are not recorded yet; a VM backend
  will add them.
- **`apply`**: the turn's latest apply, as `POST /api/v0/turns/<id>/apply` answers it, or
  `{"state":"none"}`.
- **404** when no `turn.started` carries the id. The main token, the turns token, or a turn token
  for a turn of its request tree.

### `POST /api/v0/turns/<id>/apply` — apply a turn's changes to your folder

```json
{"state":"applied","turn":"t_5d0c19a2e7b3f481","session":"abc","job":"job_8a…","files":["a.txt","logo.png"],"pruned":true}
```

Puts one turn's file changes into the person's folder: the folder the session's copy was made
from. The runner that holds the session writes each file the turn added or changed, by rename, and
removes each one it deleted. No body is read.

- **Refused on a conflict, with nothing written.** Before it writes anything, the runner checks
  every file the turn changed. A file in the folder that is neither as it was at the turn's start
  nor already as the turn left it refuses the whole apply: **409**
  `{"state":"refused","error":"apply refused: 1 file(s) in your folder changed since the checkpoint: a.txt","conflicts":[{"path":"a.txt","why":"changed in your folder since the checkpoint"}],…}`.
  `why` is one of `changed in your folder since the checkpoint`, `added in your folder since the
  checkpoint`, `removed from your folder since the checkpoint`, or `is a folder or cannot be read in
  your folder`. An agent reads it and makes another turn that resolves it, then applies that one.
- **Applying twice is a no-op.** The same turn again answers **200** with `already: true` and the
  files the first apply wrote; nothing more is written. A refused apply may be asked for again: it
  is a new apply job. The runner records each apply it did, before it removes any checkpoint, so an
  apply whose answer was lost answers the same when asked again.
- **A big apply** lists at most what fits the answer: `count` is every file written, and
  `truncated: true` says `files` (or `conflicts`) holds fewer.
- **Order.** Turns may be applied in any order; each is checked against the folder as it is.
  A runner runs one apply at a time, so two sessions of one folder (or of a folder and one inside
  it) never both pass the check and write over each other.
  Applying the oldest unapplied turn of a session also removes the checkpoints before its end
  (`pruned: true`).
- **It waits up to 30 seconds** for the runner. Past that it answers **202** with `state` `queued`
  or `running`; `GET /api/v0/turns/<id>/diff` shows how it ended, and calling apply again waits for
  the same job.
- **400** for a turn that cannot be applied: its job has not ended, it is not the job's last
  attempt, it did not run in an isolated copy (`isolation = "none"`, or `isolate: false`), or its
  session was closed. **404** when no job of this service ran the turn. **409** with `state`
  `refused` and `error` also when the runner keeps no copy for the session, or the turn's
  checkpoints are gone; with `state` `failed` when the apply job did not finish (its runner went
  away): ask again, and a file it already wrote counts as applied.
- **A turn token** may apply a turn whose job ran in a session of its request tree, its own session
  included (`scope = "session"`: its own session only), unless `apply = false` in `[agent_api]`. A
  read-only turn's token gets 403.

### `GET /api/v0/turns/<id>/events?store=<store>&after=<seq>&limit=<n>`

One turn's stored events, oldest first, paged as `GET /api/v0/sessions/<id>/events` is: the same
`limit`, 8 MiB page bound, `store` and `after`, and `next_after`. **A turn id is unique only within
its session**: a client can name the same turn in two sessions' base URLs. `session=<id>` keeps
only that session's events of the turn, and a client that knows the session should send it. Without
it, every session's events of the turn are sent. Only events carrying that `turn`:
`events.dropped` names sessions, never turns, so it is not among them; read the session's events, or
its `cost_complete`, for those.

### Agents

**An agent is a saved setup**: a harness, how it reaches its model (its access), the model and a
name. A turn names one by id. An agent says nothing about where it runs. The agents are kept in
`agents.json` in the data folder, written whole by an atomic rename. A new data folder starts with
two, `claude` (Claude Code) and `codex` (Codex), each on its own subscription with the harness's
default model.

```json
{"id":"a_3f9c0d1e2b4a5c6d","name":"DeepSeek","harness":"claude","access":"openrouter",
 "model":"deepseek/deepseek-v4.1-flash","key_env":"OPENROUTER_API_KEY","key_set":true,
 "created":"2026-11-08T23:10:00Z","updated":"2026-11-08T23:10:00Z"}
```

- `harness`: `claude` or `codex`. `opencode` is listed by `GET /api/v0/harnesses` and refused
  here: not available yet.
- `access`: `subscription` (the harness's own sign-in) or `openrouter` (an OpenRouter key).
  `api` (the vendor's API key) is refused: not available yet.
- `model`: optional for `subscription` (absent: the harness's default). Required for `openrouter`,
  in OpenRouter's naming. Same shape as a turn's `model`.
- `key_env`, for `openrouter` only: the name of the environment variable on the runner's machine
  that holds the key, ending in `_KEY`; `OPENROUTER_API_KEY` when absent. **Never the key.**
  `key_set` says whether it is set in this service's environment; its value is never sent.
- `name`: 1 to 100 bytes on one line, unique among the agents ignoring case.

Routes, all with the main token only:

- `GET /api/v0/agents`: `{"agents":[…]}`, in the order they were made.
- `POST /api/v0/agents` with `{name, harness, access, model?, key_env?}`: **201** with the agent and
  a new id. **400** with the reason for a refused field.
- `GET /api/v0/agents/<id>`: one agent, or **404**.
- `PUT /api/v0/agents/<id>` with the same body: **200**, its fields replaced, its id kept.
- `DELETE /api/v0/agents/<id>`: **204**. Its sessions stay and can still be continued: each turn
  recorded its harness, access and `key_env` (`agent_id`, `access`, `key_env` on `turn.started` and
  `session.opened`), and a next turn uses those, not the saved agent. An edit is not seen by a
  session already started either.

### `GET /api/v0/harnesses` — what an agent can run on

```json
{"harnesses":[
  {"name":"claude","label":"Claude Code","found":true,"version":"2.1.319","signed_in":true,
   "access":[{"kind":"subscription","label":"Claude subscription","models":["sonnet","opus","haiku"]},
             {"kind":"openrouter","label":"OpenRouter key","models":["deepseek/deepseek-v4.1-flash","openai/gpt-oss-120b"],
              "any_model":true,"key_env":"OPENROUTER_API_KEY","key_set":true},
             {"kind":"api","label":"API key","disabled":true,"models":[]}]},
  {"name":"codex","label":"Codex","found":false,"access":[…]},
  {"name":"opencode","label":"opencode","disabled":true,"found":false,"access":[]}]}
```

It was `GET /api/v0/agents` in 0.1, renamed in 0.2 when agents became saved objects.

- `found`: the harness's binary is on the service's `PATH`, by the same lookup runs use.
- `version`: the version token of `<bin> --version`, run with a 5 second limit. Absent when the
  binary is not found or the output has no version.
- `signed_in`: from the harness's own offline status command, by its exit code only, output
  discarded: `claude auth status` and `codex login status` (exit 0 signed in, 1 not; both answer
  with network access denied, measured 2026-11-05 on 2.1.318 and 0.164.0). Absent when the command
  ends any other way.
- `access`: each way the harness can reach a model, in the order a form shows them, with the
  models to offer first. `any_model` means any name is accepted, as an OpenRouter model id.
  `disabled` means not available yet. `key_set` is read on every call.
- `disabled` on a harness: no agent can be saved on it yet. It is not probed.
- Never a path, and nothing from the harnesses' configuration.
- The probe is kept for one minute.

### `GET /api/v0/version` — which build this service is

```json
{"version":"0.1.0","commit":"5fea80b5aaaabbbbccccddddeeeeffff00001111","modified":false}
```

- The same values `agentbus version` prints. A release build has its version and commit from the
  release's ldflags. A build from a clone has `dev` and the commit Go read from git, with
  `modified` true when the tree had changes. `go install …@v0.1.0` has the module's version and no
  commit.
- `commit`: absent when the build does not know it. `modified` is always sent, false for a release.
- The main token only; a turn token gets 403. Fixed for the life of the process, so a cockpit
  reads it once when it connects.

### `POST /api/v0/events` — a program writes its own event

```json
{"kind":"x.review.finding","session":"abc","turn":"t_…","text":"Line 12 reads past the end.",
 "data":{"file":"a.go","line":12,"severity":"high"}}
```

The reply is **201** `{"seq":1043,"id":"evt_…"}`.

- **`kind` must match `^x\.[a-z0-9][a-z0-9_-]{0,31}\.[a-z0-9][a-z0-9_.-]{0,63}$`**: `x.`, then the
  program's name, then the event's name. A program can never write a framework kind such as
  `turn.completed`.
- `session` is a session the store already has events for, or `default`. Any other gets **404**.
- `turn` is optional, at most 128 bytes. `text` is optional, at most 64 KiB. `data` is optional and
  must be a JSON object, at most 64 KiB.
- The service stamps `id`, `time`, `vendor` `"program"` and `seq`, then stores and streams the event
  like any other.
- Refused with **400** and fixed wording: a body that is not one JSON object with only these fields,
  or over 128 KiB; a bad kind; a field over its bound; `data` that is not an object.

### `GET /api/v0/calls/<exchange>/context?full=<bool>` — what one model call sent

`<exchange>` is the `exchange` field of the call's events. The reply is **200**:

```json
{"exchange":"x-9f2c01ab-7","session":"abc","vendor":"anthropic","time":"2026-11-05T09:00:00Z","size":99554,
 "stop_reason":"end_turn",
 "reply":{"kind":"message","label":"assistant","hash":"77c0…","size":131,"approx_tokens":50,"json":{"role":"assistant","content":[{"type":"text","text":"…"}]}},
 "request":{"model":"claude-sonnet-5-5","max_tokens":32000,"stream":true},
 "pieces":[{"kind":"system","label":"text","hash":"3a31…","size":1203,"approx_tokens":453,"json":{"type":"text","text":"…"}},
           {"kind":"tool","label":"Read","hash":"0ab1…","size":2210,"approx_tokens":831,"json":{"name":"Read","…":"…"}},
           {"kind":"message","label":"user","hash":"ee21…","size":88,"approx_tokens":34,"json":{"role":"user","content":"…"}}]}
```

- The gateway splits each model request's body into pieces. Anthropic Messages: each `system` block
  (or the `system` string as one piece), each tool definition, each message. OpenAI Responses:
  `instructions`, each tool, each `input` item. `request` holds every other top-level field, in the
  request's order.
- `pieces` are in the request's order. Each `json` is the piece byte for byte as the vendor got it,
  after the gateway decoded any request compression: the service writes the stored bytes into the
  reply as they are, never re-encoded, compacted or escaped. So the SHA-256 of `json`'s raw text is
  `hash` and its length is `size`. A parser that keeps raw text (Go's `json.RawMessage`) reads those
  bytes; `JSON.parse` gives the same value but not the same bytes. `request` is written the same way.
- `label`: a tool's name, a message's role, an input item's role (a message) or type (anything else),
  a system block's type.
- `approx_tokens` is `size / 2.66`, the ratio measured on one Claude Code request against the API's
  own count, measured 2026-11-05. It was off by up to
  15% per piece there. It is not the vendor's tokenizer; the real count of the whole call is the
  `usage` on `model.responded`.
- `?full=false` leaves out every `json`: sizes and labels only. Any value other than `true` or
  `false` gets **400**.
- **404** when no body is stored for the call: not a model call, a request the gateway could not
  read, a call from before this version, or a call that arrived while the store was more than
  64 MiB of bodies behind (that body is skipped; its events are not affected).
- Each piece is stored once under its SHA-256 (`hash`), so the system prompt and tools a session
  sends on every call, and the conversation each call repeats, are kept once. Ten calls of one
  measured Claude Code session took about 18 KB of store each, against 110 KB per request body.
- `reply` and `stop_reason`, when kept: the model's reply rebuilt as the message the harness sends
  back next. Anthropic: the assistant message with every block in full, each block's fields in the
  vendor's order (text, `tool_use` with every field such as `caller`, `thinking` with its
  `signature`), compact JSON. OpenAI (codex): the list of output items of `response.completed`,
  each item's bytes as the vendor sent them (encrypted reasoning included), and `stop_reason`
  `completed`, or `completed:end_turn_false` when the response set `end_turn: false` (codex goes
  on with the turn after it). Stored once under its SHA-256 like a piece, and written into the reply the same way.
  Absent when none is kept: a reply that was not 200, ended before `message_stop` or
  `response.completed`, ended incomplete or failed, carried a delta the gateway cannot rebuild
  (such as `citations_delta`), or a probe's. Own replay reads it: a turn's last reply comes back in
  no later request of that turn.
- **For a replayed turn the stored request is the one the vendor received**, with the earlier
  turn's history in front, not the agent's own. A request the gateway refused (`replay.failed`) is
  stored as the agent sent it, and has no reply.

### `GET /api/v0/sessions/<id>/search?q=<text>&limit=<n>&before=<cursor>` — search a session's history

Text search over what the store holds of one session. The reply is **200**:

```json
{"store":"st_9f2c…","hits":[
  {"source":"request","turn":"t_5d0c…","call":"x-9f2c01ab-7","piece":12,"seq":1042,"kind":"message","role":"user",
   "block":"tool_result","snippet":"…func Login(user string) error {…","read":"/api/v0/calls/x-9f2c01ab-7/pieces/12"},
  {"source":"event","turn":"t_5d0c…","seq":1001,"kind":"turn.started",
   "snippet":"Fix the login bug","read":"/api/v0/sessions/abc/events?store=st_9f2c…&after=1000&limit=1"}],
 "next_before":"1001.0"}
```

**What is searched:**

- the **messages** (Anthropic) and **input items** (OpenAI Responses) of the session's stored
  requests, tool calls and tool results among them. A message comes back in every later request
  of its conversation, so each piece is searched **once, in the first call that carried it**.
- each call's **kept reply**, once, the same way (`source` `reply`, `read` ends in `/pieces/reply`).
  A codex reply is searched **item by item**: each output item is one hit at most (`kind`
  `input_item`, `role` its role or type), under the hash a request item of the same bytes has, so an
  item a later request carries is found once. An item with tool definitions
  (`tool_search_output`) is not searched, as in a request.
- the `text` of three kinds of event: `turn.started` (the prompt as sent), a program's own `x.*`
  events, and a `model.responded` whose reply cannot be searched as a reply: none is kept, such as
  one that failed, or it is kept but its call's request is not.

Not searched: system blocks, instructions and tool definitions, codex's `additional_tools` and
`tool_search_output` input items included (every call repeats them, and they are the harness's), other event fields, and a call that has no `model.requested` stored (it has no
seq and no turn).

- Inside a piece only the **string values** are read, decoded from JSON, so `\u00e9` matches as `é`.
  The values of `type`, `id`, `tool_use_id`, `call_id`, `role`, `status`, `signature`,
  `encrypted_content`, `data`, `media_type` and `cache_control` are skipped.
- **Case is ignored**, character by character (Unicode simple folding). There is no ranking, no
  word stemming and no regex: `q` is found as written.
- `q` is required once: 1 to 256 bytes of UTF-8 with no NUL byte; otherwise **400**.
- **One hit per piece or event**: its first match. `snippet` is up to 80 characters on each side of
  it, cut at whole characters, with `…` marking a cut. A NUL byte in the stored text does not hide
  a match after it; it stays a NUL character in the snippet (`\u0000` in JSON).
- `block` is the `type` of the content block the match is in (`text`, `tool_use`, `tool_result`,
  `thinking`, `function_call_output` …), absent when there is none.
- `seq` orders the hits, **newest first**: the call's `model.requested` for a request hit, its
  `model.responded` for a reply hit, the event itself for an event hit.
- **Paging:** send `next_before` back as `before` for the next, older page; `null` means nothing
  older is left. `limit` is 1 to 200, 20 when absent. A page also stops at 1 MiB of hits or after
  reading 64 MiB of text, so a page can hold fewer than `limit` hits, even none, and still have a
  `next_before`. A `before` that no page gave gets **400**.
- `read` is the path that returns the whole of what matched.

### `GET /api/v0/calls/<exchange>/pieces/<n>` — one piece of a call

One piece of a stored call, so a client reads one tool result without the whole request. `n` is
the piece's index in the `pieces` of `GET /api/v0/calls/<exchange>/context`, from 0, or `reply`
for the call's kept reply.

- **200**: the body is the piece's bytes **exactly as stored**, with `Content-Type:
  application/json`, so their SHA-256 is the piece's hash. The headers describe it:
  `X-Piece-Kind`, `X-Piece-Hash`, `X-Piece-Count` (the request's number of pieces), and
  `X-Piece-Label` when the label is printable ASCII.
  A reply to an allowed origin (any origin, unless `-origin` locks it) also carries
  `Access-Control-Expose-Headers` naming them, so a browser cockpit can read them.
- **404** when no body is stored for the call, when it has no piece `n`, or, for `reply`, when no
  reply is kept. An `n` that is not a whole number without leading zeros, or `reply`, gets the
  unknown-path 404.

### `/api/v0/actions` — reserved

Refusing, holding and starting a review are planned for a later version.

## Settings

The policy ([permissions.toml](#permissionstoml)) and the instructions every turn gets
(`instructions.md`, under [POST /api/v0/turns](#post-apiv0turns--start-a-turn)), read and written through the API. Behind the routes is a
settings store: today the two files in the data folder, so a hand edit and an API write are the
same thing. Only the main token may call them. A turn token and the turns token get **403**.

| Route | What it does |
|---|---|
| `GET /api/v0/settings/permissions` | The policy as JSON, every value filled. Folders are as written, `~` and all. |
| `PUT /api/v0/settings/permissions` | Replace the policy whole. A key left out takes its default. |
| `GET /api/v0/settings/instructions` | `{"text": "..."}`, before `{session}` is filled in. Without the file, the default text. |
| `PUT /api/v0/settings/instructions` | Replace the text. At most 64 KiB of UTF-8, no NUL. |

- **Versions.** Every reply sends `ETag`, the setting's version. A `PUT` must send it back in
  `If-Match`, or `*` to replace whatever is there. Without `If-Match`: **428**. When the setting
  changed since your read, by the API or by hand: **412**, and nothing is written. GET it again and
  apply your change to what is there now.
- **Errors name the field**: `edit must be "session" or "none"`, `[limits] max_depth must be 0 or
  more`, `forbid has the wrong type: …`, `unknown key "allowed"`. Those are **400**.
- **A file that cannot be used** answers a `GET` with **503**, the problem, and the `ETag` of what is
  there, so a `PUT` with that `If-Match` can replace it.
- **A `PUT` of the policy rewrites the file** in the starter's layout, with its comments and your
  values. Comments of your own in the file are not kept. A folder is checked for shape here, and
  for being there when a turn runs.
- The next turn runs under what you wrote, and a turn token's next request too. No restart.
- On a runner (`agentbus runner`), the settings are the server's: a `GET` reads them, a `PUT` gets
  **409**. Change them on the server.

```sh
curl -s -D - -H "Authorization: Bearer $(cat ~/.local/share/agentbus/token)" \
  http://127.0.0.1:8787/api/v0/settings/permissions
# ETag: "3f1c…"
# {"read_folders":[],"allow":[],"forbid":[],"edit":"session","subagents":true,"limits":{…},"agent_api":{…}}
curl -s -X PUT -H "Authorization: Bearer $(cat ~/.local/share/agentbus/token)" -H 'If-Match: "3f1c…"' \
  -d '{"forbid":["git push"]}' http://127.0.0.1:8787/api/v0/settings/permissions
```

## Jobs

Every turn runs as a job. A job is a requested turn: its `kind` is `turn` or `review`. A third kind,
`apply`, puts one turn's changes into the person's folder ([Isolation](#isolation)); only
`POST /api/v0/turns/<id>/apply` makes one. A runner
takes it from the queue and runs it as an attempt. **The attempt is the turn**: the attempt id is
the turn id, `t_` and 16 hex characters, and there is no second id to match. A turn job names its
session when it is made, so `session` is on the job from the start.

`POST /api/v0/turns` is the short way to start one turn: it makes a turn job and answers with its
turn. The jobs API is the whole way: worktrees, reviews, cancel, and the list.

- **States:** `queued`, `taken`, `running`, then `finished`, `failed` or `cancelled`. A failed job
  says why in `reason`: `setup` (the turn did not start, or an apply was refused; `result.refused` and `result.status` say
  why), `conflict` (an apply: a file in the person's folder changed since the turn's start; `result.apply.conflicts` lists each), `agent` (the agent exited with a failure), `runner_lost` (its runner stopped answering
  while the turn ran) or `runner_stopped` (its runner shut down).
- **A session's turn jobs run one at a time**, in the order they were made. A next turn waits for
  the one before it. Reviews never wait.
- **A session's next turn keeps the session's agent and model.** A turn job of a session that names
  no agent and no model runs on the agent and the model of the session's latest turn, as the record
  holds them when the turn starts (`agent`, `agent_id` and `model` on its `turn.started`), so a Fix
  after a review runs on the model the author used. Its `spec.model` stays empty; the turn's
  `turn.started` shows the model. A job that names a model keeps its own. A job that names another
  agent than the latest turn's gets that agent's model, or its default; one that names the latest
  turn's own agent keeps the session's model. The agent's model travels in `spec.agent_model`.
- **No automatic retry once a job is running.** A lease lost before the turn started puts the job
  back in the queue; a lease lost while it runs fails it with `runner_lost`.
- **The job's result** holds how the turn ended (`state`, `exit_code`, `stopped`, `reply`,
  `model_calls`, `cost_usd`), the session it ran in, and `changes`: every file the job added,
  modified or deleted in its workspace, untracked and git-ignored files included, each with size
  and SHA-256. In a git workspace each file also says `tracked`, `untracked` or `ignored`, with the
  commit HEAD named before and after, and a unified diff of the tracked files. In an isolated
  session, `from` and `to` name the checkpoints the changes are between ([Isolation](#isolation)).

### Workspaces

- **A session's first job names its workspace.** Either `repo` and `base` (a branch, tag or
  commit): the runner makes a git worktree of the repo at that base, under `worktrees/` in its data
  folder, and the turn runs there. Or `folder`, used as it is. Or `folder` with `isolate: true`:
  the runner makes the isolated copy the `isolation` setting names ([Isolation](#isolation)).
  `POST /api/v0/turns` sends `isolate: true` unless told otherwise; the jobs API takes it as sent.
- **`spec.agent` is a saved agent's id**, as for `POST /api/v0/turns`. The control plane fills
  `spec.agent` with its harness, and `spec.agent_id`, `spec.access`, `spec.key_env` and, when none
  is sent, `spec.model` from it. A caller never sends those three: they are refused.
- **Later jobs name `session`**, and may name `agent`: the turn then runs on that saved agent, in
  the session's workspace, as for `POST /api/v0/turns` (switching agent). They go to the runner that
  holds the session's workspace.
- **A workspace's `folder` is where its turns run.** Its `kind` is `copy`, `worktree` or `folder`
  (used as it is). For a git worktree of a repo's subfolder, `folder` is the same subfolder inside
  the worktree; later turns and reviews use it.
- **A retry keeps what was frozen.** The idempotency key is matched against the request as sent,
  the saved agent by its id, so a retry after the agent was edited gets the first job.
- **A copy or worktree is removed only when its workspace is closed**, never when a job ends:
  `POST /api/v0/workspaces/<session>/close`. A folder the job named is never removed.

### Isolation

A new session's turns run in an isolated copy of its folder, so nothing they do reaches the
person's folder until a change is applied. The `isolation` setting in `permissions.toml` picks how
the copy is made. Every method starts from the folder exactly as it is, uncommitted edits included.

**How `auto` (the default) picks a method:**

| Machine | Method | What a copy costs |
|---|---|---|
| macOS (APFS) | `copy`: one `clonefile(2)` of the folder | about 20 µs and 0.35 KB per file; bytes are shared |
| Linux, runner can mount (root or `CAP_SYS_ADMIN`) | `overlay` | one snapshot of the folder (a reflink per file on xfs or btrfs), then one mount |
| Linux, runner cannot mount, or the disk cannot hold an overlay | `copy`: a reflink per file (xfs, btrfs), else a plain copy (ext4) | per file; a plain copy also costs every byte |
| Anything else | `copy` | as above |

When `auto` or `overlay` falls back to `copy`, the session still runs. The runner's log and the
job's `workspace.ready` event (`fallback`) say why, for example that the runner is not root.

- **`auto`** (the default): the table above.
- **`overlay`** (Linux): an overlayfs mount. Below it is the session's own read-only snapshot: a
  copy of the folder as it was when the session began (a reflink per file where the disk has
  them), so the person's live folder is never under a copy. Above it is an empty upper folder that
  takes the session's writes. The session shares nothing with another session; closing it removes
  the snapshot with the rest. Needs root or `CAP_SYS_ADMIN`: in a user namespace the mount would
  be hidden from the agent's processes. Without it, `copy`, as `auto` does. Pick an xfs or btrfs
  disk for the runner's data folder: there the snapshot and a changed big file's copy-up are
  reflinks; on ext4 the snapshot copies every byte, and the first write to a 1 GiB file copies
  all of it.
- **`copy`**: a copy of the folder, whatever it is. On macOS (APFS) it is one copy-on-write clone,
  which takes no space until a file changes; on Linux each file is a reflink where the disk has
  them (btrfs, xfs) and a plain copy where it does not. A `.git` folder at the top is copied with
  it, so git works in the copy and a commit there stays there. A `.git` file at the top (a linked
  worktree's) is left out, since it names the person's own repo.
- **`git`**: in a git repo with a commit, a detached git worktree at HEAD, with the folder's
  uncommitted edits (changed, removed and untracked files) carried in. Ignored files are not, which
  keeps a big build folder out. The turn runs in the same subfolder inside it. A subfolder the
  last commit does not have is refused with 400. Outside git, a copy.
- **`none`**: the folder itself.

**Shared dependency folders (`share_read_only`, off by default).** On macOS a clone costs per
file, so a 50,000-file `node_modules` is most of a copy's cost. `share_read_only = ["node_modules"]`
makes `copy` link each named top-level folder to one read-only copy of it, shared by every session
made from the same state of that folder (a changed folder gets a new one). The copy then costs
only the rest of the project. **A turn cannot write there**: `npm install`, `pip install` into a
`.venv`, or a tool that patches a package fails with "permission denied". It is off by default
because agents often install packages, and some tools treat a linked folder differently (a bundler
that resolves links may see files outside the project). Turn it on for sessions that only read
their dependencies. It is the one thing sessions share. `overlay` ignores it.

Each turn starts from a checkpoint and ends with one, named by the turn's id (the start one adds
`.start`, and is the last turn's end when nothing changed in between). A checkpoint records every
file's size, modification time, mode and hash, and keeps its own copy of each file that changed
(a clone where the disk has them), so one turn's changes can be applied after later turns changed
the same file again. A job's `changes` name the two checkpoints in `from` and `to`. The first
checkpoint is `base`: the folder as the session began.

What is read, and when. `base` reads no file: it records each file's size, modification time and
mode, and hashes only links and files written in the two seconds before the copy was made. A later
checkpoint hashes a file only when its size, time or mode changed, once, from its saved copy; an
overlay checkpoint looks at the session's upper folder only. A changed file that `base` did not
hash has its `base` content hashed then, from the person's folder (still with the size and time
`base` saw) or from its overlay layer, when it is small (its diff shows it) or kept its size (it
may be only touched). So a file only touched is no change. A big file whose size changed is not
read again, and its `sha256` as `base` had it is empty in a diff.

Applying a turn (`POST /api/v0/turns/<id>/apply`) puts its changed files into the person's folder,
each one cloned from the checkpoint's copy where the disk can (APFS `clonefileat`, Linux
`FICLONE`), else copied. It is refused, with the reason and each file, when a file there is
neither as it was at the turn's start nor already as the turn left it, and then nothing is
written. How a file there is checked: when the turn's start holds the file unhashed (as `base`
saw it), a file with the same size, modification time and mode is as it was, and is not read;
any other is hashed. A file that differs from the start's hash, or whose size or time changed
while the start has no hash, is a conflict, unless it already is as the turn left it. One edit
this cannot see: a file rewritten with the same size and its old modification time put back by
hand, as git's own index cannot. Applying from the oldest
checkpoint kept also removes what the checkpoints up to it kept. Closing the workspace removes the
copy and all its checkpoints. `GET /api/v0/turns/<id>/diff` shows a turn's changes, each small
text file with its own unified diff, read from the turn's checkpoints when it ended.

An apply is a job of kind `apply`, which the runner holding the session runs through its isolation
backend. It starts no turn, waits for no turn of the session, and fails with reason `conflict` when
a file refused it. A session with an apply that has not ended cannot be closed.

**A known limit: a disk without copy-on-write.** On ext4, and on a folder shared into a container
from another disk, `copy` copies every byte, so a big folder (a `node_modules`, a build output)
is slow to start and takes its full size again, and so does `overlay`'s snapshot. Put the
runner's data folder on xfs or btrfs, use `git` for a repo, or `none`.

### Reviews

- **Ask for one:** `POST /api/v0/jobs/<id>/review` with `{"agent":"claude","model":"sonnet"}`.
  `model`, `idempotency_key` and `include_reply` are optional. `agent` is a saved agent's id, as
  for a turn: the review job's `spec` carries its harness, `agent_id`, `access`, `key_env`, and its
  model when none is sent. An unknown id is refused with 400. The reviewed job must be a turn job
  that has ended, in a workspace that is not closed. Several reviews of one job may run at once.
- **Where it runs:** read-only, in the reviewed job's workspace, on the runner that holds it, in a
  session of its own. The reviewer sees the files as the author left them.
- **Its prompt** is built by the queue and kept in the review job's `spec.prompt`: the author's
  task, the changed files, the diff, and the content of small untracked text files. It asks for a
  review that ends with this block:

  ```json
  {"verdict":"pass","findings":[{"severity":"P2","file":"path/to/file","line":12,"summary":"what is wrong and why"}]}
  ```

  The author's reply is left out unless `include_reply` is true: a reviewer handed the author's
  conclusion tends to confirm it.
- **The verdict** is read from the reviewer's whole last reply when the review finishes: `pass` or
  `fail` as the reviewer wrote it, with any P1 making it `fail`; `unknown` when the reply has no
  block of that shape (never a pass); `null` while it runs, and for a review that failed or was
  cancelled.

### Endpoints

All take the main token. The turns token gets 403. A turn token may read the jobs of its request
tree, ask for a review of one, and cancel work the tree asked for (see "The turn token" above);
the rest get 403.

| Call | What it does |
|---|---|
| `POST /api/v0/jobs` | Make a job: `kind`, `spec`, and optionally `idempotency_key` and `links`. 201, or 200 for a retry with the same key and fields. Other fields under the key get 409. |
| `GET /api/v0/jobs?state=&kind=&session=&reviews=&limit=` | Jobs, newest first, with their attempts. `reviews=<job>` keeps the reviews of that job. |
| `GET /api/v0/jobs/<id>` | One job, with its attempts. |
| `POST /api/v0/jobs/<id>/review` | Make a review job of this job. 201, or 200 for a retry. 409 when it cannot be reviewed. |
| `POST /api/v0/jobs/<id>/cancel` | A queued job ends at once. A running one stops on its runner's next heartbeat, within 2 seconds. |
| `GET /api/v0/attempts/<id>/events` | Every event the runner reported for the attempt. |
| `GET /api/v0/workspaces` | Each session's workspace: its runner, folder, and closed and removed times. |
| `POST /api/v0/workspaces/<session>/close` | Close a workspace. 409 while a job of it has not ended. Its runner then removes a worktree it made. |

```sh
T=$(cat ~/.local/share/agentbus/token)
curl -s http://127.0.0.1:8787/api/v0/jobs -H "Authorization: Bearer $T" \
  -d '{"kind":"turn","idempotency_key":"job-1","spec":{"agent":"claude","repo":"'"$PWD"'","base":"main","prompt":"Say hello.","model":"haiku"}}'
# The next turn of that session, in the same worktree:
curl -s http://127.0.0.1:8787/api/v0/jobs -H "Authorization: Bearer $T" \
  -d '{"kind":"turn","spec":{"session":"<session of the first job>","prompt":"Now add a test."}}'
# A review of the first job:
curl -s http://127.0.0.1:8787/api/v0/jobs/<job id>/review -H "Authorization: Bearer $T" -d '{"agent":"claude","model":"sonnet"}'
```

### The runner protocol

`agentbus runner -server <url>` is a runner; so is the one built into `agentbus up`. Both speak this
protocol over HTTP. A runner pulls work and is never called, so it works behind NAT. These calls take the enrollment
token (`enroll_token` in the data folder) or the runner's own token, never the main token. The
cockpit never calls them.

| Call | What it does |
|---|---|
| `POST /api/v0/runners` | Enroll with name, labels, capacity and protocol 1. Returns the runner and its token, once. |
| `POST /api/v0/runners/<id>/take` | Long poll, up to 30 s: a job with its new attempt and lease, or a cleanup. 204 when there is nothing. |
| `POST /api/v0/runners/<id>/heartbeat` | Idle heartbeat with `capacity`. |
| `POST /api/v0/runners/<id>/removed` | Report a closed workspace removed, or `error`. |
| `GET /api/v0/runners/<id>/settings` | The policy and the instructions, read fresh, for the turn it is about to start. |
| `POST /api/v0/attempts/<id>/start` | Taken to running. `cancel` says the job was stopped. |
| `POST /api/v0/attempts/<id>/heartbeat` | Renew the 30 s lease. Sent every 2 s. `cancel` says stop. |
| `POST /api/v0/attempts/<id>/events` | A batch numbered from `offset`. A repeat stores nothing new; a gap gets 409 with `next`. |
| `POST /api/v0/attempts/<id>/finish` | `status`, `reason`, `result`, `last_seq`. The same finish again gets the same answer. |

Every call after take names its attempt id and number. A call for an attempt that is not current
gets 409 with `stale: true`, and the runner stops that turn.

**A runner follows the server's settings.** Before each turn, `agentbus runner -server <url>` reads
the policy and the instructions from the server (`GET /api/v0/runners/<id>/settings`, with its own
token), never from its own data folder. So a `forbid` rule set on the server holds on every runner
from the next turn. Folders in `read_folders` must exist on the runner, with `~` the runner's home.

**When the server's settings cannot be had, the turn does not start.** That is a server that
cannot be reached, a server whose files are broken, or a server too old to serve the route. The job
fails with reason `setup`, and its result says why, such as `the server's settings cannot be used,
so the turn did not start: the server could not be reached`. A runner never falls back to a policy
it read before: a `forbid` rule just added on the server must hold, and an old copy would miss it.
A server that cannot be reached cannot hand out jobs either, so this costs at most the jobs a
runner held when the server went away.

## Who writes each kind

An event's `kind` names where it comes from, as a topic does in a message broker. There is no
separate source field: read the kind. Built-in kinds have two parts, `<subject>.<what>`. A
program's kinds have three and start with `x.`, which no built-in kind does.

**Observed by the gateway**, from the agent's traffic with its model. These say what the agent and
the model did; AgentBus only reads them off the wire.

| Kind | From |
|---|---|
| `model.requested`, `model.responded` | each model request and its reply |
| `tool.requested`, `tool.completed` | the tool calls in a reply, and their results in the next request |
| `schema.unexpected` | a shape on the wire the adapter does not know |
| `turn.completed` | the reply's stop reason |
| `turn.started` | the person's words in a request, for traffic pointed at the gateway by hand |

**Written by AgentBus** about what it did itself.

| Kind | Written by |
|---|---|
| `turn.started` | the API, for a turn started with `POST /api/v0/turns`, before its agent starts. The store keeps one per turn, so the gateway's copy of the same turn is not stored again |
| `session.opened`, `session.closed` | the engine, when a turn's agent process starts and exits; the store, at start, for a process the previous service left running |
| `session.renamed` | the API, on `PATCH /api/v0/sessions/<id>` |
| `session.registered` | older versions only; nothing writes it now |
| `tool.refused` | the gateway, when it refused a tool call by a `forbid` rule |
| `replay.failed` | the gateway, when it refused a request it could not give its history |
| `events.dropped` | the gateway, when its output fell behind and events were lost |

Two kinds do not follow the subject by writer: `turn.completed` is observed, though most of
`turn.*` and `session.*` is AgentBus's own, and `tool.refused` is AgentBus's own, though the other
`tool.*` kinds are observed.

**Written by programs**, with `POST /api/v0/events`: `x.<program>.<name>`, with `vendor`
`"program"`. The API refuses any other kind there, so a program can never write a built-in kind,
and a built-in kind is never mistaken for a program's.

**A job attempt's own events** are a separate stream, `GET /api/v0/attempts/<id>/events`, written by
the runner that ran it: `attempt.started`, `workspace.ready`, `workspace.changes`,
`workspace.removed`, `turn.created`, `turn.refused`, `turn.stop`, `turn.unended` and `turn.ended`.
They are not in the store's event stream. A turn's own events, such as `turn.started`, are.

## What changes in the event

| Field or kind | Meaning |
|---|---|
| `seq` | the store's order (above) |
| `turn` | the turn this event belongs to (below) |
| `probe` | true on every event of an exchange that is the harness's own check, not a person's request. It has no `turn` and no `turn.started`; only its `model.requested` and `model.responded` are reported (never `tool.requested` or `turn.completed`, whatever the reply says), so its cost and errors stay visible. A cockpit may hide it from the turn view. Today one shape: Claude Code's startup quota check (2.1.318, seen 2026-11-05), an unstreamed request with `max_tokens` 1, no system, no tools, and one user message `"quota"`. Omitted when false |
| `agent`, `folder` | on `turn.started` and `session.opened`: the run's agent and folder. Empty on a turn of a session no run started |
| kind `session.registered` | stores from before 2026-11-05 only: a launcher or run registered the session, with `agent` and `folder`. Nothing writes it now; read it if you meet it |
| kinds `session.opened`, `session.closed` | a run's child started, or exited; `hold` names that child; `turn` the turn it runs |
| `exit_code` on `session.closed` | the child's exit code, or 128 plus the signal that ended it. A hold closed by a service restart has none |
| `resume_id` on `session.closed` | when the child's stdout named the vendor's session: claude's `session_id`, codex's `thread_id`. What a next turn continues |
| `stopped` on `session.closed` | the person stopped the turn with `POST /api/v0/turns/<id>/stop` while its agent ran. The next turn continues after it and is told so. Omitted when false |
| `unsettled` on `session.closed` | the gateway could not confirm that every model call of the turn was stored (it gave up after 30 seconds, or a queue was full), or the service closed the turn at start because the previous service stopped without its usual shutdown. Own replay does not continue from such a turn. Omitted when false |
| `continuity` on `turn.started` | on a session's next turn: `replay` (own replay, with `replay_from`), `resume` (the vendor's `resume_id`) or `transcript` (another harness: the earlier turns as text, in `context`). Absent on a first turn |
| `context` on `turn.started` | with `continuity: "transcript"`: the earlier turns as plain text. The agent's prompt was `context` then `text`; `text` stays the person's words |
| `replay_from` on `turn.started` | the session's earlier turn whose history the gateway replays in front of every model request of this turn (own replay, claude and codex). Absent on a first turn and on a turn continued by the vendor's resume |
| `recovered_from` on `turn.started` | on a replayed turn: the session's previous turn, which did not finish (interrupted, its last model call failed, or it never reached the model). This turn continues after it, and the model is told what happened. Absent when the previous turn finished. A cockpit can show "continued after an interrupted turn" |
| `recovered_reason` on `turn.started` | with `recovered_from`: one sentence on how that turn ended, the words the model's note uses, such as `its last model request failed with status 529; its agent process exited with code 1` |
| `replayed` on `model.requested` | how many messages of the earlier turn's history the gateway put in front of this request. Omitted when none |
| kind `tool.refused` | the gateway refused a tool call of a run's turn, because its command matched a `forbid` entry of `permissions.toml`. `tool_id`, `tool_name`, `tool_input` as the model wrote them; `rule` is the entry; `text` is what the model is told in place of the output. The agent did not run the call. Show it as a warning |
| kind `replay.failed` | a model request of a replayed turn could not be given the earlier turn's history; `error` says why. The gateway answered the agent 400 and sent nothing to the vendor, so the turn fails rather than run without its past. Show it as an error |
| kind `session.renamed` | the session was given a name with `PATCH /api/v0/sessions/<id>`: `title` is the new name, `old_title` the one before it (absent when it had none). It has no `turn`. The latest names the session in `GET /api/v0/sessions` |
| kinds `x.<program>.<name>` | a program's own event (`POST /api/v0/events`); `vendor` is `"program"` |
| `data` | only on a program's own event: the JSON object it sent |
| `sessions` on `events.dropped` | every session that lost events. The queue knows each lost event's session when it drops it. A request without `/s/<session>` counts as the session `default` |
| `all_sessions` on `events.dropped` | true when more sessions lost events than the report lists (over 256, or a session name over 256 bytes). `sessions` is then empty and every session counts as affected. The bound keeps the gateway's memory bounded while its output is blocked |
| `usage` absent on `model.responded` | the vendor sent no usage. Adapters must leave it absent, never fill zeros |
| `usage.cache_write_5m_tokens`, `usage.cache_write_1h_tokens` | split `cache_write_tokens` by cache lifetime, when the vendor said which (Anthropic's `usage.cache_creation`). Omitted when zero. Writes they do not cover have an unknown lifetime and no price |
| `usage.service_tier`, `usage.speed` | the tier and speed the vendor says served the call, in its own words: Anthropic's `usage.service_tier` and `usage.speed` (fast mode says `"fast"`), OpenAI's `service_tier` on the final response (`"default"`; codex's fast tier says `"priority"`). Omitted when the vendor sent none |
| `usage.inference_geo` | where the vendor says the call ran: Anthropic's `"global"` (the default) or `"us"` (data residency). Omitted when the vendor sent none |
| `usage.server_tool_use` | counts of tools the vendor ran itself, keyed by the vendor's own name: Anthropic's `usage.server_tool_use` (such as `web_search_requests`, `web_fetch_requests`), and OpenAI's hosted tool call item types in the reply (such as `web_search_call`). Omitted when there are none |
| `cost_usd`, `cost_complete` on `model.responded` | the call's cost in USD from the built-in price table, at the reported tier and speed. `inference_geo` `"us"` applies the model's data residency multiplier to the token prices; none reported counts as `"global"`. Server tools with a per-use price are added at that price (Anthropic web search $0.01, web fetch free). No reported speed counts as standard speed. No reported tier is priced as standard and marked incomplete. `cost_usd` is absent when `usage` is. `cost_complete` is false when `usage` is absent, the output count is incomplete, no tier was reported, the model, tier or a token class has no price, a server tool was used that has no per-use price, `inference_geo` is neither `"global"` nor `"us"`, or a cache write has no stated lifetime; `cost_usd` is then a lower bound |

Everything else stays as `event/event.go` has it today.

### `schema.unexpected`: the vendor sent something new

An adapter emits `schema.unexpected` when the traffic holds a name it does not know: a content
block or item type, a stop reason or status, a stream event, a delta type, a role, or a usage field.
A harness or API release that changes the wire format shows up here, instead of being noticed by luck.

```json
{"kind":"schema.unexpected","client":"claude-cli/2.1.319","exchange":"x-…","turn":"t_…",
 "unexpected":{"where":"response.content[].type","value":"server_tool_use_v2"}}
```

- `unexpected.where` says where on the wire; `value` is the unknown name, or `field` the unknown
  field name. **Shape only**: names, never text, tool input, tool output or anything that could hold
  a secret. A vendor value or field not shaped like a vendor name (lowercase letters, digits, `_`
  and `.`, up to 48 long) is replaced with `(not a name)`.
- It comes beside the exchange's normal events, which are unchanged. Each distinct shape comes at
  most once per exchange, and at most 16 per exchange. A reply cut off, or one that fails to parse partway, still reports what it showed before that.
- `client` names the harness version that sent it. A cockpit can show it as a quiet warning, such as
  "Claude Code 2.1.319 sent something new", and never as an error: the exchange itself went through.

## Turns

**A turn started through `POST /api/v0/turns` is named by the service** (since 2026-11-05). Its id is in the agent's base URL, so every request the
agent sends names it, and every event of every exchange carries it. One one-shot run is exactly one
turn. Nothing below about hashing applies to it: compaction, retries and the agent's own extra calls
all stay in that turn.

**Other traffic, pointed at the gateway by hand, is named by its conversation, not by timing.**
Replies are read in the background, so the order in which the service learns things is not the order
they happened. Every request carries the whole conversation, so the turn can be read from the
request itself.

**The turn id** is a hash of the session id and the conversation up to and including the message
that started the turn: the last message holding the person's words, skipping tool results, trailing
system messages and a prefill, as the adapter already decides. The hash covers each message's role
and every block in full (text, images and their source, tool ids and inputs, tool results), with
one exception: `cache_control`, which moves between requests. So two prompts with the same words
but different images are different turns.

What follows from that, with no ordering rule:
- **Every event of an exchange carries the turn id of its request**, including the reply's events,
  whenever the reply is read.
- **A tool result lands in its own turn.** The request that carries it still holds the person's
  message that started the turn, so a late result cannot take another turn's id.
- **A cockpit shows a tool result with its request, matched by `tool_id`**, never by turn id. The
  two turn ids differ only if the harness compacted the conversation between the tool call and its
  result; the match by `tool_id` survives that.
- **A retry is the same turn**: it sends the same conversation. The service stores `turn.started`
  once per turn id.
- **Repeating the same words later is a new turn**: the conversation now also holds the reply in
  between.
- **When Claude Code compacts the conversation**, the next turn starts a new chain of ids. That is
  correct: the model sees a different history.

**A turn ends only with a `turn.completed`** from the model's stop reason: finished, partial,
refused, unknown. The service never makes one up, so a late reply can still finish its turn.

**"Last exchange" means the exchange whose `model.requested` has the highest `seq` in that turn.**
Every rule below uses it. When a retry and the original both reply, two `turn.completed` events can
exist for one turn: **the one from the last exchange is the outcome**, whatever order they arrive in.

**What a cockpit shows for a turn without `turn.completed`,** from its last exchange:
- **running**: the exchange has no `model.responded` yet
- **waiting on tools**: its `model.responded` has stop reason `tool_use` (or `pause_turn`), status
  200, no error, and no later exchange in the turn. This is the normal state while a tool runs.
- **stopped**: its `model.responded` has an error or a status other than 200
- **left behind**: a later turn in the same session has started

A `turn.completed` replaces any of these.

## Recorded files

A recorded event file (`.jsonl`, one event per line, in `seq` order) holds everything a cockpit
needs, `session.opened` included, so a cockpit can be built and tested from files alone.

## Logs

For finding out why something failed without a cockpit. Both live in the data folder, in `logs/`,
which is made mode 0700 on every start. Every file in it is mode 0600.

**`logs/service.log`** is what the service did, one plain line each, UTC time first:

```
2026-11-05T15:49:51.185Z service.start listen=127.0.0.1:8787 pid=50479
2026-11-05T15:49:56.778Z request method=POST path=/api/v0/turns status=400 duration=768µs error="folder must be an absolute path to an existing directory, at most 4096 bytes"
2026-11-05T15:49:56.806Z run.start session=238ad7abf801b14c agent=claude turn=t_5d0c19a2e7b3f481 hold=hold_dc6ed6e47a0a10b5 pid=71985
2026-11-05T15:49:57.334Z run.exit session=238ad7abf801b14c agent=claude turn=t_5d0c19a2e7b3f481 hold=hold_dc6ed6e47a0a10b5 exit=1 duration=528ms
```

- `request`: every request, the API's and the agents' alike, once it is answered: method, path
  **without its query**, status, duration. For an error the service wrote itself (the API's
  fixed wording, or a path refused before routing) it adds `error="…"`. A vendor's own error reply
  is a body, so a vendor path logs its status only. A stream logs its line when it ends.
- `run.start` / `run.exit` (a new session's first turn) and `message.start` / `message.exit` (a
  session's next turn): session, agent, turn, hold, the child's pid, then its exit code and how long
  it ran. The log keeps the word run: a run is the process that does one turn.
- `store.error`: an event the store could not write. `service.start`, `service.stopping`,
  `service.stop`.
- At 10 MiB the file becomes `service.log.1`, replacing the older one, and a new file starts.
- **It never holds** a token, a header, a query string, a request or response body, a prompt,
  the agent's output, or anything of a vendor sign-in.

**`logs/runs/<session>.log`** is what the agents the service started printed, stdout and stderr
together, one block per turn under a header line:

```
=== 2026-11-05T15:49:57Z agent=claude exit=1 args=["-p" "--output-format" "json" "--tools" "Read,Grep,Glob" "--allowedTools" "Read,Grep,Glob" "--strict-mcp-config" "--" <prompt>]
Invalid API key · Please run /login
```

- The header has the time the child exited, the agent, its exit code, and its arguments with the
  prompt left out (`<prompt>` stands in its place).
- A turn keeps the last 1 MiB of its output, and the file keeps its last 1 MiB across turns,
  cut at a line break.
- **This file is the agent's own output on the person's machine, so it can hold their data**: the
  reply, file contents, anything the agent printed. It goes only into this file: never into an
  event, an API reply or `service.log`.
- A session pointed at the gateway by hand has no such file: the service started no agent for it.

## Versions

`v0` may change without notice until the November demo. After that, a change that breaks a cockpit
gets a new version path, and the old one stays for a while.

**What 0.2 changed for a 0.1 client:**

- The settings have routes: `GET` and `PUT /api/v0/settings/permissions` and
  `/api/v0/settings/instructions` ([Settings](#settings)). A `PUT` needs `If-Match`. A turn token
  can call none of them.
- A runner on another machine reads the policy and the instructions from its server
  (`GET /api/v0/runners/<id>/settings`), not from its own data folder.

- `GET /api/v0/agents` lists the saved agents ([Agents](#agents)). What it listed before, the
  harnesses this service finds, is `GET /api/v0/harnesses`, with each harness's access and models
  added. `POST`, `GET <id>`, `PUT <id>` and `DELETE <id>` under `/api/v0/agents` are new. A
  preflight now allows `PUT` and `DELETE`.
- `agent` in `POST /api/v0/turns` and `spec.agent` in `POST /api/v0/jobs` name a saved agent. The
  seeded `claude` and `codex` keep 0.1's turns working unchanged.
- `POST /api/v0/turns` isolates a new session in its own copy of its folder, made as the new
  `isolation` setting says (default `copy`), uncommitted edits included. Send `"isolate": false`
  for 0.1's behaviour. A workspace's `kind` may be `copy`. A job's `changes` carry `from` and `to`.
- `model` takes OpenRouter's names: `^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`.
- `turn.started` and `session.opened` of a run carry `agent_id`, `access` and `key_env`.
