# Contributing to AgentBus

AgentBus runs the coding agents you already have and tracks every step. It sits between your
agents and the model vendors, keeps a record of each turn and its cost, and runs each session in
its own copy of your folder.

## Build and run it

You need Go 1.27.1 or later, and `claude`, `codex` or `agy` installed to run a real turn.

```sh
go build ./cmd/agentbus && ./agentbus version
```

Start it and open the cockpit:

```sh
./agentbus up -open
```

The Quickstart in [README.md](README.md) walks a first run from nothing to two vendors talking
about the same work.

## Run the tests

Use the fast unit tests while you work:

```sh
make test
```

Run the full gate before you commit. CI runs the same steps on Linux and macOS:

```sh
make gate
```

The gate runs gofmt on tracked files, `go vet` with and without the integration build tag, every
test with `-race`, and the build. Tests that start a child process or wait on real time carry the
`integration` build tag and run only in the gate. Put a new test of that kind in a
`*_integration_test.go` file.

## House rules

- One change per pull request. Keep it small.
- A fix comes with a test that fails before the fix and passes after it.
- `make gate` passes, which includes gofmt and `go vet`. Format with `gofmt -w` before you commit.
- The pull request says what you changed, why, and what you ran to check it.
- A new dependency needs a reason in the commit message. Code shipped in the binary is listed in
  [THIRD_PARTY.md](THIRD_PARTY.md).
- Read the code rules in [AGENTS.md](AGENTS.md) before a change that touches the gateway, a runner,
  or an adapter.

## Adding a harness or an adapter

Two extension points have their own package docs, and they are the place to start:

- **A harness** is a coding agent that AgentBus runs. The "To add a harness" paragraph in
  [`harness/harness.go`](harness/harness.go) lists the steps: a type that implements `Harness`, an entry in the
  registry, and its cases in the launch test.
- **An adapter** knows one vendor's API format and turns its requests and replies into events. The contract is
  stated in [`adapter/adapter.go`](adapter/adapter.go).

## Pick a task

Issues marked `good first issue` are small and self-contained, with a starting point in the code.
Pick one, say on the issue that you are taking it, and open a pull request when it is ready.

## License

AgentBus is source-available under the Functional Source License, FSL-1.1-ALv2. Each release
becomes Apache 2.0 two years after it is published.

By contributing, you agree that your contribution may be released under the FSL-1.1-ALv2 and also
under the MIT License.
