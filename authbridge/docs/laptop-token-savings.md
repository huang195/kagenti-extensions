# Cut Claude Code token cost on your laptop

Cortex runs as a local proxy in front of Claude Code and strips tool definitions
your agent never calls out of every request. Claude Code sends the full tool
manifest on every turn — tens of thousands of tokens of JSON schema, billed each
time — and the manifest is built by the client, so the proxy is the only place to
trim it without changing every client.

macOS or Linux, amd64 or arm64. No cluster, Keycloak, or SPIRE.

## 1. Install and start

```sh
curl -fsSL https://raw.githubusercontent.com/rossoctl/cortex/main/authbridge/install-demo.sh \
  | sh -s -- --claude-code
```

That downloads the released binaries into `~/.local/bin`, writes
`~/.cortex/config.yaml`, picks which tools to prune from your own transcripts,
starts the proxy, and prints the command for step 2. Re-running it is safe — it
never overwrites a config you already have.

## 2. Run Claude Code through it

```sh
HTTPS_PROXY=http://localhost:47600 \
  NODE_EXTRA_CA_CERTS="$HOME/.cortex/ca/ca.crt" \
  CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
  claude
```

Step 1 prints this line with the paths already filled in.

## 3. See what it saved

```sh
abctl --endpoint http://localhost:47601
```

Plugin pane → `tool-prune` → `Metrics`. Stop the proxy with
`kill $(cat ~/.cortex/proxy.pid)`.

## What to expect

**4–20% of the prompt per turn, median 6%**, measured over 99 requests of one
real session. Two things move it, and neither is a defect:

- **How much of the manifest is yours to prune.** Requests carrying the full tool
  set saved 15–20%; most requests in that session offered a reduced set and saved
  4–6%.
- **How far into the conversation you are.** The removed bytes are a fixed size,
  so their share of a growing prompt falls — 13% early in that session, 4% by the
  end.

A single early turn can read ~24%, which is why a figure quoted from one request
is not the number to plan with.

`$ saved` appears with no extra configuration, labelled `default rates`. **Read it
as a floor.** The built-in rates were measured on a shared gateway that bills
below vendor list; if your Claude Code talks straight to Anthropic — which it does
unless you have set `ANTHROPIC_BASE_URL` — you pay list, so the real saving is
several times what the column shows. To make it accurate, set your own rates:
[`tool-prune-plugin.md`](./tool-prune-plugin.md#costing-it).

## Keeping the prune list honest

The scan proposes tools you have not called in 30 days. It only ever proposes
tools it recognises, never one it has seen you call, and it refuses to write a
list at all if it saw no tool calls to reason from.

**What it cannot know is the future.** It reports what you have not used, not what
you will not need. If you start work that needs a pruned tool, its definition is
gone from the request and the model cannot call it — a functional failure, not
merely a smaller saving. So:

- **Re-run it occasionally** (monthly, or when your work changes shape):
  `abctl tools scan --write ~/.cortex/config.yaml`. The proxy hot-reloads.
- **If a tool goes missing, delete its name from `remove:`** in
  `~/.cortex/config.yaml`. It comes back without a restart.
- **To try a list without committing to it**, set `on_error: observe` on the
  `tool-prune` plugin. It measures the saving and changes nothing; abctl marks
  those figures with `~`.

## What this does and does not change

`/cost` and anything from the API response `usage` block **do** drop — the server
bills the request it received.

`/context` **does not**. It is computed client-side before the request leaves, and
the pruning happens downstream. So this saves money, not context window;
auto-compact still triggers at the same point. Recovering headroom needs
client-side settings (`--allowedTools`, disabling unused MCP servers).

## If it isn't working

- **Metrics pane empty, every event shows `tunnel`** — Claude Code is not trusting
  the bridge CA. Check `NODE_EXTRA_CA_CERTS` is the absolute path from step 2. The
  proxy also warns about this in `~/.cortex/proxy.log` after a few requests.
- **The proxy won't start** — read `~/.cortex/proxy.log`; a port conflict is
  logged at `ERROR`. The config pins every listener to loopback on 47600–47604, so
  a clash usually means Cortex is already running.
- **`abctl: command not found`** — `~/.local/bin` is not on your `PATH`:
  `export PATH="$HOME/.local/bin:$PATH"`.

## Other ways in

- **Just the binaries**, no setup: `... | sh -s -- --install-only`.
- **A throwaway demo** in the current directory instead of a persistent config:
  `... | sh` with no arguments.
- **Pin a version**: `AUTHBRIDGE_VERSION=vX.Y.Z`.
- **Re-run setup offline**, using the binaries you already have:
  `AUTHBRIDGE_SKIP_DOWNLOAD=1`.
