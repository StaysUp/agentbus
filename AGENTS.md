# AGENTS.md: working in this repository

For people and coding agents who change AgentBus. `CLAUDE.md` is a symlink to this file, so every
agent reads the same text. Keep it a symlink.

## What this repository is

The AgentBus framework: the `agentbus` command, the gateway, the adapters, the store, own replay and
the client API. The Go module is `github.com/staysup/agentbus`. It ships no client SDKs, no programs
and no screens. Cockpits and programs are clients of the API and live in their own repositories.

## Build and test

You need Go 1.27.1 or later.

```sh
go build ./cmd/agentbus && ./agentbus version
```

A source build prints `agentbus dev` and the commit Go read from git, with `, modified` when the
tree had changes. Release builds set the version and commit through the ldflags in
`.goreleaser.yaml` (`main.version`, `main.commit`).

Releases build with `CGO_ENABLED=0`; a plain `go build` with gcc installed uses cgo. Both must run
`up` and `runner` in their own user namespace on Linux. The namespace child must clear its ambient
capabilities on every thread before it starts an agent. Go's `AllThreadsSyscall` cannot do that in a
cgo build, so `cmd/agentbus/userns_cgo_linux.go` clears them in a C constructor, before Go starts a
second thread, and `clearAmbient` reads every thread's set back in both builds.

Overlay mounts belong to the runner that made them. A runner unmounts every overlay in its workdir
when it stops, and again when it starts (what a crash left); a session's next turn mounts its copy
again. A test that opens a copy must close it, or stop its runner, or the temp folder cannot go.

## Tests and the gate

```sh
make test   # unit tests, a few seconds: use this while you work
make gate   # gofmt, go vet, every test with -race, the build: run before every commit
```

Tests that start a child process or wait on real time (timeouts, sleeps, streams held open) carry
`//go:build integration` and run only in `make gate`. Put a new test of that kind in a
`*_integration_test.go` file. Helpers both kinds use stay in untagged files.

CI runs `make gate` on Linux and macOS, and it must pass before a change merges to `main`.

The gate runs gofmt on `git ls-files`, not `gofmt -l .`. A local `_references/` folder (git-ignored)
may hold other projects' Go code. Go's own tools skip folders that start with `_`; gofmt does not.

## Rules of the code

- **Headers never reach an event, a log or a file.** They carry the user's sign-in. Adapters get
  bodies only, and `gateway/gateway_test.go` asserts it. One exception: the User-Agent's first
  token, when shaped `name/version`, becomes the event's `client`.
- **Error text is fixed wording.** Never pass Go's own error message on: it can quote a header. A
  binary that embeds the gateway sends Go's global logger nowhere, since net/http writes to it.
- **The sign-in never leaves the user's machine.** The service listens on loopback by default.
  On another address, such as `0.0.0.0` in a container, the token guards the API, the agents are
  still handed a loopback URL, and the vendor paths answer callers on this machine only.
- **An adapter is per API format, not per agent.** Record a vendor quirk with the date and the
  agent version it was seen on, in a comment next to the code and in a test with that exact shape.
- **Own replay covers both agents.** `replay/` continues Claude and codex sessions from the store.
  A change to how history is built needs a test for each agent.
- **A turn that cannot start stores nothing.** If the agent's program is missing, the API answers
  HTTP 400 with how to install it. Keep that check before anything is written.
- **The client API describes itself.** Routes and types in `api/routes.go` generate the OpenAPI
  description. Change the route table, not a hand-written spec. `docs/CLIENT_API.md` is the
  contract in words; keep it in step.
- **One program, one flow.** `agentbus up`, `server`, `runner` and `connector` are modes of one
  binary. Every turn is a job, and every runner and connector, the built-in ones too, takes jobs
  through the runner protocol over HTTP. A runner starts a turn through `run/`, never through the
  API.
- **Three roles, and control never touches a folder.** Control (`api/`, `jobs/`, `queue/`,
  `agents/`, `store/`, `gateway/`, `replay/`, `settings/`) knows a person's folder by its reference
  only. The folder side (`folders/`, `connector/`) is the only code that reads or writes the
  person's folder: snapshots out, applies in. The runner side (`runner/`, `run/`, `isolation/`)
  works in copies and never imports the folder side; on one machine the command wires them
  through interfaces (`isolation.Lender`, `runner.FolderSide`). File data goes through control's
  blob store (`blobs/`), never from a connector to a runner. Two tests hold it: no control package
  imports the folder or runner side, and no control package calls a function that touches files
  or starts a program (`os.Stat`, `os.OpenRoot`, `filepath.EvalSymlinks`, `exec.Command` and the
  like), but the few its own data folder needs, listed in `controlAllowed`. That list only
  shrinks: an entry no call needs fails the test.
- **The package rule is tested** (`cmd/agentbus/layers_test.go`): `harness/` and `settings/`
  import nothing of the module, so every package may use them; `gateway/`, `adapter/`,
  `event/`, `cost/`, `replay/`, `store/`, `body/` and `logfile/` import nothing from `run/`,
  `workspace/`, `queue/`, `jobs/` or `runner/`; `run/` imports nothing from the queue side;
  `agents/` (the saved agents) may use `run/`, never the queue side or `workspace/`;
  `isolation/` (a session's copies) may use `workspace/` and `blobs/` only, and the runner uses it;
  `connector/` uses `folders/`, `blobs/`, `workspace/` and the queue's types, nothing of the runner
  side or the record; `blobs/` and `folders/` import nothing of the module;
  `queue/` may use `run/` and `workspace/`, never the reverse.
- **Isolation goes through `isolation.Backend`.** A session's copy is opened, checkpointed,
  diffed and closed by its backend, never by git calls or file copies elsewhere. Its changes go
  out as a changeset (`isolation.ChangesetOf`); applying them is the connector's. A new
  backend (a VM) is one more entry in its registry; nothing above it changes.
- **Settings go through `settings.Store`.** The policy, the instructions and the data folder's
  protected files are read and written through the store, never by a path. `settings.Files` is
  the data folder's files; `settings.Remote` is a runner's, read from its server. A database store
  is one more implementation.
- **Fix the root cause.** No defensive backstops on top of a fix.
- **A live run beats a fixture.** Vendors change their wire formats. Check a change against a real
  agent when it touches an adapter or the gateway.

## Adding a harness

A harness is a program that runs an agent's loop, such as `claude` or `codex`. Everything that
differs per harness lives in `harness/`, one file each, and the rest of the code asks the registry
(`harness.Get`, `harness.All`). No other package branches on a harness name.

1. Write `harness/<name>.go` with a type that implements `harness.Harness`: its `Info` (name,
   label, what to install, the sign-in status command, the endpoint variable, the access it offers,
   its replay format), its `Wire` and `WireOpenRouter`, `Edit`, `OwnInstructions`, `Args` and
   `ResumeID`. Note the version and date each flag was checked on, next to the code.
2. Add it to `registry` in `harness/harness.go`. A harness listed as coming but not runnable goes
   in `planned` instead, as opencode does.
3. Add its cases to `cmd/agentbus/launch_integration_test.go`, run it with `-update-launch`, and
   read the diff of `testdata/launch.golden` line by line. Existing lines must not change.
4. Own replay needs a history format in `replay/`. Without one, leave `Info.Replay` empty: its
   sessions continue by the vendor's resume. The gateway's `forbid` guard reads shell commands by
   API format (`gateway/guard.go`), so check that it sees the new harness's shell tool.

## Changes and review

- Every change is reviewed by someone other than its author before it lands on `main`.
- Keep commits small, with a message that says what changed and why.
- **When one area keeps failing review, replace the mechanism.** If two review rounds in a row find
  new problems in the same area, and some come from the last fix, stop patching. Ask what single
  rule would make every one of those cases impossible, build that, and add a test for each earlier
  finding. (Step 4 of 0.3: six rounds of case fixes to record ownership ended in one round once
  "control assigns the session to the job" replaced them.)
- New dependencies need a reason in the commit message. Third-party code shipped in the binary is
  listed in `THIRD_PARTY.md`.

## License

AgentBus is source-available under the Functional Source License, FSL-1.1-ALv2 (see `LICENSE`).
Each release becomes Apache 2.0 two years after it is published. Contributions are accepted under
the same license.
