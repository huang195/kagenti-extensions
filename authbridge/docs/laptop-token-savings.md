# Cut Claude Code token cost on your laptop

Claude Code sends the full tool manifest on every turn — tens of thousands of
tokens of JSON schema, billed each time — and the manifest is built by the client,
so a proxy is the only place to trim it without changing every client. Cortex
strips the definitions your agent never calls.

**Setup is the [README quick start](../../README.md#quick-start-local-no-kubernetes)** —
one command, and it fills the prune list for you. This page is what that gets you
and how to tune it; there is nothing extra to install.

## What to expect

**4–20% of the prompt per turn, median 6%**, measured over 99 requests of one real
session. Two things move it, and neither is a defect:

- **How much of the manifest is yours to prune.** Requests carrying the full tool
  set saved 15–20%; most requests in that session offered a reduced set and saved
  4–6%.
- **How far into the conversation you are.** The removed bytes are a fixed size,
  so their share of a growing prompt falls — 13% early in that session, 4% by the
  end.

A single early turn can read ~24%, which is why a figure quoted from one request is
not the number to plan with.

Watch it live in `abctl` (`--endpoint http://localhost:47601`): the plugin pane's
`tool-prune` → `Metrics`, and the per-request saving in the events timeline's
`TOKENS / SAVED` column.

## Reading the dollar figure

`$ saved` appears with no configuration, labelled `default rates`. **Read it as a
floor.** The built-in rates were measured on a shared gateway that bills below
vendor list; if your Claude Code talks straight to Anthropic — which it does unless
you have set `ANTHROPIC_BASE_URL` — you pay list, so the real saving is several
times what the column shows.

Savings are reported per prompt-cache tier, never as one blended number: providers
charge ~1.25x the input rate for a cache write and ~0.1x for a cache read, so
identical saved bytes differ by more than 12x depending on cache state.

To make the figure accurate, set your own rates — per million tokens, the unit
price lists use, and keyed by model family so a version bump needs no edit:

```yaml
# ~/.cortex/config.yaml, under the tool-prune plugin
config:
  pricing:
    "*claude-opus-*":
      input_cost_per_million: 3.80
      cache_write_cost_per_million: 4.75
      cache_read_cost_per_million: 0.38
```

Full reference, including how to measure your own from a gateway's cost headers:
[`tool-prune-plugin.md`](./tool-prune-plugin.md#costing-it).

## Keeping the prune list honest

The scan proposes tools you have not called in 30 days. It only ever proposes tools
it recognises, never one it has seen you call, and it refuses to write a list at
all if it saw no tool calls to reason from.

**What it cannot know is the future.** It reports what you have not used, not what
you will not need. If you start work that needs a pruned tool, its definition is
gone from the request and the model cannot call it — a functional failure, not
merely a smaller saving. So:

- **Re-run it occasionally** (monthly, or when your work changes shape):

  ```sh
  abctl tools scan --write ~/.cortex/config.yaml
  ```

  The proxy hot-reloads; no restart. Use `--days N` to widen the window and
  `--keep Name,Name` to protect specific tools.

- **If a tool goes missing, delete its name from `remove:`** in
  `~/.cortex/config.yaml`. It comes back without a restart.

- **To try a list without committing to it**, set `on_error: observe` on the
  `tool-prune` plugin. It measures the saving and changes nothing; abctl marks
  those figures with `~` instead of `−`.

## What this does and does not change

`/cost` and anything from the API response `usage` block **do** drop — the server
bills the request it received.

`/context` **does not**. It is computed client-side before the request leaves, and
the pruning happens downstream. So this saves money, not context window;
auto-compact still triggers at the same point. Recovering headroom needs
client-side settings (`--allowedTools`, disabling unused MCP servers).

## If it isn't working

- **Metrics pane empty, every event shows `tunnel`** — Claude Code is not trusting
  the bridge CA. `NODE_EXTRA_CA_CERTS` must point at `~/.cortex/ca/ca.crt`,
  expanded to an absolute path. The proxy also warns about this in
  `~/.cortex/proxy.log` after a few requests, naming the path it expects.
- **`tool-prune` shows `skip`, never `modify`** — the remove list is empty. Run the
  scan above; if it refuses, you have no transcript history for it to reason from
  yet.
- **The proxy won't start** — read `~/.cortex/proxy.log`; a port conflict is logged
  at `ERROR`. Every listener is pinned to loopback on 47600–47604, so a clash
  usually means Cortex is already running (`kill $(cat ~/.cortex/proxy.pid)`).
