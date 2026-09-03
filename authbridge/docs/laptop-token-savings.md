# Cut Claude Code token cost on your laptop

Claude Code sends the full tool manifest on every turn — tens of thousands of
tokens of JSON schema, billed each time — and the manifest is built by the client,
so a proxy is the only place to trim it without changing every client. Cortex
strips the definitions your agent never calls.

Nothing here installs anything: the proxy comes from the
[README quick start](../../README.md#quick-start-local-no-kubernetes), which sets
it up to observe traffic without changing it. Pruning is a separate, opt-in step,
because it rewrites requests and because the list it proposes is read from Claude
Code's own transcripts — of no use if you drive a different agent.

## Turn it on

```sh
abctl tools scan --write ~/.cortex/config.yaml
```

That reads your `~/.claude/projects` transcripts, proposes the built-in tools you
have not called in 30 days, and writes them to `tool-prune`'s `remove:` list. The
proxy hot-reloads, so it takes effect immediately — no restart.

**Choosing the window.** `--days N` sets it; `--all` ignores it and counts every
call in every transcript. Widening is the *cautious* direction: a longer window can
only find more tools in use, so it proposes fewer for removal. Reach for `--all` if
you have been using Claude Code for months and want nothing pruned that you have
ever touched; keep the 30-day default to also drop tools you used once and moved on
from. (`--days 0` is rejected rather than read as "everything" — a zero-width
window finds nothing used, which would propose removing every tool it knows.)

It prints what it chose before writing. Two guards on what it will propose:

- It only ever proposes tools it **recognises**, and never one it has **seen you
  call** — including tools implied by ones you called, so `BashOutput` survives if
  you have used background `Bash`.
- It **refuses to write at all** if it saw no tool calls to reason from. With no
  history, "tools you have not called" would be every tool it knows, which is a
  guess rather than a measurement.

To undo: delete names from `remove:` in `~/.cortex/config.yaml`, or empty the list
to disable pruning entirely. Either way the proxy reloads without a restart.

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

**What the scan cannot know is the future.** It reports what you have not used, not what
you will not need. If you start work that needs a pruned tool, its definition is
gone from the request and the model cannot call it — a functional failure, not
merely a smaller saving. So:

- **Re-run it occasionally** (monthly, or when your work changes shape):

  ```sh
  abctl tools scan --write ~/.cortex/config.yaml
  ```

  The proxy hot-reloads; no restart. `--days N` / `--all` set the window (see
  above) and `--keep Name,Name` protects specific tools by name.

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
- **`tool-prune` shows `skip`, never `modify`** — expected until you opt in: the
  remove list ships empty. Run the scan above. If it refuses, you have no
  transcript history for it to reason from yet.
- **The proxy won't start** — read `~/.cortex/proxy.log`; a port conflict is logged
  at `ERROR`. Every listener is pinned to loopback on 47600–47604, so a clash
  usually means Cortex is already running (`kill $(cat ~/.cortex/proxy.pid)`).
