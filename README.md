# Cortex

Cortex delivers easy-to-use platform services to agentic workloads. It runs in a workload's request path — a sidecar in Kubernetes, or a standalone binary anywhere else — and provides:

- **Identity & access** — a verifiable identity for each workload, authentication and authorization of its calls, and the right credentials for each downstream service.
- **Guardrails** — block agent actions that stray from the user's intent or aren't grounded in the conversation.
- **Observability** — decrypt and parse a workload's model, tool, and agent-to-agent traffic into a live view.
- **Egress control** — govern which external services a workload can reach.
- **Optimizations** — trim the model context a workload sends and cap its spend, to cut latency and cost.

It ships as a single binary; the identity and access layer is **AuthBridge**, and the code lives under [`authbridge/`](./authbridge/).

## Quick start (local, no Kubernetes)

Watch an AI agent's traffic — its model, tool, and agent-to-agent calls —
decrypted and parsed live on your laptop.

1. **Install and start Cortex** (macOS/Linux). Downloads two small binaries,
   writes one config under `~/.cortex`, and starts the proxy in the background:

   ```sh
   curl -fsSL https://raw.githubusercontent.com/rossoctl/cortex/main/authbridge/install.sh \
     | sh -s -- --claude-code
   ```

   `--claude-code` then asks whether to point Claude Code at it, by adding three
   variables to the `env` block of `~/.claude/settings.json`. It shows them first
   and changes nothing else. Say no and everything still works — you just pass
   the variables yourself. (`--install-only` installs the binaries and stops.)

   Traffic is decrypted and parsed for viewing; nothing is rewritten.

2. **Open the live viewer** in another terminal:

   ```sh
   abctl --endpoint http://localhost:47601
   ```

3. **Run Claude Code normally:**

   ```sh
   claude
   ```

   Its calls stream into `abctl`, decrypted and parsed. Stop the proxy with
   `kill $(cat ~/.cortex/proxy.pid)`, and undo the settings change with
   `abctl claude-code disable`.

   Settings rather than a shell export on purpose: Claude Code's supervisor is one
   process shared by every terminal and inherits whichever shell started it first,
   so an exported variable reaches background agents only by luck. If you would
   rather not have it edit the file, `abctl claude-code status` shows what it would
   set and you can pass those three variables to `claude` yourself.

**Using Claude Code?** One more command turns this into a cost saving — Cortex
strips the tool definitions your agent never calls out of every request, worth
**4–20% of the prompt billed per turn, median 6%** over 99 requests of one real
session: **[Cut Claude Code token cost](./authbridge/docs/laptop-token-savings.md)**.
It is opt-in because it rewrites requests, and because the tool list it proposes
is read from Claude Code's own transcripts.

## Running on Kubernetes

In a cluster, Cortex sidecars are injected automatically by the [operator](https://github.com/rossoctl/operator), with Keycloak + SPIFFE/SPIRE for identity and token exchange. Start with the end-to-end **[Weather Agent walkthrough](./authbridge/demos/weather-agent/demo-ui.md)** (or the [`abctl` version](./authbridge/demos/weather-agent/demo-with-abctl.md)); see the [demos index](./authbridge/demos/README.md) and the [architecture reference](./authbridge/README.md) for all modes and details.

## Related repositories

- [rossoctl](https://github.com/rossoctl/rossoctl) — core platform
- [operator](https://github.com/rossoctl/operator) — sidecar injection + admission webhook

## License

[Apache 2.0](./LICENSE)
