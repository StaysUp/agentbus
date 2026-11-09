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
- **One program, one flow.** `agentbus up`, `server` and `runner` are modes of one binary. Every
  turn is a job, and every runner, the built-in one too, takes jobs through the runner protocol
  over HTTP. A runner starts a turn through `run/`, never through the API.
- **The package rule is tested** (`cmd/agentbus/layers_test.go`): `gateway/`, `adapter/`,
  `event/`, `cost/`, `replay/`, `store/`, `body/` and `logfile/` import nothing from `run/`,
  `workspace/`, `queue/`, `jobs/` or `runner/`; `run/` imports nothing from the queue side;
  `agents/` (the saved agents) may use `run/`, never the queue side or `workspace/`;
  `queue/` may use `run/` and `workspace/`, never the reverse.
- **Fix the root cause.** No defensive backstops on top of a fix.
- **A live run beats a fixture.** Vendors change their wire formats. Check a change against a real
  agent when it touches an adapter or the gateway.

## Changes and review

- Every change is reviewed by someone other than its author before it lands on `main`.
- Keep commits small, with a message that says what changed and why.
- New dependencies need a reason in the commit message. Third-party code shipped in the binary is
  listed in `THIRD_PARTY.md`.

## License

AgentBus is source-available under the Functional Source License, FSL-1.1-ALv2 (see `LICENSE`).
Each release becomes Apache 2.0 two years after it is published. Contributions are accepted under
the same license.
