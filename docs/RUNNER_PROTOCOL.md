# The runner protocol

How runners talk to control: one runner on each machine, which holds that machine's folders. The client API, for people, programs and agents, is in
[CLIENT_API.md](CLIENT_API.md).


`agentbus runner -server <url>` is a runner; so is the one built into `agentbus up`. Both speak this
protocol over HTTP. A runner pulls work and is never called, so it works behind NAT. These calls take the enrollment
token (`enroll_token` in the data folder) or the runner's own token, never the main token. The
cockpit never calls them.

All of it is under `/api/v0/runner/`. A running service describes it at
`/api/v0/runner/openapi.json`, without the token, and shows it at `/docs/runner`. It is apart from
the client API on purpose: **it is our binaries' own protocol and not a public promise.** It may
change in any release until third parties write runners. This is protocol version 4: the routes
moved under `/api/v0/runner/` in 0.4 (2), in 0.5 the relay records every model call it carries
(3), and one runner per machine holds its folders and is the only runner of a job that names that
machine, with `agentbus connector` retired (4). Register and every take carry `protocol`; a runner of another version is refused (400) at
either, so a runner already running when control is upgraded stops at its next take.

**Words.** A **job** is the queue's word for work: a turn, a review, an apply, a snapshot or a
folder check. A
runner takes a job and runs it as an **attempt**. A turn or review job names its turn id when it is
made (`turn` on the job). The runner starts the turn with that id, whichever attempt runs it, so
the turn a client asked for keeps its id. The client API never shows jobs or attempts.

| Call | What it does |
|---|---|
| `POST /api/v0/runner/register` | Register with name, labels, capacity, protocol 4 and `capabilities` (its machine as `connector`, its `folders`), with the enrollment token. Returns the runner and its token, once. A `capabilities.role` is refused (400): `agentbus connector` was retired. |
| `POST /api/v0/runner/take` | `{"wait_ms": n, "protocol": 4, "folders": false}`. Long poll, up to 30 s: a job with its new attempt and lease, or a cleanup. 204 when there is nothing. With `folders: true`, only a folder job of the runner's own machine (below). |
| `POST /api/v0/runner/heartbeat` | Idle heartbeat with `capacity`. A restarted runner sends it to check the identity it saved. |
| `POST /api/v0/runner/removed` | Report a closed session's copy removed, or `error`. |
| `GET /api/v0/runner/settings` | The policy and the instructions, read fresh, for the turn it is about to start. |
| `POST /api/v0/runner/attempts/<id>/start` | Taken to running. `cancel` says the job was stopped. `token` is the attempt's turn token, minted by the server: its agent's API token and model credential, working while the attempt is current. |
| `POST /api/v0/runner/attempts/<id>/heartbeat` | Renew the 30 s lease. Sent every 2 s. `cancel` says stop. |
| `POST /api/v0/runner/attempts/<id>/events` | A batch numbered from `offset`. A repeat stores nothing new; a gap gets 409 with `next`. |
| `POST /api/v0/runner/attempts/<id>/finish` | `status`, `reason`, `result`, `last_seq`. The same finish again gets the same answer. |
| `POST /api/v0/runner/attempts/<id>/snapshot` | For a first turn in an isolated copy of a folder: the snapshot job, made once, on the folder's machine. Ask again until `snapshot` is set. Since protocol 4 a session runs on its folder's machine, so the runner of this build never asks. |
| `POST /api/v0/runner/attempts/<id>/calls` | A runner of a server that keeps its own sign-in (`-gateway local`), while the turn runs: `{"number": n, "calls":[…], "replies":[…]}`, the turn's model calls as its record kept them, each body as base64 so its bytes arrive as kept (`top`, each piece's `bytes`, a reply's `message`), a piece's `bytes` left out when the server holds it. Answers `{"stored": n, "missing":[…]}`: send the calls whose pieces are missing again, with those pieces' bytes. Each call must be of the session control assigned the job. A runner that holds no secret (`-gateway server`, the default) never sends calls: the relay recorded each one (below). |
| `POST /api/v0/runner/attempts/<id>/record` | A runner of a server, while the turn runs and at its end: `{"number": n, "events":[…]}`, at most 500, the turn's events from its own record. The server's record keeps each once, by its id, without its `folder` (the runner's path). A runner that holds no secret leaves out every event of a model call (one with an `exchange`) except `tool.refused` and `replay.failed`, which only its own gateway makes: the relay recorded the rest. One rule decides who records a call: the server records every call its relay carries, and only those. A call that never reached the server reached no vendor: it stays in the runner's own record and log. The server stores a turn's `session.closed` only after the relay has recorded the turn's calls, each counted from its arrival, before its credential is read. It waits at most 5 s, then stores it with `unsettled: true`. Every event must be of the session control assigned the job: a turn job's session, or for a review the one control named when it made the job (`links.review_session`). |
| `POST /api/v0/runner/blobs` | `{"hashes":[…]}`, at most 10,000: answers `{"missing":[…]}`, the ones the store lacks, so only those are sent. |
| `PUT /api/v0/runner/blobs/<hash>` | A file's content, streamed, up to 4 GiB: kept only when its SHA-256 is `<hash>`. 204. |
| `GET /api/v0/runner/blobs/<hash>` | A file's content, streamed. Only a blob of the runner's own jobs (below); any other gets 403. |
| `POST /api/v0/runner/packs` | Many blobs in one body, a **pack**: frames of `<hash> <size>\n` and then that many bytes, at most 1,000 blobs and 8 MiB of them. Each is checked against its hash. `{"kept": n}`. |
| `POST /api/v0/runner/packs/fetch` | `{"hashes":[…]}`, 1 to 1,000: their blobs as one pack, in that order, streamed. 404 with `{"missing":[…]}` when one is not held. 403 when one is not of the runner's own jobs. |

**A runner reads only its own jobs' blobs.** Both reads answer only blobs reached from the jobs of
each session the runner holds an attempt of that has not ended or ended in the last 10 minutes,
and from its own jobs of no session: their snapshots (the trees and every file in them, the times
blob, `.git`), their changesets and the files those name, and the session's own snapshot. Its
apply and snapshot jobs are its jobs too. Any other blob gets **403** `this runner
holds no job that reads this blob`, whether the store holds it or not. Sending blobs and asking
which are missing are not limited.


**The relay records every call it carries.** A runner that holds no secret sends each model call
of its agent to the server's relay, `/relay/<anthropic|chatgpt|cloudcode|openrouter>/…`, with the
attempt's token as its credential. The relay checks the token, adds the real credential, and
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

The runner's own routes carry no runner id: its token names the runner. Every call after take
names its attempt id and number. A call for an attempt that is not current
gets 409 with `stale: true`, and the runner stops that turn.

**One program per machine.** A runner holds its machine's folders and runs that machine's
sessions. It reports its machine's id as `capabilities.connector` (`con_` and 16 hex digits, kept
in `folders.json` in its data folder; the name is kept from before 0.5) and the folders it holds
as `capabilities.folders`, by reference. **A job that names a machine (`spec.connector`) goes to
that machine's runner and no other**, so a session runs on the machine that holds its folder for
its whole life: its copy is made there from the folder, by clone or overlay (`sessions/<session>`
in the runner's data folder; copies made before 0.4 stay in `worktrees/job_<id>`), its turns run
there, and its changes are applied there. A next job of a session goes to the
session's runner. One exception keeps a session made before 0.5 going: a session whose first turn
another machine's runner built from a snapshot stays with that runner; its applies still go to the
folder's machine.

A runner takes its jobs in two kinds of places. Turns and reviews take its `capacity`.
**Folder jobs** (`apply`, `folder`, and a `snapshot` stored before 0.5) take one of 4 folder places
of their own, asked for with `take`'s `folders: true`, so an apply never waits behind running
turns. A `folder` job names a path a person typed for that machine (`spec.folder`): the runner
checks that it is a directory it can read, registers it, and answers its reference in
`result.folder_ref`, or a one-sentence `refused`. It reports its folders again at once. Typing the
folder is the grant for that session. The server never stores the path: it hands it out with the
job from memory, so a job taken after the server restarted has none and is refused. One no runner
took within 15 seconds ends. Control (the server) never reads or writes a folder; it knows one by
its reference. File data goes through the server's blob store, by the hash of its content:

- At the end of each turn the runner puts the copy's changeset (what it holds that differs from
  its base) and the files it names in the store; the job's `result.changeset` names it.
- An apply job goes to the runner of the folder's machine, which reads both changesets and the
  files from the store (on that machine they are already in its own store), checks the folder and
  writes it.
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
agentbus up                                   # control and the machine's runner, one process
agentbus server                               # control alone
agentbus runner -server URL [-folder DIR]...  # the one program of any other machine
```

`agentbus connector` was retired in 0.5: it prints the `agentbus runner` line to run instead, with
its own flags, changes nothing and exits 2. A runner started on the connector's data folder keeps
the machine's id and its folders' references, so the sessions and applies that name them reach it.
The old connector stays listed in `GET /api/v0/runners` only while no runner reports its machine.

In `agentbus up` control and the runner share the server's blob store on disk: nothing is sent
over HTTP. A runner of a server keeps its own store in its data folder (`blob-cache/`, apart from a server's `blobs/`) and sends
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
