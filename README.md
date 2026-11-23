<div align="center">

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/agentbus-logo-dark.svg">
  <img alt="AgentBus" src="docs/assets/agentbus-logo-light.svg" width="360">
</picture>

**Control every AI agent in a single place.**

[![GitHub stars](https://img.shields.io/github/stars/staysup/agentbus?style=flat)](https://github.com/staysup/agentbus/stargazers)
[![Latest release](https://img.shields.io/github/v/release/staysup/agentbus)](https://github.com/staysup/agentbus/releases)
[![CI](https://github.com/StaysUp/agentbus/actions/workflows/ci.yml/badge.svg)](https://github.com/StaysUp/agentbus/actions/workflows/ci.yml)

[Quickstart](#quickstart) &nbsp;&bull;&nbsp; [Demo](https://github.com/user-attachments/assets/674686dc-394d-4431-9e0b-a48ef3d8ef9d) &nbsp;&bull;&nbsp; [Docs](#docs) &nbsp;&bull;&nbsp; [Releases](https://github.com/staysup/agentbus/releases)

</div>

[![AgentBus 0.5.9: Codex reviews Claude Code's change and finds a P1; every model call with its cost. Click to watch the 2-minute demo.](docs/assets/cockpit-059.png)](https://github.com/StaysUp/agentbus/releases/tag/v0.5.9)

AgentBus sits between your coding agents and every model vendor. It runs the agents you already
have (Claude Code, Codex, Antigravity, OpenCode or any OpenRouter model) and tracks every step.

- **Your agents work together.** Claude can ask Codex, or any agent from any vendor, to answer a
  question, take on a task or check its work. The answer comes back into the same session.
- **You see every model call**, and what it cost: exactly what each model was sent, piece by piece,
  and what it answered.
- **Your folder stays as it is.** Each session works in its own copy. You review the changes and
  apply them to your folder. Turn on the sandbox to make the copy a hard wall.
- **Everything stays on your machine.** Your sessions, every model call and reply, and your sign-ins
  and keys are kept in AgentBus's data folder on your machine.

## Install

You need macOS or Linux, and `claude`, `codex`, `agy` or `opencode` installed. Use **one** of these:

**Install script** (puts the binary in `~/.local/bin`):

```sh
curl -fsSL https://raw.githubusercontent.com/staysup/agentbus/main/install.sh | sh
```

**Homebrew** (macOS or Linux):

```sh
brew install staysup/tap/agentbus
```

**From source** (Go 1.27.1 or later):

```sh
go install github.com/staysup/agentbus/cmd/agentbus@latest
```

A source build has no cockpit. Use the install script or Homebrew to follow the Quickstart, or from a clone run `tools/pack-cockpit.sh` then `go build`.

The install script takes `AGENTBUS_INSTALL_DIR` (another folder) and `AGENTBUS_VERSION` (another
release). Check the install with `agentbus version`. If `~/.local/bin` is not on your PATH, the
script prints the exact line to add for your shell (zsh, bash or fish); it never edits a profile.

## Quickstart

A first run, from nothing to two vendors talking about the same work. You need `claude` and `codex`
installed, and a Claude and a ChatGPT subscription.

1. **Start AgentBus.**

   ```sh
   agentbus up -open
   ```

   It prints a sign-in link, `http://127.0.0.1:8787/?t=…`, and with `-open` opens it in your
   browser. Without `-open` it only prints the link. The cockpit comes with
   `agentbus up`: nothing else to install. The link works once, for 10 minutes. The link expired?
   Stop `agentbus up` (Ctrl-C) and start it again for a new one.

   In another tab or browser, paste your token on the connect screen instead. The token is the file
   `token` in the data folder that `agentbus up` prints when it starts (`$AGENTBUS_HOME`, by default
   `~/.local/share/agentbus`). Copy it
   without showing it: `pbcopy < ~/.local/share/agentbus/token` on a Mac, or
   `xclip -sel c < ~/.local/share/agentbus/token` on Linux. Anyone with the token controls your
   agents, so keep it to yourself.

2. **Add your agents.** A new AgentBus starts with no agents. Press **+ Agent** at the top of the
   Agents column, pick Claude Code, and press **Create agent**. Do the same for Codex.

3. **Connect your subscriptions.** Open **Settings** (at the foot of the Agents column), then
   **Subscriptions**. Press **Connect** next to Claude Code and sign in as it asks. Do the same for
   Codex. You do this once.

4. **Bring your setup (optional).** In **Settings**, **Config**, press **Use my config**, look over
   the list and save. Your settings, skills, MCP servers and logins such as `gh` and `aws` then come
   into every session.

5. **Ask Claude something.** Press **+ Session**, pick Claude Code, type the path of a project
   folder, write a first message such as "what does this project do?", and press **Start**.

6. **Ask Codex about it.** In the same session, switch the agent beside Send to Codex, and send
   "what do you think about the answer above?". Codex reads Claude's answer and gives its own.

Every turn and every model call is now in the record, with its cost.
By default (`-replay`), `agentbus up`, `server` and `runner` continue Claude and Codex sessions by sending the earlier turns' history through the gateway. Pass `-replay=false` when you want to use the vendor's own session resume instead.

### Sign-ins and keys

Every turn uses the sign-in connected in Settings. A tool's own sign-in on your machine
(`~/.claude`, `~/.codex`) is not used: each session runs in a home of its own. A turn whose tool is
not connected is refused with "connect your claude subscription in Settings".

An agent can also use an API key instead of a subscription. Paste the key once in Settings,
Subscriptions: an OpenRouter key for any OpenRouter model, or the vendor's own key (Anthropic for
Claude Code, OpenAI for Codex, Gemini for Antigravity). AgentBus keeps it, adds it only to that
vendor's model calls, and never shows it again or gives it to an agent. **Replace** and **Remove**
change it.

An agent reaches its model in one of three ways, its **access**:

- `subscription`: the subscription connected in Settings. The model is optional.
- `openrouter`: an OpenRouter key. The model is required and is written in OpenRouter's naming.
- `api`: the vendor's own API key set in Settings (Anthropic for Claude Code, OpenAI for Codex, Gemini
  for Antigravity). The model is optional.

OpenCode (`npm install -g opencode-ai`) runs on `openrouter` only.

### Use any model

Claude Code can run on any model OpenRouter offers: the agent still runs Claude Code, but the model
behind it changes. From an empty AgentBus:

1. **Add an OpenRouter key.** Open **Settings**, **Subscriptions**, and paste your OpenRouter key
   once. AgentBus keeps it and adds it only to OpenRouter model calls.
2. **Add a Claude Code agent.** Press **+ Agent**, pick Claude Code, name it, set access to
   `openrouter` and model to an id in OpenRouter's naming, such as `openai/gpt-oss-120b` (GPT) or
   `deepseek/deepseek-v4.1-flash` (DeepSeek), then press **Create agent**. `openrouter` needs a model.
3. **Start a session.** Press **+ Session**, pick the agent, type your project folder, write a first
   message such as "what does this project do?", and press **Start**.
4. **Have another model check it.** Add a second Claude Code agent the same way with a different
   model id. In the same session, switch the agent beside Send to it and send "check the work
   above". Its reply comes back into the session, as Codex's does in the Quickstart.

Claude Code's tools are tuned for Claude, so other models may behave differently; give clear steps
and read what they reply.

## Settings

Settings apply to every session, from the next turn. Change them on the cockpit's Settings page, or
through the API (`/api/v0/settings/permissions` and `/api/v0/settings/instructions`; the keys and
their defaults are in `/docs`, under Settings).

- **Permissions:** whether an agent may apply its own changes (off by default; you always may), then
  two lists by name, each in three parts: **Commands** and **Tools**. **Allow** runs without asking,
  **Ask** holds the call, and **Refuse** never runs it. Starting subagents and editing files are
  tools too (`subagent`, `edit`), so they are ruled like any other. On **Ask** the call is held for
  up to 60 seconds while a card above the message box waits for you: **Allow once** runs it in the
  same turn, **Always allow** runs it and allows it from then on, and **Refuse** or no answer in time
  returns a plain refusal. Refuse wins over Ask, Ask over Allow. **Anything else** decides a call on
  none of the lists. Agents reach the record, and apply and stop a turn, through `agentbus-api`.
- **Config:** files from your home copied into each session's own home at each turn: your
  settings, skills, MCP servers and logins. **Use my config** fills the list. Sign-ins are never
  copied.
- **Isolation:** each session works in its own copy of your folder, cloned copy-on-write where the
  disk supports it, so a big `node_modules` costs nothing to copy. To reach a file outside its copy,
  an agent uses an app that asks you once per folder (**Always for this folder** grants it from then
  on). The sandbox walls each session to its folder and its home, and network contain lets agents
  reach only the hosts you list. Both are off by default.
- **Limits:** how much work one message may cause: how deep agents may ask each other, how many
  turns, and the cost.

## Known limits

- Ask works for plain commands such as `git push origin main`. A Claude Code command chained with `;`
  or `&&`, or started with environment variables, is checked by Claude Code's own rules too, which
  can block it even after you allow it. AgentBus runs every command and tool call through one rules
  list, by name; the vendor's own rules still apply on top.
- On Ubuntu 24.04 and later, AppArmor allows user namespaces only to a program with a profile, so
  the sandbox and network contain need one. The install script offers it when it installs into a
  folder only root can change: `curl -fsSL https://raw.githubusercontent.com/staysup/agentbus/main/install.sh | sudo AGENTBUS_INSTALL_DIR=/usr/local/bin sh`.
  The profile is `packaging/apparmor/agentbus`, also in each release archive. AgentBus says what is
  missing. The fallback is `sudo sysctl kernel.apparmor_restrict_unprivileged_userns=0`, which turns
  the rule off for every program, so the profile is the better choice.
- On ext4 a copy is a full copy. A clone (APFS) or a reflink (xfs, btrfs) is used where the disk can
  make one.
- MCP servers in your config load on every turn, reviews included. On a Mac, a server that
  signs in with OAuth keeps its token in the Keychain, and macOS asks to let the session read it;
  deny it to keep that server off. What a server does is outside AgentBus's record.
- Antigravity cannot start with the sandbox on, on macOS: it opens a local port, and the macOS sandbox cannot
  limit a port to this machine, so AgentBus does not allow it. Turn the sandbox off to use Antigravity on a Mac.
  Linux runners are not affected. A fix that keeps the sandbox is planned.
- The sandbox hides your secret folders (`~/.ssh`, `~/.aws`, `~/.gnupg`, `~/.config/gh`,
  `~/.docker/config.json`, `~/.npmrc`, `~/.netrc`, the Keychains) and any `.env` outside the copy,
  even from a folder you grant for reading. On macOS this is enforced in full. On Linux the agent
  sees only what the wall binds in, so an ungranted secret is invisible, but a secret that sits
  inside a folder you grant is not yet hidden there. Grant narrowly on Linux until that lands.
- Claude's browser (Chrome) cannot run in AgentBus sessions: it needs the tool's own sign-in.
- Antigravity: use it with a Gemini API key (Settings, Subscriptions). Its Google sign-in does not
  work through AgentBus yet: before any work it checks your account with Google directly, outside
  AgentBus, and that check fails. It gets no instructions from AgentBus, a turn with forbidden
  commands set is refused on it, and it cannot turn off its own subagents.
- OpenCode runs on an OpenRouter key only; its own provider sign-ins are not used yet. It gets no
  instructions and no AgentBus tools from AgentBus (it reaches the API through `agentbus-api`), and
  its sessions continue by its own resume. A turn with forbidden or asked commands set is refused on
  it, and a command on none of the lists is refused rather than held for you. In a folder below the
  root of a git repository it runs without edits, since OpenCode would edit the whole repository.

## Docs

The running service describes its own API, without the token or the internet:

- `http://127.0.0.1:8787/docs`: the API reference, as a page.
- `http://127.0.0.1:8787/api/v0/openapi.json`: the same, as OpenAPI 3.1. It is the contract.
  Generate a client from it in any language.
- `http://127.0.0.1:8787/docs/llms.txt`: one short page for coding agents.

Running agents on more than one machine is in [docs/RUNNER_PROTOCOL.md](docs/RUNNER_PROTOCOL.md). Third-party
code in the binary is listed in [THIRD_PARTY.md](THIRD_PARTY.md).

To contribute, see [CONTRIBUTING.md](CONTRIBUTING.md), the [ROADMAP.md](ROADMAP.md), and
[SECURITY.md](SECURITY.md) for reporting a vulnerability.

## License

Free to use, change and run, including inside your business. You can't sell it as a competing product or service. Each release becomes Apache 2.0 two years after it is published.

Licensed under the [Functional Source License](https://fsl.software) (FSL-1.1-ALv2). See [LICENSE](LICENSE).
