# The runner protocol

How runners talk to control: one runner on each machine, which holds that machine's folders. The client API, for people, programs and agents, is
described at `/api/v0/openapi.json` and `/docs` on the running service.


`agentbus runner -server <url>` is a runner; so is the one built into `agentbus up`, which up starts
as a process of its own (below). Both speak this protocol over HTTP. A runner pulls work and is never called, so it works behind NAT. These calls take the runner's own token,
from `agentbus login` once the person approved the machine; never the main token. `agentbus login`
is the only way a machine joins: register takes an enrollment token that `agentbus up` makes at
each start for its built-in runner, keeps in memory and never writes. A removed runner's token
works no more; `agentbus login` on that machine starts a new sign-in. The cockpit never calls them.

All of it is under `/api/v0/runner/`. A running service describes it at
`/api/v0/runner/openapi.json`, without the token, and shows it at `/docs/runner`. It is apart from
the client API on purpose: **it is our binaries' own protocol and not a public promise.** It may
change in any release until third parties write runners. This is protocol version 5: the routes
moved under `/api/v0/runner/` in 0.4 (2), in 0.5 the relay records every model call it carries
(3), and one runner per machine holds its folders and is the only runner of a job that names that
machine, with `agentbus connector` retired (4). Register and every take carry `protocol`; a runner of another version is refused (400) at
either, so a runner already running when control is upgraded stops at its next take.
0.6.1 adds the attempt decision read for H1 **additively** — the version stays 5, because an older runner never calls it, and since 3c18fe0 a runner refuses a control older than itself, so a runner that holds an Ask always meets a control that serves the read.

**Words.** A **job** is the queue's word for work: a turn, a review, an apply, an
apply's preview or a folder check. (A snapshot job, which a runner of 0.4 asked for to build a copy, is retired in
0.5.1: one stored before is kept and read, and a runner refuses it as a kind it does not run.) A
runner takes a job and runs it as an **attempt**. A turn or review job names its turn id when it is
made (`turn` on the job). The runner starts the turn with that id, whichever attempt runs it, so
the turn a client asked for keeps its id. The client API never shows jobs or attempts.

| Call | What it does |
|---|---|
| `POST /api/v0/runner/register` | Register with name, labels, capacity, protocol 5 and `capabilities` (its machine as `connector`, its `folders`), with the enrollment token. Only the built-in runner of `agentbus up` holds that token. Returns the runner and its token, once. A `capabilities.role` is refused (400): `agentbus connector` was retired. |
| `POST /api/v0/runner/login` | `agentbus login`: the same body as register, with no token. Returns a sign-in id, a code for the machine to show, and the runner token, once. The token works only for `GET /api/v0/runner/login` until the person types the code on the Runners page. Waits 15 minutes; at most 20 wait at once (429). |
| `GET /api/v0/runner/login` | With that token: `state` pending, approved (with `runner`, `heartbeat_ms`, `lease_ms`), refused, expired, or removed. |
| `POST /api/v0/runner/take` | `{"wait_ms": n, "protocol": 5, "folders": false}`. Long poll, up to 30 s: a job with its new attempt and lease, or a cleanup (a copy to remove). 204 when there is nothing. With `folders: true`, only a folder job of the runner's own machine (below). Close no longer hands out a cleanup (B14): archiving a session keeps its copy, so nothing removes one today; the path stays for release, which will mark a copy for removal under its own guard. |
| `POST /api/v0/runner/heartbeat` | Idle heartbeat with `capacity`. A restarted runner sends it to check the identity it saved. |
| `POST /api/v0/runner/removed` | Report a copy the runner removed for a cleanup, or `error`. No cleanup is handed out today (close is retired, B14); the endpoint stays for release. |
| `GET /api/v0/runner/settings` | The policy and the instructions, read fresh, for the turn it is about to start. |
| `POST /api/v0/runner/attempts/<id>/start` | Taken to running. `cancel` says the job was stopped. `token` is the attempt's turn token, minted by the server: its agent's API token and nothing else. `stand_in` is its agent's model credential, the only one a runner that holds no secret gives the agent as its sign-in or key; the server's relay swaps it for the real credential and takes nothing else, not the turn token. Both work while the attempt is current, until the turn's child exits. `signin`, when set, is why control can give the turn's agent no credential now, such as `connect your claude subscription in Settings`: a runner whose credentials come from control refuses the turn with it before the agent starts. |
| `POST /api/v0/runner/attempts/<id>/heartbeat` | Renew the 30 s lease. Sent every 2 s. `cancel` says stop. |
| `POST /api/v0/runner/attempts/<id>/events` | A batch numbered from `offset`. A repeat stores nothing new; a gap gets 409 with `next`. |
| `POST /api/v0/runner/attempts/<id>/finish` | `status`, `reason`, `result`, `last_seq`. The same finish again gets the same answer. |
| `GET /api/v0/runner/attempts/<id>/decision?number=n&approval=<id>` | **Added in 0.6.1 (additive; protocol stays 5).** The decision read a holding gateway uses for H1: the guard runs on the runner's own gateway, holds a streamed Ask reply, and polls this about once a second until a person answers or it times out. `number` names the current attempt (as every attempt call does), `approval` the held reply's id. Answers `{"state": "pending" \| "allowed" \| "refused"}`, from control's record — the truth both a built-in and a remote runner read the same way. Scoped to the runner's own current attempt: control binds the polled id to this attempt (as it does on write), so a poll for an id of another turn is scoped here and finds nothing — it reads `pending`, never another turn's answer. An allow releases the held call so the agent runs it in the same turn; a refuse, or no answer in 60 s, refuses it, and no continuation turn is started. |
| `POST /api/v0/runner/attempts/<id>/record` | A runner of a server, while the turn runs and at its end: `{"number": n, "events":[…]}`, at most 500, the turn's events from its own record. The server's record keeps each once, by its id, without its `folder` (the runner's path). A runner that holds no secret leaves out every event of a model call (one with an `exchange`) except the few only its own gateway makes — `tool.refused`, `replay.failed`, `history.shortened`, `history.compacted`, `call.refused`, `approval.requested` and `approval.decided` (the last written by the gateway itself when an H1 hold times out or the agent disconnects): the relay recorded the rest. An `approval.requested` or `approval.decided` is bound to the attempt that raised it: control scopes its id to this authenticated attempt as `<attempt>-<local>`, whatever the runner sent (a raw id from an older runner is scoped, not refused; a foreign id is scoped to the sender's own attempt), and a runner may record only a refusal, never an allow (a person's allow is control's own) — so a runner can never answer or name another turn's approval, and no id is shared across turns. One rule decides who records a call: the server records every call its relay carries, and only those. A call that never reached the server reached no vendor: it stays in the runner's own record and log. The server stores a turn's `run.ended` only after the relay has recorded the turn's calls, each counted from its arrival, before its credential is read. It waits at most 5 s, then stores it with `unsettled: true`. Every event must be of the session control assigned the job: a turn job's session, or for a review the one control named when it made the job (`links.review_session`). |
| `POST /api/v0/runner/blobs` | `{"hashes":[…]}`, at most 10,000: answers `{"missing":[…]}`, the ones the store lacks, so only those are sent. |
| `PUT /api/v0/runner/blobs/<hash>` | A file's content, streamed, up to 4 GiB: kept only when its SHA-256 is `<hash>`. 204. |
| `GET /api/v0/runner/blobs/<hash>` | A file's content, streamed. Only a blob of the runner's own jobs (below); any other gets 403. |
| `POST /api/v0/runner/packs` | Many blobs in one body, a **pack**: frames of `<hash> <size>\n` and then that many bytes, at most 1,000 blobs and 8 MiB of them. Each is checked against its hash. `{"kept": n}`. |
| `POST /api/v0/runner/packs/fetch` | `{"hashes":[…]}`, 1 to 1,000: their blobs as one pack, in that order, streamed. 404 with `{"missing":[…]}` when one is not held. 403 when one is not of the runner's own jobs. |

**A runner reads only its own jobs' blobs.** Both reads answer only blobs reached from the jobs of
each session the runner holds an attempt of that has not ended or ended in the last 10 minutes,
and from its own jobs of no session: their snapshots (the trees and every file in them, the times
blob, `.git`), their changesets and the files those name, and the session's own snapshot. Its
apply and preview jobs, and snapshot jobs stored before 0.5.1, are its jobs too. Any other blob gets **403** `this runner
holds no job that reads this blob`, whether the store holds it or not. Sending blobs and asking
which are missing are not limited.


**The relay records every call it carries.** A runner that holds no secret sends each model call
of its agent to the server's relay, `/relay/<anthropic|chatgpt|cloudcode|openrouter|key-<harness>>/…`,
with the attempt's token as its credential (as `Authorization: Bearer`, or in `X-Goog-Api-Key` for
agy on a Gemini API key). The relay checks the token, adds the real credential (for OpenRouter: the
key set in Settings, or else the one in the agent's `key_env` where control runs; for access `api`:
the vendor's key set in Settings, in that vendor's header, only on the model calls that vendor's
adapter records: any other path or method is refused with 403 and reaches no vendor), and
records the call in the server's record from its own copy, as a gateway on the agent's machine
does: `model.requested`, then the reply as it streams (`model.responded` with its usage and cost,
the tool events, `turn.completed`), and the call's context. Every event has the session and turn of
the job the token names, whatever the request says. A call the relay refuses (403, 503) is recorded
with the refusal as its reply. A vendor it cannot reach or read answers **502** with
`{"error": "control could not reach the model's vendor: <what happened>; cause: <one word>"}`,
is recorded as a failed `model.responded`, and the service log gets one
`gateway.upstream_failed … via=relay session=… turn=… exchange=… class=… reason=…` line, never a
header, a token or a body. A reply cut off before its end is recorded with
`the reply was cut off` in its error, and a request cut off before it arrived whole with
`the request did not reach control whole, so no vendor got it`. The runner's gateway names each call in the
`X-Agentbus-Exchange` header, and the relay records the call under that name, so a `tool.refused`
the runner sends names the same call. The relay ignores a name of another session's call, of
another turn's, or of its own, and names the call itself; the header never reaches a vendor. A
call with no current attempt's token is refused (401) and recorded nowhere.

**The credential goes only on calls the vendor's adapter records** (0.5.3), on the relay and on a
gateway of `agentbus up` alike: one list per adapter (`Calls` in `adapter/`), method, path and the
query keys and values it reads. Today: `POST /v1/messages` with `beta=true` or no query (Anthropic,
and OpenRouter's Anthropic format); `POST /backend-api/codex/responses` and `POST /v1/responses`
with no query (ChatGPT, and OpenRouter's Responses format); `POST /v1internal:streamGenerateContent`
with `alt=sse` or no query, and `POST /v1internal:generateContent` with none (Code Assist). Besides
those, agy's own Code Assist methods `loadCodeAssist`, `listExperiments`, `fetchAdminControls`,
`fetchAvailableModels`, `fetchUserInfo` and `retrieveUserQuotaSummary` (POST, no query) get it too,
because agy stops without them; each is recorded as `vendor.called` with its status (and `retrieveUserQuotaSummary`'s
reply body is read for plan quota; the others' bodies unread). Any other call (another path, another method, another query) is answered **403** with
`{"error": "control adds a credential only to the calls the vendor's adapter records, …"}`, gets no
credential, reaches no vendor, and is recorded as `call.refused` with `call` (method and path,
never the query) and why. A reply in a format the adapter does not read (a Content-Type it does not
list, such as `application/x-protobuf`) is not passed on: the agent gets **502**, and the call's
`model.responded` and a `call.refused` say why.

The runner's own routes carry no runner id: its token names the runner. Every call after take
names its attempt id and number. A call for an attempt that is not current
gets 409 with `stale: true`, and the runner stops that turn.

**One program per machine.** A runner holds its machine's folders and runs that machine's
sessions. It reports its machine's id as `capabilities.connector` (`con_` and 16 hex digits, kept
in `folders.json` in its data folder). The runner sends the id under the old key alone: control's
runner-wire decode is strict, so a control of a build before 0.6 refuses an unknown `machine`
field; a 0.6 control reads the id through either key (machine or connector). The id leaves this
wire for `machine` next release, when runners of a build before 0.6 are no longer served. The
other way, control sends a job's spec to a runner under both keys (the runner reads it leniently),
so a runner of a build before 0.6 reads it from `connector` and a 0.6 runner from `machine`. The
folders it holds it reports as `capabilities.folders`, by reference. **A job that names a machine
(`spec.machine`) goes to that machine's runner and no other**, so a session runs on the machine
that holds its folder for
its whole life: its copy is made there from the folder, by clone or overlay (`sessions/<session>`
in the runner's data folder; copies made before 0.4 stay in `worktrees/job_<id>`), its turns run
there, and its changes are applied there. A next job of a session goes to the
session's runner. One exception keeps a session made before 0.5 going: a session whose first turn
another machine's runner built from a snapshot stays with that runner; its applies still go to the
folder's machine. Each harness of `capabilities.harnesses` may carry
`models` and `models_at`: the model names the harness's own cache in a session's home held after the
runner's latest turn of it (codex's `models_cache.json`), the names only. Control offers them in the
agent window.

A runner takes its jobs in two kinds of places. Turns and reviews take its `capacity`.
**Folder jobs** (`apply`, `preview` and `folder`) take one of 4 folder places
of their own, asked for with `take`'s `folders: true`, so an apply never waits behind running
turns. A `folder` job names a path a person typed for that machine (`spec.folder`): the runner
checks that it is a directory it can read, registers it, and answers its reference in
`result.folder_ref`, or a one-sentence `refused`. It reports its folders again at once. Typing the
folder is the grant for that session. The server never stores the path: it hands it out with the
job from memory, so a job taken after the server restarted has none and is refused. One no runner
took within 15 seconds ends. Control (the server) never reads or writes a folder; it knows one by
its reference. File data goes through the server's blob store, by the hash of its content:

- At the end of each turn the runner puts the copy's changeset (what it holds that differs from
  its base) and the files it names in the store; the job's `result.changeset` names it. A turn of
  a copy never finishes without it: a send control did not answer, or answered only in part, is
  tried again for as long as the finish is; one control refuses, 5 times; then the turn is
  finished as failed with reason `changes_not_kept`.
- An apply job goes to the runner of the folder's machine, which reads both changesets and the
  files from the store (on that machine they are already in its own store), checks the folder and
  writes it.
- A `preview` job (from 0.5.5) is an apply's dry run, for `GET /api/v0/workspaces/{id}/changes`. Its
  spec is the one the apply would have (`folder_ref`, `apply_from`, `apply_to`). The runner runs the
  apply's own code and stops before it would write: it answers `result.preview` with each file the
  apply would write or remove, and each file that would refuse it, with the file's change and
  unified diff against the folder now, and `conflicts`. It writes nothing in the folder and sends no
  event. Control stores the result as a `changes.previewed` event in the record and removes the
  consumed preview job (from 0.6); it serves and pages that event, and reuses it until the session
  changes. A runner of an older build refuses the preview job as a kind it does not run.
- A job of a kind the runner does not run ends as a setup failure with a one-sentence `refused`.
  The `command` job of 0.4 (the person's `!`) is retired: a `!` is now a message to the session's
  agent, which runs it with its own shell tool. One that an older control made is refused so.
- **Outcomes go to control's record, the job keeps a pointer** (from 0.5). On a finish, control
  saves it in its queue, as the job's result. Control's one finisher then stores what the job did
  as an event and ends the job, with that event's id in place of the outcome. A job is never
  finished before its outcome is in the record, so a reader that sees it finished can read the
  outcome at once. Once saved, the finish is the attempt's only one: another gets **409**, and the
  attempt's lease no longer counts. The answer comes once the job has ended (**200**). It is
  **503** when that takes longer than 5 seconds, and **500** when the record could not be written.
  The finish is saved either way: send the same finish again, which changes nothing. If control
  stops in between, it ends the job when it starts again. The outcome event for each kind: a
  turn's `result.changes` becomes `turn.changed` (`result.changes_event`, and `result.checkpoint` keeps the end checkpoint the next turn starts from), and an apply's
  `result.apply` and `result.refused` become `turn.applied` (`result.applied_event`). A finish sent again
  is answered the same and stores nothing twice. A runner may not send `turn.changed` or
  `turn.applied` in a turn's record (**400**): they are control's own. A job from before 0.5 keeps
  its outcome in its result, and readers read it there.
- A runner asks for work about once a second. One not seen for a minute is offline: an apply of
  its folder is refused (503, `the folder's machine is offline, so nothing was written: start
  agentbus runner there, then ask again`), and so is a typed folder for it. A session's turns wait
  in the queue until its machine is back.
- **Control alone decides when a turn ended, and how.** One place (`runner/lease.go`): the end of
  the turn's job on control, a finish it took or an attempt its sweep found lost, recorded once
  as the job's state and as the record's `turn.ended`. `GET /api/v0/turns`, the record's turns
  and the runs control closes all read that decision; a run's own `run.ended` says only how the
  agent ended. A second finish of an attempt is refused, as Buildkite refuses a job finished
  twice and Temporal a completed activity's token.
  - Control counts the 30 s lease from each heartbeat or start it takes, and gives every running
    attempt a fresh one when it starts, once it holds its address and before it serves anything;
    a taken attempt no runner started is queued again then instead, since nothing was spent,
    and its runner's `start` is answered stale. When it stops, its sweep stops before its address
    closes.
  - **The runner keeps no clock over an attempt.** It runs the turn until control answers that
    the attempt is no longer current (a heartbeat or any call answered stale or 404), and only
    then stops its agent. While control does not answer, from a restart of any length or a link
    that is down, the turn goes on, and its start, changes, events, record and finish are tried
    again, a second at first and up to 5 s apart, until control answers.
  - **Control running but not hearing the runner** (a link down longer than the lease): control
    ends the turn with `runner_lost`, and that is how it ended everywhere. The runner stops the
    agent when it next reaches control, and sends nothing more for it. Within a copy,
    every attempt waits for every earlier one, with one exception: a turn passes an earlier
    command (`run_command`), so it runs beside it. A command, discard, update, close or
    put-away waits for everything earlier. So what comes next in that workspace waits on the
    runner until the lost turn's agent, or a lost command's process, has stopped.
  - **A turn's end kept on disk.** A runner keeps each turn's end in `finishes/` until control has
    answered it: taken, or refused as no longer current. Its record goes before its finish, and
    while control does not answer the record at all the finish waits for it. A record batch
    answered 5xx three times is given up: the finish goes, and control closes the run whose end
    it did not get (a run control saw start always ends there). A runner that leaves while control is away (up's built-in runner, once no agent of its
    runs) leaves it there, and the next runner of the machine sends it when it starts.
  A model call
  whose connection to control's relay is refused (nothing was sent) waits up to two minutes on
  the runner's gateway for control, then fails as before.
- A turn whose job has ended, by a finish or otherwise, has its run closed by control if the
  runner's `run.ended` never arrived: control writes it, unsettled, from the job's result, within
  a second. The record keeps one `run.ended` per run, so the runner's own, sent first, wins.
- A turn whose runner stops answering ends with `runner_lost` once its lease runs out. Control
  closes the turn in its record (unsettled), so the cockpit shows it ended and the session takes
  its next turn when the runner is back. A later turn's diff starts at the end of the session's
  latest turn whose changes control holds (the job's `workspace.diff_from`), so anything written
  to the copy since (by a lost turn, or what it left running) shows in that diff, as an apply up to
  it writes it. A session whose first turn ends without an accepted finish (stopped while
  queued, its runner lost, its finish refused) is ended: a turn already queued behind it ends,
  and a new one gets **409**, both with `the session is ended; start a new session`.

The modes:

```sh
agentbus up                                   # control, and the machine's runner as its own process
agentbus server                               # control alone
agentbus runner -server URL [-folder DIR]...  # the one program of any other machine
```

`agentbus connector` was retired in 0.5: it prints the `agentbus runner` line to run instead, with
its own flags, changes nothing and exits 2. A runner started on the connector's data folder keeps
the machine's id and its folders' references, so the sessions and applies that name them reach it.
The old connector stays listed in `GET /api/v0/runners` only while no runner reports its machine.

**up's built-in runner** is `agentbus runner` started by up from its own program, in a session of
its own, so stopping up (Ctrl-C, a restart to upgrade it) leaves it and its turns running. It works
in up's data folder: the folders (`folders.json`, which both read and write), the session copies,
the homes, the run logs and its identity (`runner.json`). It keeps apart only its own record
(`runner/events.db`, a copy of control's the first time, since its sessions' history is there) and
its log (`logs/runner.log`, its own lines in `logs/runner.out`). up starts one whenever none runs (one
runs while it holds `runner/events.db.lock`). It leaves on its own: when no agent of its runs and
control does not answer, so a stopped up leaves nothing behind (a turn's end it could not send
waits in `finishes/` for the next runner); and when up is started from
another program (another build, by the program's content, `runner/build`), once its running jobs
have ended, so the next runner is the new build. It takes no new job meanwhile: turns asked for
then wait in the queue until the new runner starts.

A turn's `start` answer carries `agent_names` (additive; protocol stays 5): the saved agents' names
by id, which a runner uses where a turn names the earlier turns' agents. A runner's gateway takes a
call only on the path of a turn it runs now, carrying that turn's stand-in; any other is refused
(401, or 403 for another turn's) before it is recorded or sent on.

Every runner, up's built-in one too, keeps its own blob store in its data folder (`blob-cache/`,
apart from a server's `blobs/`) and sends
only what the server lacks: blobs under
1 MiB go a pack at a time, four packs at once, and a bigger one alone by `PUT`. A runner fetches a
snapshot's trees a level at a time and its files a pack at a time.

**The blob store is cleaned.** The server keeps every blob that a snapshot or a changeset of an open
session reaches: every job's but those of a closed session. Once a session closes, the server
sweeps (also at start and every hour) and removes each blob nothing reaches that is older than an
hour. A blob just sent, or just answered as held by `POST /api/v0/runner/blobs`, counts as new, so a
snapshot still being sent loses nothing. A runner's own store is a cache of the server's: what it lacks it fetches again, so every blob in it older than an hour may go.

**A runner in a VM on the same machine.** A Lima VM reaches the Mac's loopback as
`host.lima.internal`. A service on loopback refuses another name in `Host`, so start it with
`-host host.lima.internal` (repeat `-host` for more names). The token still guards every request.

**A runner follows the server's settings.** Before each turn, `agentbus runner -server <url>` reads
the policy and the instructions from the server (`GET /api/v0/runner/settings`, with its own
token), never from its own data folder. So a `forbid` rule set on the server holds on every runner
from the next turn. Folders in `read_folders` must exist on the runner, with `~` the runner's home.
The reply also carries `settings_version`, the settings version it was read at (0 from a server
before 0.5), which the turn records on its `turn.started`. The runner reads it all in one call.

**When the server's settings cannot be had, the turn does not start.** That is a server that
cannot be reached, a server whose stored settings cannot be used, or a server too old to serve the route. The job
fails with reason `setup`, and its result says why, such as `the server's settings cannot be used,
so the turn did not start: the server could not be reached`. A runner never falls back to a policy
it read before: a `forbid` rule just added on the server must hold, and an old copy would miss it.
A server that cannot be reached cannot hand out jobs either, so this costs at most the jobs a
runner held when the server went away.

**An attempt's own events.** A runner reports what it did about an attempt with
`POST /api/v0/runner/attempts/<id>/events`: `attempt.started`, `workspace.ready`,
`workspace.changes`, `workspace.removed`, `turn.created`, `turn.refused`, `turn.stop`,
`turn.unended` and `turn.ended`. They stay in the queue (`jobs.db`), apart from the record. The
client API has no route that reads them.
