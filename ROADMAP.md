# Roadmap

The basic direction. Not a dated plan. Each issue sits in a milestone, so you can see what comes first.

## Now: usable by one developer (0.6.4 to 0.6.6, and more 0.6.x before 0.7)

One person, `agentbus up` on one machine: any model, and its work checked.

- **Any model.** Claude Code, Codex and Antigravity, and any model through OpenRouter, from one place.
- **Checked.** One click asks another model to check an agent's work, right next to Apply.
- **See what a turn did.** The files it changed, the commands it ran and their output, and what it cost.
- **Sandbox, one switch.** Start a session in a walled copy of your folder, then apply or discard what it did.
- **Clear when something goes wrong.** Every error on the first-run path says what to do.

## Next: programs on events (0.7)

Standing instructions an agent can set up from chat (on a timer, before changes are applied, on outside events
such as a new pull request), each run recorded like any turn.

## Later

More machines and runners; team views; a browser and other tools for every agent.

## Where help is welcome

Issues labelled **wanted now** matter most this month; **good first issue** are small with a starting point in the
code; **design first** means: post a short plan in the issue before writing code.

- **More model routes:** local OpenAI-compatible endpoints (Ollama, LM Studio), more OpenRouter models, the OpenAI
  Chat Completions format.
- **More harnesses:** the Gemini CLI on its Google sign-in.
- **The first-run path:** clearer messages, the install script, the README.
- **Prices that stay current:** where the built-in price table drifts from a public source.

Ideas and requests: open an issue.
