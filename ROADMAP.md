# Roadmap

The basic direction. Not a dated plan. Each issue sits in a milestone, so you can see what comes first.

## Now: usable by one developer (0.6.4 to 0.6.6, and more 0.6.x before 0.7)

One person, `agentbus up` on one machine: any model, and its work checked.

- **Any model.** Claude Code, Codex and Antigravity, and any model through OpenRouter, from one place.
- **Checked.** One click asks another model to check an agent's work, right next to Apply.
- **See what a turn did.** The files it changed, the commands it ran and their output, and what it cost.
- **Direct, Copy or Locked, per session.** Copy works in its own copy of your folder: the agent's commands write only
  there, so several sessions can work on the same code; then review, apply or discard. Locked comes later.
- **One sign-in path.** Connect each agent once in Settings; your own setup (plugins, config) comes along.
- **Clear when something goes wrong.** Every error on the first-run path says what to do.

## Next: events in, actions out (0.7)

Programs that react to events: something happens (a new pull request, a message), a turn runs, an action goes out
(a drafted review, a reply). GitHub and Telegram are the first examples; the core stays generic.
A program acts with its own scoped token: a grant naming which sources it may work from, which levels (Copy or
Locked, never Direct) and which actions, so each pull request or chat gets its own workspace.

## Then

- **0.8, hosted and teams:** AgentBus serving more than its owner, more machines, team views, webhooks.
- **0.9, our own harness:** our own agent loop and tools, depending on no vendor.

## Where help is welcome

Issues labelled **wanted now** matter most this month; **good first issue** are small with a starting point in the
code; **design first** means: post a short plan in the issue before writing code.

- **More model routes:** local OpenAI-compatible endpoints (Ollama, LM Studio), more OpenRouter models, the OpenAI
  Chat Completions format.
- **More harnesses:** the Gemini CLI.
- **The first-run path:** clearer messages, the install script, the README.
- **Prices that stay current:** where the built-in price table drifts from a public source.
- **Testing:** the first ten minutes (UI and UX rough edges), security holes, Windows and WSL2.

Ideas and requests: open an issue.
