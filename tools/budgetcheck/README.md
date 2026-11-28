# budgetcheck: the history budget, measured on a real session

`tools/budgetcheck` answers one question offline, with no model call: on a session already in the
record, what would own replay have sent, and paid, with the history budget off and with it on? It
is not part of the release binary.

## Run it

```sh
go run ./tools/budgetcheck -data ~/.local/share/agentbus -session <session id>
```

- `-data`: the data folder, its `events.db`, or its `jobs.db` (the record is `events.db` beside
  it). The file is opened read-only, with no lock, so the tool runs beside a running service.
- `-session`: the session to measure.
- `-budget 0.4`: the on arm's budget, a share of each model's window (the default is the
  settings default).
- `-format`: `anthropic`, `openai` or `chatcompletions`. Default: the session's harness's.
- `-json <file>`: also write the whole report, every turn and call.

A 186-turn session takes about three minutes.

## What it prints

One row a turn, then the totals of three columns: **real** (what the record says was sent and
paid), **budget off** and **budget on**.

- `calls`: the turn's calls that carried the history, of all its calls.
- `real hist`, `off hist`, `on hist`: the history in front of the turn, in budget tokens (bytes /
  2.66, an image 1,600), the budget's own count. `fresh`: the turn carried none.
- `on cached`: how much of the on arm's history opens the previous turn's last request byte for
  byte, the part a warm prompt cache reads. A step drops it to near 0 once.
- `on step`, `recorded step`: the step fit made in the on arm, and the `history.shortened` the
  gateway recorded, as `before>after` and what fired: `R` results made stubs, `I` inputs made
  stubs, `S` turns summarised, `L` turns left out of the summary, `N` the newest turn shortened.
- `context`: whether the real history and the on arm's carry the standing context of the turn
  that started the chain (the harness's head, Claude Code's system-reminder blocks with
  CLAUDE.md, memory and AGENTS.md) byte for byte, as `real/on`: `no/yes` is a turn whose
  recorded history lost it. Totalled under the table.
- `real $`, `off $`, `on $`: per turn.

Under the totals, the self-check: how many turns each arm rebuilt within 1% of what the record
carried. **A session that ran without the budget must match the off arm at every turn**, and one
that ran with it the on arm at the same budget. A miss means the rebuild is wrong for that
session, and its numbers are not to be trusted.

## How the arms are built

`replay.WhatIf` runs the gateway's own `fit` on each turn. A turn's own part (its person's
message, tool loop, last reply, and AgentBus's note) is read from the record once. Each arm chains
it onto its own carried history: `full(t) = carried(t) + own(t)` and `carried(t) =
fit(full(from))`. The off arm never fits. For Claude Code, the turn's first system message is laid
out again for each arm, as `build` does: cut when the history holds an environment, whole when it
holds none. Known answer: `replay/whatif_test.go` records one session twice (budget off, budget
on), and from either record the arms are each record's history at every turn, with the very
steps the gateway recorded. All three formats pass.

## How the cost is modelled

Only the calls that carried a history change. The others are priced as recorded.

1. **Size.** The request grows or shrinks by the arm's history less the real one, times the
   session's real tokens per budget token. That ratio is printed. It is the vendor's count over
   the budget's count on the same calls, about 0.74 to 0.82 on Claude Code sessions. When no call
   that carried a history has usage, the ratio cannot be measured: 1 is used, and the output
   says `NOT MEASURED` (JSON `scale_measured: false`).
2. **Cache.** A call's recorded cache read covers the system prompt, then the history, then the
   turn's own messages. The arm changes only the history's part. At a turn's first call that part
   is the history's cached prefix (`on cached`). At later calls it is the whole history. If the
   recorded read covered at least 80% of the real cached history, the cache was warm, and the arm
   reads its own cached part instead. Otherwise the cache was cold (a long pause), and the arm
   reads only what the call read. When the real cached history is under 1,024 tokens the reads
   cannot tell, so the gap since the previous carrying call decides, against the session's cache
   lifetime.
3. **Write.** What is not read is written, with the call's own split of cache lifetimes.
   Uncached input stays as recorded. A call of any vendor other than Anthropic that wrote nothing
   to the cache bills the rest as uncached input. That is how OpenAI bills (its usage reports no
   cache writes). For any other vendor it is an assumption: the output counts these calls, and
   says so under the totals.
4. **Price.** The recorded cost, plus `cost.Default()`'s price of the arm's usage, less its
   price of the recorded usage. So the off arm of a session that ran without the budget costs
   exactly what it paid. Hand-worked case: `TestPriceByHand`.

## What it cannot tell

- **Quality.** A shorter history may lose what the model needed. Only a live run tells, and the
  three-arm run in agentbus-notes `plans/BUDGET_THREE_ARM_RUN.md` measures it.
- **Behaviour.** The arms keep the session's own calls. With a different history the model
  might have made more calls (fetching stubs back) or fewer.
- **Latency.** Not in the record per arm.
- **The real tokenizer.** Sizes use one ratio per session, so a history unlike the session's
  average (many images) is off by that much.
- A turn whose history the record cannot rebuild is marked `!`. It and the turns after it go on
  from the record.
