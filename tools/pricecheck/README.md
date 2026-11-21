# pricecheck: where the built-in price table and LiteLLM disagree

`tools/pricecheck` compares the built-in price table (`cost/table.go`) with a price file in
LiteLLM's `model_prices_and_context_window` format, read by `cost.LoadLiteLLM`. It is not part of
the release binary, and it is read-only: it reports differences and never rewrites `table.go`.

## Run it

```sh
go run ./tools/pricecheck                    # fetch LiteLLM's current file from GitHub
go run ./tools/pricecheck -file prices.json  # or read a file you already have
```

Flags:

- `-file <path>`: read this file instead of fetching one.
- `-url <url>`: fetch from here instead of LiteLLM's main branch.
- `-missing`: also list built-in models the file has no entry for.

It prints one line per rate that differs, and nothing when every rate both sides know matches:

```text
MODEL                      RATE             BUILT-IN  LITELLM
anthropic/claude-opus-5-5  standard output  20        25
```

Rates are USD per million tokens. Exit status: 0 when nothing differs, 1 when something does, 2
when the file can't be fetched or read.

## What is compared

Each built-in model is matched to the file's entry for the same vendor and model id. Only what
both sides price is compared: the tiers (standard, batch, flex, fast), each token class, the
long-context threshold and rates, and the data-residency multiplier. A rate one side has no price
for is unknown there, not different, so it is skipped. Models only the file has are not listed.

When a line shows up, check the vendor's pricing page before changing `cost/table.go`: LiteLLM
can be the one that is behind.
