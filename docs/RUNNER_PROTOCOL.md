# The runner protocol

How runners and connectors talk to control. The client API, for people, programs and agents, is in
[CLIENT_API.md](CLIENT_API.md).


`agentbus runner -server <url>` is a runner; so is the one built into `agentbus up`. Both speak this
protocol over HTTP. A runner pulls work and is never called, so it works behind NAT. These calls take the enrollment
token (`enroll_token` in the data folder) or the runner's own token, never the main token. The
cockpit never calls them.

All of it is under `/api/v0/runner/`. A running service describes it at
`/api/v0/runner/openapi.json`, without the token, and shows it at `/docs/runner`. It is apart from
the client API on purpose: **it is our binaries' own protocol and not a public promise.** It may
change in any release until third parties write runners. This is protocol version 2: the routes
moved under `/api/v0/runner/` in 0.4, and a runner of an older build is refused at register.

**Words.** A **job** is the queue's word for work: a turn, a review, an apply, a snapshot or a
command. A
runner takes a job and runs it as an **attempt**. A turn or review job names its turn id when it is
made (`turn` on the job). The runner starts the turn with that id, whichever attempt runs it, so
the turn a client asked for keeps its id. The client API never shows jobs or attempts.

| Call | What it does |
|---|---|
| `POST /api/v0/runner/register` | Register with name, labels, capacity and protocol 2, with the enrollment token. Returns the runner and its token, once. |
| `POST /api/v0/runner/take` | Long poll, up to 30 s: a job with its new attempt and lease, or a cleanup. 204 when there is nothing. |
| `POST /api/v0/runner/heartbeat` | Idle heartbeat with `capacity`. A restarted runner sends it to check the identity it saved. |
| `POST /api/v0/runner/removed` | Report a closed session's copy removed, or `error`. |
| `GET /api/v0/runner/settings` | The policy and the instructions, read fresh, for the turn it is about to start. |
| `POST /api/v0/runner/attempts/<id>/start` | Taken to running. `cancel` says the job was stopped. `token` is the attempt's turn token, minted by the server: its agent's API token and model credential, working while the attempt is current. |
| `POST /api/v0/runner/attempts/<id>/heartbeat` | Renew the 30 s lease. Sent every 2 s. `cancel` says stop. |
| `POST /api/v0/runner/attempts/<id>/events` | A batch numbered from `offset`. A repeat stores nothing new; a gap gets 409 with `next`. |
| `POST /api/v0/runner/attempts/<id>/finish` | `status`, `reason`, `result`, `last_seq`. The same finish again gets the same answer. |
| `POST /api/v0/runner/attempts/<id>/snapshot` | For a first turn in an isolated copy of a folder on another machine: the snapshot job, made once, on the folder's connector. Ask again until `snapshot` is set. |
| `POST /api/v0/runner/attempts/<id>/calls` | A runner of a server, while the turn runs: `{"number": n, "calls":[…], "replies":[…]}`, the turn's model calls as its record kept them, each body as base64 so its bytes arrive as kept (`top`, each piece's `bytes`, a reply's `message`), a piece's `bytes` left out when the server holds it. Answers `{"stored": n, "missing":[…]}`: send the calls whose pieces are missing again, with those pieces' bytes. Each call must be of the session control assigned the job. |
| `POST /api/v0/runner/attempts/<id>/record` | A runner of a server, while the turn runs and at its end: `{"number": n, "events":[…]}`, at most 500, the turn's events from its own record. The server's record keeps each once, by its id, without its `folder` (the runner's path). Every event must be of the session control assigned the job: a turn job's session, or for a review the one control named when it made the job (`links.review_session`). |
| `POST /api/v0/runner/blobs` | `{"hashes":[…]}`, at most 10,000: answers `{"missing":[…]}`, the ones the store lacks, so only those are sent. |
| `PUT /api/v0/runner/blobs/<hash>` | A file's content, streamed, up to 4 GiB: kept only when its SHA-256 is `<hash>`. 204. |
| `GET /api/v0/runner/blobs/<hash>` | A file's content, streamed. |
| `POST /api/v0/runner/packs` | Many blobs in one body, a **pack**: frames of `<hash> <size>\n` and then that many bytes, at most 1,000 blobs and 8 MiB of them. Each is checked against its hash. `{"kept": n}`. |
| `POST /api/v0/runner/packs/fetch` | `{"hashes":[…]}`, 1 to 1,000: their blobs as one pack, in that order, streamed. 404 with `{"missing":[…]}` when one is not held. |

The runner's own routes carry no runner id: its token names the runner. Every call after take
names its attempt id and number. A call for an attempt that is not current
gets 409 with `stale: true`, and the runner stops that turn.

**Three roles.** A runner runs agents in copies, each named after its session
(`sessions/<session>` in its data folder; copies made before 0.4 stay in `worktrees/job_<id>`). A **connector** owns the folders of its machine:
it enrolls like a runner, with `capabilities.role` `connector` and its `folders`, and takes only
`snapshot`, `apply` and `folder` jobs of those folders. A `folder` job names a path a person typed
(`spec.folder`): the connector checks that it is a directory it can read, registers it, and
answers its reference in `result.folder_ref`, or a one-sentence `refused`. It reports its folders
again at once. The server never stores the path: it hands it out with the job from memory, so a
job taken after the server restarted has none and is refused. One no connector took within 15
seconds ends. Control (the server) never reads or writes a folder;
it knows one by its reference. File data always goes through the server's blob store, by the hash
of its content, never from a connector to a runner:

- A first turn in an isolated copy of a folder goes to any runner. A runner on the folder's machine
  (one that reports the folder's connector: the built-in one of `agentbus up`, or `agentbus runner
  -folder`) copies the folder by clone or overlay, as before. Any other asks for a snapshot
  (`POST /api/v0/runner/attempts/<id>/snapshot`): the connector hashes every file, puts each file and
  each content tree that the store lacks in it, and answers the snapshot id; the runner fetches
  the blobs it lacks into its own store and builds the copy from them. A folder used as it is
  (`isolate: false`) and a git worktree (`isolation = "git"`) run only on the folder's machine.
- At the end of each turn the runner puts the copy's changeset (what it holds that differs from
  its base) and the files it names in the store; the job's `result.changeset` names it.
- An apply job goes to the folder's connector, which reads both changesets and the files from the
  store, checks the folder and writes it.
- A command job (the person's `!`, `POST /api/v0/sessions/<id>/commands`) goes to the session's
  runner, in the session's lane with its turns: `spec.command_id`, `spec.command` and
  `spec.timeout_ms`. It waits for what runs in the session, a review of it included. The runner
  runs it in the session's copy with no turn and no token. Control stores `command.started` at its
  start. Its finish carries the `command.ran` event as `result.ran`, and control keeps it once the
  finish is accepted; one of another session or command, with a turn, or of another kind refuses
  the finish with **400**. A command job that ends without one gets `command.failed` from control.
- **Outcomes go to control's record, the job keeps a pointer** (from 0.5). Once control accepts a
  finish, it stores what the job did as an event and replaces it in the job's result with that
  event's id: a turn's `result.changes` becomes `turn.changed` (`result.changes_event`, and
  `result.checkpoint` keeps the end checkpoint the next turn starts from), an apply's
  `result.apply` and `result.refused` become `turn.applied` (`result.applied_event`), and a
  command's `result.ran` is kept once as `command.ran` (`result.ran_event`). A finish sent again
  is answered the same and stores nothing twice. A runner may not send `turn.changed` or
  `turn.applied` in a turn's record (**400**): they are control's own. A job from before 0.5 keeps
  its outcome in its result, and readers read it there.
- A connector that takes work asks for it about once a second. One not seen for a minute is gone:
  a turn that needs a snapshot of its folder is refused (`the folder's connector is not connected:
  start agentbus connector on the folder's machine, then send the turn again`), and so is an apply
  (503). A connector stopped while it takes a snapshot ends that turn: `the folder's connector
  stopped while it took the snapshot; send the turn again`.
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
agentbus up                                   # control, a connector and a runner, one process
agentbus server                               # control alone
agentbus runner -server URL [-folder DIR]...  # a runner; with -folder, a connector beside it
agentbus connector -server URL [-folder DIR]... # a connector alone: no agents, record or gateway
```

In `agentbus up` the three share the server's blob store on disk: nothing is sent over HTTP and
copies are made by clone or overlay (the local fast path). A runner or a connector of a server
keeps its own store in its data folder (`blob-cache/`, apart from a server's `blobs/`) and sends
only what the server lacks: blobs under
1 MiB go a pack at a time, four packs at once, and a bigger one alone by `PUT`. A runner fetches a
snapshot's trees a level at a time and its files a pack at a time.

**The blob store is cleaned.** The server keeps every blob that a snapshot or a changeset of an open
session reaches: every job's but those of a closed session. Once a session closes, the server
sweeps (also at start and every hour) and removes each blob nothing reaches that is older than an
hour. A blob just sent, or just answered as held by `POST /api/v0/runner/blobs`, counts as new, so a
snapshot still being sent loses nothing. A runner's or a connector's own store is a cache of the
server's: what it lacks it fetches again, so every blob in it older than an hour may go.

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
