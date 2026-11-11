# agenttest: can a fresh agent use AgentBus?

`tools/agenttest` gives a new agent five plain requests and checks, from the record and the folder,
whether it did each one. It is the before-and-after test for a change to the API or to what a turn
is told. It is not part of the release binary. It needs Python 3 (standard library only), a built
`agentbus`, and `claude` signed in; `codex` too for a codex round.

## The five tasks

Each run is one new session in a new folder (`calc.py`, `README.md`). Seed turns on the saved agent
"Claude Haiku" set the session up. Then one test turn, on the agent under test, gets the prompt.

| Task | Seed turns | Test prompt | Done when |
|---|---|---|---|
| 1 | one turn adds `add()` to `calc.py` | Ask another agent to review your last turn. | a review turn of the seed turn exists |
| 2 | four turns: `a.txt` "one", a line "two", `b.txt` "three", `c.txt` "four" | Apply turn 3 of this session to my folder. | the person's folder has `a.txt` (one, two) and `b.txt`, and no `c.txt` |
| 3 | a decision for the record (SQLite in WAL mode, not Redis, one machine), then two edits | What did we decide earlier in this session about the cache, and why? | the reply names SQLite, WAL and the reason |
| 4 | the session asks for a turn that runs `sleep 1800` | A turn is running away: it has been going far too long. Stop it. | that turn reads stopped |
| 5 | one edit, then the session asks Claude Haiku to review it, and the result comes back | How much did the review cost? | the reply holds the review turn's `cost_usd`, within 5% |

Tasks 4 and 5 need work the session itself asked for, which only an agent's turn token can make, so
those seed turns run a given `agentbus-api` command. That command stays in the session's history:
the agent under test can see how a turn or a review was asked for.

## Run it

```sh
go build -o /tmp/at/agentbus ./cmd/agentbus
python3 tools/agenttest/agenttest.py up -agentbus /tmp/at/agentbus -dir /tmp/at/data -port 8911
python3 tools/agenttest/agenttest.py agents -dir /tmp/at/data -without-codex
python3 tools/agenttest/agenttest.py run -dir /tmp/at/data claude sonnet 1 claude-t1-r1
python3 tools/agenttest/agenttest.py table -dir /tmp/at/data
python3 tools/agenttest/agenttest.py show /tmp/at/data/runs/claude-t1-r1/result.json
python3 tools/agenttest/agenttest.py down -dir /tmp/at/data
```

- `up` starts `agentbus up` on the port with a new data folder, puts `bin/` first on its `PATH`,
  allows `sleep` in `permissions.toml` (task 4), and saves the "Claude Haiku" agent. It refuses a
  port in use or a data folder that exists.
- `agents -without-codex` removes the codex agents, so a claude round cannot hand work to codex.
  `-with-codex` adds one for a codex round.
- `run AGENT MODEL TASK LABEL` makes one run and writes `runs/LABEL/result.json`: the test turn, its
  diff (tool calls and model calls), its events, and the check. `-no-followup` stops the work the
  test turn asked for and the turns its results start, once the measurement is taken: use it for a
  vendor with little quota.
- `up -show-own-agents` keeps Claude Code's own agent tools (`Task`, `Agent`, `ListAgents`,
  `SendMessage`) in a turn that has the `agentbus` MCP tools: `bin/claude` takes them out of
  `--disallowedTools`. It tells the tools' effect apart from the hiding's.
- `table` prints one row per run. `api_calls` counts AgentBus calls: `agentbus-api` commands and
  `agentbus` MCP tool calls (claude's `mcp__agentbus__…` tools, and those codex runs from its `exec`
  code). `api_failed` counts the commands that answered 400 or more or that the harness refused to
  run, and the tool calls whose answer is a refusal. Calls that worked but were not needed are judged by hand from `show`.
- `down` stops the service by its PID. Remove the data folder yourself once the results are read.

## What the agent sees

`bin/claude` and `bin/codex` stand in for the real programs. A turn's claude runs with
`--setting-sources project,local`: no user settings, hooks or user `CLAUDE.md`. A turn's codex runs
with `--ignore-user-config --disable hooks` and a fixed reasoning effort (`up -effort`, default
high). The sign-in is not touched. ⚠️ codex still reads `~/.codex/AGENTS.md`: it has no switch for
it. Say so in any result from a machine where that file exists.

## After an API change

The prompts and the seeds' words stay the same, so the numbers compare. The routes this tool calls
as the person are in `R`, and the two seed commands in `SEED_ASK_TURN` and `SEED_ASK_REVIEW`: port
those and `check`. Ported to the 0.4 API (agent, session, turn) on 2026-11-11: the review seed asks
`POST /api/v0/turns/{turn}/review`, and every check reads turns, not jobs.
