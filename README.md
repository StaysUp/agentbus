<div align="center">

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/agentbus-logo-dark.svg">
  <img alt="AgentBus" src="docs/assets/agentbus-logo-light.svg" width="360">
</picture>

**Control every AI agent in a single place.**

[![GitHub stars](https://img.shields.io/github/stars/staysup/agentbus?style=flat)](https://github.com/staysup/agentbus/stargazers)
[![Latest release](https://img.shields.io/github/v/release/staysup/agentbus)](https://github.com/staysup/agentbus/releases)
[![CI](https://github.com/StaysUp/agentbus/actions/workflows/ci.yml/badge.svg)](https://github.com/StaysUp/agentbus/actions/workflows/ci.yml)

[Quickstart](#quickstart) &nbsp;&bull;&nbsp; [Demo](#demo) &nbsp;&bull;&nbsp; [Cockpit](https://github.com/staysup/agentbus-cockpit) &nbsp;&bull;&nbsp; [Docs](#docs) &nbsp;&bull;&nbsp; [Releases](https://github.com/staysup/agentbus/releases)

</div>

![The AgentBus cockpit: a session, and one turn's model calls](docs/assets/cockpit-session.png)

AgentBus sits between your coding agents and every model vendor. It runs the agents you already
have (Claude Code, codex, Antigravity), signed in as you, and records every step.

- **Your agents work together.** Claude can ask codex, or any agent from any vendor, to answer a
  question, take on a task or check its work, through the same API you use. The answer comes back
  into its conversation.
- **You see every model call**, and what it cost. Each request and reply is stored on your machine.
- **The conversation is kept.** Each turn is a fresh agent run. AgentBus keeps the history and
  continues the conversation from its own record, on any agent or model.
- **Your folder stays as it is.** Each session works in its own copy. Its changes reach your folder
  only when you apply a turn.

## Demo

[![AgentBus demo](https://img.youtube.com/vi/si0HHQFcCD0/maxresdefault.jpg)](https://youtu.be/si0HHQFcCD0)

## Install

You need macOS or Linux, and `claude`, `codex` or `agy` installed and signed in. Pick one:

```sh
brew install staysup/tap/agentbus
curl -fsSL https://raw.githubusercontent.com/staysup/agentbus/main/install.sh | sh   # into ~/.local/bin
go install github.com/staysup/agentbus/cmd/agentbus@latest                          # Go 1.27.1 or later
```

The install script takes `AGENTBUS_INSTALL_DIR` (another folder) and `AGENTBUS_VERSION` (another
release). Check the install with `agentbus version`.

## Quickstart

```sh
agentbus up
```

This runs everything on this machine and listens on `http://127.0.0.1:8787`. The token that guards
the API is the file `token` in its data folder (`~/.local/share/agentbus`, or `$AGENTBUS_HOME`).

Then open a cockpit: [staysup/agentbus-cockpit](https://github.com/staysup/agentbus-cockpit). Follow
its README and paste the token when it asks. `agentbus up -h` lists the options.

## Several machines

AgentBus has two parts:

- **Control** is the one place that keeps everything: the record of every turn, the queue, your
  settings and agents, and the API the cockpit uses. It does no work itself.
- **A runner** does the work. Each machine that should run agents runs one. It asks control for
  work, makes a copy of the folder for each session, runs the agents there, and applies the changes
  to your folder when you say so.

`agentbus up` is both, in one program, for one machine. For several machines, run control once,
reachable from the others (`agentbus server -listen 0.0.0.0`), and a runner on each machine.

**Every runner must reach control's address.** That works on one network, over a VPN such as
Tailscale, or with control on a server the machines can reach. There is no hosted relay yet.

To add a machine:

1. Install AgentBus on it (see Install above).
2. Run `agentbus login http://<control>:8787`. It prints a code.
3. In the cockpit, open **Runners** (at the foot of the Agents column), type the code and press
   **Approve**.

The runner then keeps running in the background, also after a restart (a launchd agent on macOS, a
systemd user service on Linux). The Runners page shows the machine as **Online**. When you start a
session, you pick the machine, then a folder on it; the session stays on that machine.

![The Runners page: a machine waiting for approval, and the code field](docs/assets/cockpit-runners.png)

By default a runner holds no secrets: control adds the sign-in or API key to each model call.
Connect your subscriptions once in the cockpit (Agents, the agent's Connect step, then **Turn on for
my runners**). Control and every runner must run the same release. `agentbus login -h` lists the
options.

## Settings

Control keeps the settings in `settings.db`: the permissions every turn runs under and the
instructions every turn gets. Change them on the cockpit's Settings page, or with
`PUT /api/v0/settings/permissions` and `PUT /api/v0/settings/instructions`. A change applies to the
next turn. Runners read control's settings before each turn. The keys and their defaults are in
`/docs`, under Settings. In short:

- `read_folders`, `allow`, `forbid`, `edit`, `subagents`: what a turn may read, run and edit
  without asking. `forbid` reads command text only; it is not a sandbox.
- `isolation`: how a session's copy is made (`auto`, `overlay`, `copy`, `git` or `none`).
- `[limits]` and `[agent_api]`: what agents may ask other agents for.
- `[network]`: on Linux, close agents off to a list of hosts. Off by default.
- `[sandbox]`: a wall around each session's agent, on macOS and Linux runners. Off by default.

## Known limits

- OpenRouter turns show a cost of $0: the price table has no OpenRouter prices yet.
- A session stays on the machine that holds its folder.
- Network control is Linux only. On macOS, `contain = "on"` refuses every turn unless the sandbox
  is on.
- On ext4 a copy is a full copy. A clone (APFS) or a reflink (xfs, btrfs) is used where the disk
  can make one.
- An asked agent works in a fresh copy of your folder. It reads the asking turn's changes through
  the API, but does not have those files.
- agy runs on its own Google sign-in only, and gets no instructions of AgentBus's. A turn with
  `forbid` set, or `subagents = false`, is refused on agy. Using agy's sign-in on runners is at your
  own risk with Google: its terms (clause 6) forbid third-party tools, and it stays off unless you
  set `subscriptions_on_runners = "own"`.
- The API as tools (MCP) is off by default: `[agent_api] tools = true` turns it on for claude and
  codex.

## Docs

The running service describes its own API, without the token or the internet:

- `http://127.0.0.1:8787/docs`: the API reference, as a page.
- `http://127.0.0.1:8787/api/v0/openapi.json`: the same, as OpenAPI 3.1. It is the contract.
  Generate a client from it in any language.
- `http://127.0.0.1:8787/docs/llms.txt`: one short page for coding agents.

How runners talk to control is in [docs/RUNNER_PROTOCOL.md](docs/RUNNER_PROTOCOL.md). Third-party
code in the binary is listed in [THIRD_PARTY.md](THIRD_PARTY.md).

## License

Free to use, change and run, including inside your business. You can't sell it as a competing product or service. Each release becomes Apache 2.0 two years after it is published.

Licensed under the [Functional Source License](https://fsl.software) (FSL-1.1-ALv2). See [LICENSE](LICENSE).
