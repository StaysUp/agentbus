<div align="center">

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/agentbus-logo-dark.svg">
  <img alt="AgentBus" src="docs/assets/agentbus-logo-light.svg" width="360">
</picture>

**Control every AI agent in a single place.**

[![GitHub stars](https://img.shields.io/github/stars/staysup/agentbus?style=flat)](https://github.com/staysup/agentbus/stargazers)
[![Latest release](https://img.shields.io/github/v/release/staysup/agentbus)](https://github.com/staysup/agentbus/releases)
[![CI](https://github.com/StaysUp/agentbus/actions/workflows/ci.yml/badge.svg)](https://github.com/StaysUp/agentbus/actions/workflows/ci.yml)

[Quickstart](#quickstart) &nbsp;&bull;&nbsp; [Demo](https://youtu.be/si0HHQFcCD0) &nbsp;&bull;&nbsp; [Cockpit](https://github.com/staysup/agentbus-cockpit) &nbsp;&bull;&nbsp; [Docs](#docs) &nbsp;&bull;&nbsp; [Releases](https://github.com/staysup/agentbus/releases)

</div>

[![AgentBus demo](https://img.youtube.com/vi/si0HHQFcCD0/maxresdefault.jpg)](https://youtu.be/si0HHQFcCD0)

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

## Install

You need macOS or Linux, and `claude`, `codex` or `agy` installed. Use **one** of these:

**Homebrew** (macOS or Linux):

```sh
brew install staysup/tap/agentbus
```

**Install script** (puts the binary in `~/.local/bin`):

```sh
curl -fsSL https://raw.githubusercontent.com/staysup/agentbus/main/install.sh | sh
```

**From source** (Go 1.27.1 or later):

```sh
go install github.com/staysup/agentbus/cmd/agentbus@latest
```

The install script takes `AGENTBUS_INSTALL_DIR` (another folder) and `AGENTBUS_VERSION` (another
release). Check the install with `agentbus version`.

## Quickstart

A first run, from nothing to two vendors talking about the same work. You need `claude` and `codex`
installed, and a Claude and a ChatGPT subscription.

1. **Start AgentBus.**

   ```sh
   agentbus up
   ```

   It prints a sign-in link, `http://127.0.0.1:8787/?t=…`, and opens it in your browser. The
   cockpit comes with `agentbus up`: nothing else to install. The link works once, for 10 minutes.
   No browser opened, or the link expired? Stop `agentbus up` (Ctrl-C) and start it again for a
   new link.

   Signing in from another tab or browser: the cockpit's sign-in lasts for the tab that opened the
   link. In a new tab or browser, either start `agentbus up` again for a new link, or paste your
   token on the connect screen. The token is the file `token` in the data folder
   (`$AGENTBUS_HOME`, by default `~/.local/share/agentbus`). Copy it without showing it:
   `pbcopy < ~/.local/share/agentbus/token` on a Mac, or `xclip -sel c < …/token` on Linux.
   Anyone with the token controls your agents, so keep it to yourself.

2. **Add your agents.** A new AgentBus starts with no agents. In the cockpit, press **+ Agent**
   at the top of the Agents column, pick the tool, Claude Code, and press **Create agent**. Do the
   same for Codex.

3. **Connect your subscriptions.** Open **Settings** (at the foot of the Agents column), then
   **Subscriptions**. Press **Connect** next to claude, and sign in as the tool asks. Do the same
   for codex. You do this once.

4. **Ask claude something.** Back in the cockpit (**Cockpit** at the top of Settings), press
   **New session**. Pick Claude Code as the agent, type the path of a project folder, write a first
   message such as "what does this project do?", and press **Start**.

5. **Ask codex about it.** In the same conversation, switch the agent box beside Send to Codex, and
   send "what do you think about above?". Codex reads Claude's answer and gives its own.

Every turn and every model call is now in the record, with its cost. `agentbus up -h` lists the
options.

### More on subscriptions

Every turn uses the sign-in connected in Settings, on one machine as on many: a tool's own sign-in
on the machine (`~/.claude`, `~/.codex`) is not used, and each session runs in a home of its own. A
turn whose tool is not connected is refused with "connect your claude subscription in Settings".

For OpenRouter agents, paste your OpenRouter key once in the same place (Subscriptions,
OpenRouter key, **Save**). Control keeps it beside the subscriptions, readable by you only, and adds
it to each OpenRouter call. It is never shown again, logged, or sent to a runner or an agent.
**Replace** and **Remove** change it. An agent saved before this can still read its key from an
environment variable where control runs (`key_env`, `OPENROUTER_API_KEY` by default); a key set in
Settings wins. API: `PUT` and `DELETE /api/v0/subscriptions/openrouter/key`.

An agent can also use the vendor's own API key: access **API key** (`api`). Paste the key once in
the same place: an Anthropic API key for claude, an OpenAI API key for codex, a Gemini API key for
agy. Control keeps each like the OpenRouter key and adds it only to the model calls it records for
that vendor; any other call gets 403. The agent gets a placeholder in its place, so the key never
reaches a runner, an agent's environment, a log or the record. A turn while no key is set is refused with "set your Anthropic API key in Settings" (and
the like). agy on a Gemini API key needs no Google sign-in, so it also works with control on a Mac.
API: `PUT` and `DELETE /api/v0/subscriptions/{anthropic,openai,gemini}/key`.

## Several machines

AgentBus has two parts:

- **Control** is the one place that keeps everything: the record of every turn, the queue, your
  settings and agents, and the API the cockpit uses. It does no work itself.
- **A runner** does the work. Each machine that should run agents runs one. It asks control for
  work, makes a copy of the folder for each session, runs the agents there, and applies the changes
  to your folder when you say so.

`agentbus up` is both, in one program, for one machine. For several machines, run control once,
reachable from the others (`agentbus server -listen 0.0.0.0`), and a runner on each machine.

**Every runner must reach control's address.** That works on one network. A hosted relay, as a
managed service, is planned and not available yet.

To add a machine:

1. Install AgentBus on it (see Install above).
2. Run `agentbus login http://<control>:8787`. It prints a code.
3. In the cockpit, open **Runners** (at the foot of the Agents column), type the code and press
   **Approve**.
4. Run the command `agentbus login` prints after approval. It starts the runner, which works while
   that command runs.

The Runners page then shows the machine as **Online**. When you start a session, you pick the
machine, then a folder on it; the session stays on that machine.

![The Runners page: a machine waiting for approval, and the code field](docs/assets/cockpit-runners.png)

By default a runner holds no secrets: control adds the sign-in or API key to each model call.
Connect your subscriptions once in the cockpit (Settings, Subscriptions). Control and every runner must run the same release. `agentbus login -h` lists the
options.

## Settings

Control keeps the settings in `settings.db`: the permissions every turn runs under and the
instructions every turn gets. Change them on the cockpit's Settings page, or with
`PATCH /api/v0/settings/permissions` (only the keys you send), `PUT /api/v0/settings/permissions`
(the whole policy) and `PUT /api/v0/settings/instructions`. A change applies to the next turn. Runners read control's settings before each turn. The keys and their defaults are in
`/docs`, under Settings. In short:

- `read_folders`, `allow`, `forbid`, `edit`, `subagents`: what a turn may read, run and edit
  without asking. `forbid` reads command text only; it is not a sandbox. `subagents` is off by
  default: agents ask other agents through AgentBus, where the work is recorded.
- `isolation`: how a session's copy is made (`auto`, `overlay`, `copy`, `git` or `none`).
- `home_files`: files from your home, such as `~/.aws/config`, copied into each session's own
  home at the start of each turn, sandbox on or off. Before 0.5.2 this was `[sandbox] read`;
  the first start of 0.5.2 moves a stored value here, and the old name still reads.
- `[limits]`: how much agents may ask other agents for. `[agent_api]` has two keys: `tools` (the
  API as MCP tools) and `apply` (a turn's own token may apply changes; off by default). Within its
  own request tree a turn's token may always read, list agents, start, review, stop and rename.
  The switches for those were retired in 0.5.2: the first start removes stored ones, and an old
  file or body that holds them still reads.
- `[network]`: on Linux, close agents off to a list of hosts. Off by default.
- `[sandbox]`: a wall around each session's agent, on macOS and Linux runners. Off by default.

## Known limits

- OpenRouter turns show a cost of $0: the price table has no OpenRouter prices yet.
- A session stays on the machine that holds its folder.
- On Ubuntu 24.04 and later, AppArmor allows user namespaces only to a program with a profile, so
  the sandbox and `contain` need one. The install script offers it when it installs into a folder
  only root can change: `curl -fsSL https://raw.githubusercontent.com/staysup/agentbus/main/install.sh | sudo AGENTBUS_INSTALL_DIR=/usr/local/bin sh`.
  The profile is `packaging/apparmor/agentbus`, also in each release archive. The runner says what is
  missing. The fallback is the system setting `sudo sysctl kernel.apparmor_restrict_unprivileged_userns=0`,
  which turns the rule off for every program, so the profile is the better choice.
- Network control is Linux only. On macOS, `contain = "on"` refuses every turn unless the sandbox
  is on.
- On ext4 a copy is a full copy. A clone (APFS) or a reflink (xfs, btrfs) is used where the disk
  can make one.
- An asked agent works in a fresh copy of your folder. It reads the asking turn's changes through
  the API, but does not have those files.
- agy uses the Google sign-in connected once on control (Settings, Subscriptions), like the other
  tools; to connect agy, control must run on Linux, not on a Mac. agy gets no instructions of
  AgentBus's. A turn with `forbid` set is refused on agy. agy cannot turn its own subagents off, so `subagents = false`
  does not apply to it: its turn runs, and its `turn.started` says so (`unkept`). Using agy's sign-in on runners is at your
  own risk with Google: its terms (clause 6) forbid third-party tools. A sign-in connected on control
  is used on every approved runner; to keep agy's off your runners, do not connect it.
- The API as tools (MCP) is on by default for claude and codex: `[agent_api] tools = false` turns
  it off, and agents then call the API with `agentbus-api`.

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
