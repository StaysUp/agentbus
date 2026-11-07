# The client API

Status: **v0**, as built in AgentBus 0.1. Paths under `/api/v0` may still change before 1.0.

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
    `Access-Control-Allow-Headers: Authorization, Content-Type`. A preflight carries no token, so
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

### The turn token: what a turn may read of its own session

Every turn started by `POST /api/v0/turns` gets two variables in its child's environment, for claude
and codex alike:

- `AGENTBUS_URL`: the service's own address, such as `http://127.0.0.1:8787`. On `0.0.0.0` it is
  still `http://127.0.0.1:<port>`.
- `AGENTBUS_TOKEN`: a token for **this turn only**, `abt_` and 64 hex characters (256 random bits).

The main token never reaches a child. An `AGENTBUS_URL` or `AGENTBUS_TOKEN` in the service's own
environment is replaced.

**A turn token reads its own session and nothing else.** It works on these operations only:

| Operation | Limit |
|---|---|
| `GET /api/v0/turns?session=<own>` | without `session`, its own is used; another gets 403 |
| `GET /api/v0/turns/<id>/events` | the same `session` rule: only its own session's events of that turn |
| `GET /api/v0/sessions/<own>/events` | another session gets 403 |
| `GET /api/v0/sessions/<own>/search?q=…` | another session gets 403 |
| `GET /api/v0/calls/<exchange>/context` | the call must belong to its own session, else 403 |
| `GET /api/v0/calls/<exchange>/pieces/<n>` | the same |

Everything else gets **403** with a plain reason: every write (`POST /api/v0/turns`,
`POST /api/v0/turns/<id>/stop`, `POST /api/v0/events`), the snapshot `GET /api/v0/sessions`, the stream `GET /api/v0/events`, and
`GET /api/v0/agents`. The description and the docs need no token at all.

**It stops working when the turn's child process exits**, even if a process it left behind still
holds its output open, and before the turn's `session.closed` is
stored; after that it gets 401. The service keeps turn tokens in memory only, under their SHA-256,
so a restart leaves no token working. At most one token per running turn exists at once.

From inside a turn, with plain `curl`:

```sh
curl -s -H "Authorization: Bearer $AGENTBUS_TOKEN" "$AGENTBUS_URL/api/v0/turns"
curl -s -H "Authorization: Bearer $AGENTBUS_TOKEN" "$AGENTBUS_URL/api/v0/sessions/<session>/search?q=login"
```

The session id is in the turn's own base URL, `<service>/s/<session>/t/<turn>`, which the agent
already has (`ANTHROPIC_BASE_URL` for claude). `GET /api/v0/turns` without `session` also names it.

## The data folder

`$AGENTBUS_HOME` if set, otherwise `$XDG_DATA_HOME/agentbus`, otherwise `~/.local/share/agentbus`.
It holds `events.db` (the store: events, and since this version every model request's body, see
`GET /api/v0/calls/<exchange>/context`), `token`, `logs/` (below), and `events.db.lock`, which one service holds while it runs, so a second `agentbus up` on the same folder refuses to start. No version in the name.

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
- `open`: true while a run's child for this session is running: a `session.opened` whose `hold`
  has no `session.closed` yet (below). A session no run started is never open.
- `resumable`: the session has a `resume_id` (on a run's `session.closed`, below) and is not
  `open`. A cockpit enables its prompt box on it; `POST /api/v0/turns` with `session` accepts it.
  A claude session continued by own replay can still be refused with 409 when its newest turn's
  history cannot be rebuilt (see `POST /api/v0/turns` with `session`). A turn that was interrupted
  or failed does not cause this: the next turn continues after it.
- `title`: the `text` of the session's first `turn.started` that has text, the person's own words.
  Over 120 characters (runes) it is cut to 119 on a rune boundary plus `…`, so it is never longer
  than 120. A `turn.started` with `probe: true` (Claude Code's startup quota check) never gives
  it. Absent when the session has no such turn.
- `cost_complete` is false if any of these is true: a call's output count was incomplete; a
  `model.responded` had no `usage` (absent, which is not the same as zeros); a model had no price;
  any `model.responded` lacks `cost_complete: true` (which covers the others, and events stored
  before cost was wired); or an `events.dropped` event names this session or says `all_sessions`.
- `cost_usd` sums `cost_usd` over the session's `model.responded` events.

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

**One endpoint starts every turn.** Without `session`, the turn opens a new
session; with `session`, it is the next turn of that session (below). A program reads the work from
the event stream, or from `GET /api/v0/turns/<turn>/events`. It was `POST /api/v0/runs` until
2026-11-05, renamed with no alias: the turn is the one resource, and a run is only the
process that does it.

```json
{"agent":"codex","folder":"/Users/ana/Sites/x","prompt":"Review this diff: …","model":"gpt-5.5","read_only":true}
```

The reply is **202** `{"id":"abc","turn":"t_5d0c19a2e7b3f481","base_url":"http://127.0.0.1:8787/s/abc/t/t_5d0c19a2e7b3f481"}`,
sent as soon as the agent has started. `id` is the session, `turn` the new turn. The agent's work
arrives later, as events of that session, **every one carrying `turn`** (below).

- **The run is the turn.** The service makes the turn id, `t_` and 16 random
  hex characters (the same shape as the gateway's hashed ids), and puts it in the agent's base URL,
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
- **What runs**, with the prompt always last, after `--`, so it is never read as a flag:
  - claude: `claude -p --output-format json --append-system-prompt <hint> [--model M] -- <prompt>`
  - codex: `codex -c openai_base_url=… exec --skip-git-repo-check --json --sandbox <mode> -c developer_instructions=<hint> [-c sandbox_workspace_write.network_access=true] [--model M] -- <prompt>`
- **The hint tells the agent its history is there.** It is three lines, with the session id filled
  in, the same on every turn of the session:

  ```
  Your session's earlier turns are stored by AgentBus. To look something up, run:
  curl -s -H "Authorization: Bearer $AGENTBUS_TOKEN" "$AGENTBUS_URL/api/v0/sessions/<session>/search?q=<text>"
  with <text> URL-encoded. Each hit's "read" is a path under $AGENTBUS_URL that returns the whole of what matched, with the same header.
  ```

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
  - claude gets `--tools Read,Grep,Glob --allowedTools Read,Grep,Glob`. `--tools` is what removes
    every other tool. `--allowedTools` alone only approves tools without asking, and the person's
    own settings could approve more.
  - codex gets `--sandbox read-only`.
  - With `read_only` false or absent, claude keeps its own permission handling. No flag bypasses
    it, so a headless run is refused anything its settings would ask the person about.
  - With `read_only` false or absent, codex gets `--sandbox workspace-write -c
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
- **At most 4 turns running at once.** A fifth gets 429.
- **When the service stops**, it sends each running child SIGTERM, then SIGKILL after 5 seconds,
  and stores each `session.closed` before the store closes. One turn is stopped the same way with
  [`POST /api/v0/turns/<id>/stop`](#post-apiv0turnsidstop--stop-a-turn). A run that started while the service
  was stopping gets 503.
- **The token never reaches the child**: not in its environment, its arguments or a log.
- Refused with **400** and fixed wording:
  - an agent other than `claude` or `codex`
  - a folder that is not absolute, not an existing directory, or over 4096 bytes
  - an empty prompt, one over 256 KiB, or one holding a NUL byte (no argument can carry one)
  - a model that does not match `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`. A model is passed only as the
    value of `--model`, never as a flag of its own.
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
    /api/v0/agents` shows which agents the service finds before a turn is sent.
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
  /api/v0/sessions` reads them (the latest turn or run that names them). How the new turn reaches
  the earlier ones depends on the agent:
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
- Fields: `session` and `prompt` (required), `read_only` (false unless true), `model` (optional).
  **`model` is not remembered**: a turn without one runs on the agent's default model, so a cockpit
  that chose a model for the run sends it again.
- `agent` and `folder` come from the session, so leave them out. If the body sends one, it must be
  the session's own (a folder is compared after removing a trailing `/` and the like), or the turn
  is refused with 400. The service never picks one of two.
- Each turn is a run: `session.opened` when the child starts, `session.closed` with `exit_code` and
  the new `resume_id` when it exits (the vendor can change it). `session.closed` is stored only
  once the gateway has written all of the turn's model calls (their events and kept replies, which
  are written after the agent has its last bytes), at most 30 seconds after the exit. So a turn
  that reads closed shows all of its calls. When the gateway cannot confirm that (it gave up, or
  its queues were full), `session.closed` carries `unsettled: true`. Same stdout rule, same 4-run bound
  shared with runs, same stopping behaviour, same token rule.
- Refused:
  - **404** `no such session`: the store has no events for it.
  - **409** `a turn is already running in this session`: one of its holds is open, or a run or
    turn of it is starting.
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
    `""` and `null` (only a body without `session` starts a new session); an `agent` or
    `folder` that is not the session's own; and, as runs, an empty prompt, one over 256 KiB or
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
  turn not started through `POST /api/v0/turns`.
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

### `POST /api/v0/turns/<id>/stop` — stop a turn

Stops one running turn, the way the service stops every turn when it shuts down. The reply is
**202** `{"id":"abc","turn":"t_5d0c19a2e7b3f481"}` (`id` is the session), sent at once.

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
- **409** when the turn is not running: it has ended (its agent's process has exited, even if its
  `session.closed` is not stored yet), or this service did not start it (a turn of traffic pointed
  at the gateway by hand, or one the previous service process started).
- **404** when no stored event carries the turn id, and when starting turns is turned off.
- **The main token only.** A turn token gets **403**, its own turn included.
- No body is read.

### `GET /api/v0/turns/<id>/events?store=<store>&after=<seq>&limit=<n>`

One turn's stored events, oldest first, paged as `GET /api/v0/sessions/<id>/events` is: the same
`limit`, 8 MiB page bound, `store` and `after`, and `next_after`. **A turn id is unique only within
its session**: a client can name the same turn in two sessions' base URLs. `session=<id>` keeps
only that session's events of the turn, and a client that knows the session should send it. Without
it, every session's events of the turn are sent. Only events carrying that `turn`:
`events.dropped` names sessions, never turns, so it is not among them; read the session's events, or
its `cost_complete`, for those.

### `GET /api/v0/agents` — which agents this service can start

```json
{"agents":[{"name":"claude","found":true,"version":"2.1.318","signed_in":true},
  {"name":"codex","found":false}]}
```

- `found`: the agent's binary is on the service's `PATH`, by the same lookup runs use.
- `version`: the version token of `<bin> --version`, run with a 5 second limit. Absent when the
  binary is not found or the output has no version.
- `signed_in`: from the agent's own offline status command, by its exit code only, output
  discarded: `claude auth status` and `codex login status` (exit 0 signed in, 1 not; both answer
  with network access denied, measured 2026-11-05 on 2.1.318 and 0.164.0). Absent when the command
  ends any other way.
- Never a path, and nothing from the agents' configuration.
- The answer is kept for one minute.

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
| `continuity` on `turn.started` | on a session's next turn: `replay` (own replay, with `replay_from`) or `resume` (the vendor's `resume_id`). Absent on a first turn |
| `replay_from` on `turn.started` | the session's earlier turn whose history the gateway replays in front of every model request of this turn (own replay, claude and codex). Absent on a first turn and on a turn continued by the vendor's resume |
| `recovered_from` on `turn.started` | on a replayed turn: the session's previous turn, which did not finish (interrupted, its last model call failed, or it never reached the model). This turn continues after it, and the model is told what happened. Absent when the previous turn finished. A cockpit can show "continued after an interrupted turn" |
| `recovered_reason` on `turn.started` | with `recovered_from`: one sentence on how that turn ended, the words the model's note uses, such as `its last model request failed with status 529; its agent process exited with code 1` |
| `replayed` on `model.requested` | how many messages of the earlier turn's history the gateway put in front of this request. Omitted when none |
| kind `replay.failed` | a model request of a replayed turn could not be given the earlier turn's history; `error` says why. The gateway answered the agent 400 and sent nothing to the vendor, so the turn fails rather than run without its past. Show it as an error |
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
