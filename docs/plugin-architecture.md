# AuthBridge Plugin Architecture Proposal

**Status**: Proposal  
**Authors**: Kagenti AuthBridge maintainers  
**Date**: April 2026

## Context

AuthBridge intercepts all inbound and outbound traffic for Kubernetes AI
agents, handling JWT validation and token exchange transparently via sidecar
injection. The architecture supports three deployment modes (envoy-sidecar,
proxy-sidecar, waypoint), each with a different traffic interception
mechanism but the same auth logic.

The auth logic is currently hardcoded: inbound validates JWTs, outbound
exchanges tokens. This proposal adds an extensible plugin pipeline so
additional processing — observability, guardrails, authorization, credential
privacy, egress policy — can be added without modifying the core binary.

### Current Architecture

The `Auth` struct in `authlib/auth/auth.go` already composes independent
building blocks through interfaces and function types:

```go
type Auth struct {
    verifier         validation.Verifier   // JWT validation (interface)
    exchanger        *exchange.Client      // RFC 8693 token exchange
    cache            *cache.Cache          // SHA-256 keyed token cache
    bypass           *bypass.Matcher       // path-based bypass (/healthz, etc.)
    router           *routing.Router       // host-glob → audience/scopes
    identity         atomic.Pointer[IdentityConfig]  // hot-reloadable
    noTokenPolicy    string                // allow | deny | client-credentials
    actorTokenSource ActorTokenSource      // func(ctx) → actor token (optional)
    audienceDeriver  AudienceDeriver       // func(host) → audience (optional)
}
```

Listeners are already thin adapters. The ext_proc listener is ~170 lines
that translate between Envoy's `ProcessingRequest` and `HandleInbound` /
`HandleOutbound`. The ext_authz listener is ~100 lines. The reverse and
forward proxy listeners are similar. The adapter pattern already exists in
the codebase — this proposal formalizes it and opens it to extension.

The `ActorTokenSource` and `AudienceDeriver` function types are the existing
precedent for pluggable behavior: optional functions injected at construction
time that modify how auth decisions are made. The plugin architecture
generalizes this pattern.

## Design Principles

1. **Simple first version** — one interface, one context, compiled-in plugins
2. **Mutations on context** — plugins modify headers/body directly on the
   context, no separate mutation API
3. **Mode-agnostic plugins** — plugins never see protocol details (ext_proc,
   HTTP proxy, ext_authz); a thin adapter per mode converts to/from the
   shared context
4. **Tighten-only policy** — a plugin can add validation or reject requests
   but cannot bypass built-in security (see [enforcement](#tighten-only-enforcement))
5. **Declare what you need** — body access is opt-in per plugin; the pipeline
   only buffers the body when at least one plugin requests it
6. **Identity-first** — every plugin receives the agent's own identity and
   the caller's validated claims, enabling security decisions no external
   proxy can make

## Plugin Interface

```go
// Plugin is the single interface that all extensions implement — both
// built-in (jwt-validation, token-exchange) and custom.
type Plugin interface {
    Name() string
    OnRequest(ctx *Context) Action
    OnResponse(ctx *Context) Action
}
```

### Context

```go
type Context struct {
    // Request metadata
    Direction Direction       // Inbound or Outbound
    Method    string          // HTTP method
    Host      string          // target host (port stripped)
    Path      string          // request path
    Headers   http.Header     // read/write — mutations are applied downstream
    Body      []byte          // nil unless plugin declared body_access: true

    // Identity — what makes AuthBridge plugins unique
    Agent     *AgentIdentity  // this agent's own identity (workload identity + client_id)
    Claims    *Claims         // caller's validated JWT claims (nil before jwt-validation runs)
    Route     *ResolvedRoute  // resolved routing decision (audience, scopes, passthrough)

    // Plugin-to-plugin communication
    Values    map[string]any  // shared state between plugins in the chain
}
```

**`Agent`** carries the agent's own workload identity. This is always
populated (from `auth.IdentityConfig`), even before any plugin runs.
Plugins can make decisions based on *who this agent is*, not just who is
calling it.

**`Claims`** contains the caller's validated JWT claims (subject, issuer,
audience, client_id, scopes) and is nil until the `jwt-validation` plugin
runs. This maps directly to `validation.Claims` in `authlib/validation/`.
Plugins after `jwt-validation` can read `ctx.Claims` without re-parsing
the token. Claims are exposed as read-only to plugins — only the
`jwt-validation` built-in can populate them.

**`Route`** carries the resolved routing decision from `routing.Router`
— the target audience, scopes, and passthrough flag. This is populated
before the pipeline runs (routing is infrastructure, not a plugin concern).
Plugins that need to make decisions based on the destination service read
`ctx.Route` instead of re-resolving.

**`Values`** is a typed map for plugin-to-plugin communication. For example,
a protocol-parsing plugin reads `ctx.Body`, parses the MCP JSON-RPC
envelope, and sets `ctx.Values["mcp.tool_name"]` for downstream plugins.

### AgentIdentity

```go
type AgentIdentity struct {
    ClientID    string // OAuth client_id
    WorkloadID  string // workload identity URI (SPIFFE, k8s SA, platform-specific)
    TrustDomain string // trust domain of the workload identity
}
```

The identity type is deliberately abstract — it works with SPIFFE, plain
Kubernetes service accounts, or any platform-specific workload identity
scheme. AuthBridge supports multiple identity backends (`identity.type` in
config: `spiffe`, `client-secret`, `k8s-sa`), and the plugin context
normalizes them into one struct.

This is what makes AuthBridge plugins fundamentally different from generic
proxy extensions. A guardrails plugin can allow tool X for internal callers
(same trust domain) but block it for external callers. An audit plugin can
correlate every request with the agent's workload identity. No proxy
extension has access to both the caller's JWT identity *and* the agent's
workload identity in a single context.

### Action

```go
type Action struct {
    Type   ActionType
    Status int    // only for Reject
    Reason string // only for Reject
}

type ActionType int
const (
    Continue ActionType = iota // pass to next plugin with any ctx mutations
    Reject                     // stop pipeline, return error to client
)
```

Mutations happen directly on `ctx.Headers`, `ctx.Body`, or `ctx.Values`.
Returning `Continue` means "proceed with whatever I changed." There is no
separate mutation action.

## Pipeline

The pipeline holds an ordered list of plugins and runs them sequentially:

```go
type Pipeline struct {
    plugins []Plugin
}

func (p *Pipeline) Run(ctx *Context) Action {
    for _, plugin := range p.plugins {
        action := plugin.OnRequest(ctx)
        if action.Type == Reject {
            return action
        }
    }
    return Action{Type: Continue}
}
```

Response plugins run in reverse order (last plugin sees the response first):

```go
func (p *Pipeline) RunResponse(ctx *Context) Action {
    for i := len(p.plugins) - 1; i >= 0; i-- {
        action := p.plugins[i].OnResponse(ctx)
        if action.Type == Reject {
            return action
        }
    }
    return Action{Type: Continue}
}
```

## Pipeline Configuration

Plugins are declared in `config.yaml` with explicit ordering:

```yaml
pipeline:
  inbound:
    - jwt-validation
    - audit-log:
        destination: "s3://audit-bucket"
    - guardrails:
        body_access: true
        max_prompt_tokens: 4096
        on_error: continue   # fail-open for this plugin
  outbound:
    - pii-redactor:
        body_access: true
    - token-exchange
    - egress-policy:
        policy_file: /etc/authbridge/egress.rego
```

Properties:
- **Ordering is explicit** — plugins run in the order listed
- **`on_error`** — `reject` (default) or `continue` per plugin
- **`body_access`** — opt-in; pipeline buffers the body only when needed
- **Plugin-specific config** — passed to the factory as `map[string]any`

When no `pipeline` section is present in config, the default pipeline is:

```yaml
pipeline:
  inbound:
    - jwt-validation
  outbound:
    - token-exchange
```

This preserves backward compatibility — existing deployments work unchanged.

## Tighten-Only Enforcement

The "tighten-only" principle is not just a convention — it is enforced:

1. **Required plugins**: the pipeline validates at startup that
   `jwt-validation` is present in the inbound chain and `token-exchange`
   is present in the outbound chain. Config that removes either fails
   startup with a clear error.
2. **Protected registry**: built-in plugins (`jwt-validation`,
   `token-exchange`) are registered in a sealed registry. Custom plugins
   cannot replace them by registering the same name.
3. **Read-only claims**: `ctx.Claims` is populated exclusively by
   `jwt-validation`. Custom plugins receive a read-only view — they can
   read claims for authorization decisions but cannot forge or modify them.
4. **No bypass escalation**: `ctx.Route.Passthrough` is set by the routing
   layer before the pipeline runs. A plugin cannot flip a non-passthrough
   route to passthrough.

Custom plugins can *add* authorization checks (reject requests that the
built-in plugins would allow) but cannot *weaken* security (allow requests
that the built-in plugins would reject). This is the key invariant.

## Plugin Loading (v1: Registry)

Plugins register themselves via a factory function:

```go
// In the plugin package
func init() {
    pipeline.Register("pii-redactor", NewPIIRedactor)
}

func NewPIIRedactor(cfg map[string]any) (Plugin, error) {
    maxSize := cfg["max_size"].(int)
    return &PIIRedactor{maxSize: maxSize}, nil
}
```

```go
// In main.go or a build-tag file — one import per plugin
import _ "github.com/kagenti/authbridge-plugins/pii-redactor"
```

At startup the pipeline reads config, looks up each plugin name in the
registry, calls the factory with the plugin's config, and builds the chain.
Unknown names fail startup with a clear error.

This is compiled-in: adding a plugin means adding an import and rebuilding
the authbridge image. The tradeoff is zero runtime overhead and compile-time
safety.

### Future Loading Mechanisms

These are not part of v1 but are anticipated:

| Mechanism | Tradeoff |
|-----------|----------|
| **Go plugins** (`.so` at runtime) | More modular, but fragile — must match exact Go version and dependencies |
| **Sidecar** (gRPC call to separate container) | Any language, maximum isolation, but adds network hop per plugin per request |
| **WASM** (embedded runtime) | Language-agnostic, sandboxed, but limited Go interop and higher complexity |

## Adapter Layer (Mode Abstraction)

Each deployment mode implements a thin adapter that converts between
protocol-specific types and the shared `Context`:

```
┌─────────────────┐     ┌──────────────────┐     ┌─────────────────┐
│  Mode Adapter    │     │  Plugin Pipeline  │     │  Mode Adapter    │
│  (protocol in)   │ ──→ │  (mode-agnostic)  │ ──→ │  (protocol out)  │
│                  │     │                   │     │                  │
│  ext_proc gRPC   │     │  [jwt-validation] │     │  ext_proc resp   │
│  HTTP proxy      │     │  [audit-log]      │     │  HTTP proxy resp │
│  ext_authz gRPC  │     │  [token-exchange] │     │  ext_authz resp  │
└─────────────────┘     └──────────────────┘     └─────────────────┘
```

| Mode | Adapter In | Adapter Out | Body Access |
|------|-----------|-------------|-------------|
| envoy-sidecar | ext_proc `ProcessingRequest` → `Context` | `Context` → ext_proc `ProcessingResponse` | Only with `processing_mode: { request_body_mode: BUFFERED }` in EnvoyFilter — operator must configure this |
| proxy-sidecar | `http.Request` → `Context` | `Context` → modified `http.Request` | Full access (body is in-process) |
| waypoint | ext_authz `CheckRequest` → `Context` | `Context` → ext_authz `CheckResponse` | **Never** — ext_authz only sends headers. Hard constraint of the Envoy ext_authz API. |

Body access is a hard constraint of the deployment mode, not a soft
configuration option. The pipeline validates at startup that plugins
declaring `body_access: true` are not used in modes that cannot provide it.
This turns a silent failure (plugin never sees the body) into a loud startup
error.

For envoy-sidecar mode specifically: body access requires the Envoy
`ext_proc` filter to be configured with `processing_mode.request_body_mode:
BUFFERED`. If body-access plugins are declared, the kagenti-operator should
auto-patch the EnvoyFilter resource. This is a Phase 2 concern.

## Protocol Parsing as a Plugin

Higher-level protocol awareness (MCP, A2A, JSON-RPC) is not a special
interface — it is a plugin that parses `ctx.Body` and populates
`ctx.Values`:

```go
type MCPParser struct{}

func (p *MCPParser) OnRequest(ctx *Context) Action {
    if ctx.Body == nil {
        return Action{Type: Continue}
    }
    var rpc jsonRPCRequest
    if err := json.Unmarshal(ctx.Body, &rpc); err != nil {
        return Action{Type: Continue} // not JSON-RPC, pass through
    }
    ctx.Values["rpc.method"] = rpc.Method
    ctx.Values["rpc.id"] = rpc.ID
    if rpc.Method == "tools/call" {
        ctx.Values["mcp.tool_name"] = rpc.Params["name"]
    }
    return Action{Type: Continue}
}
```

### Identity-Aware Protocol Policy

The combination of protocol parsing and identity context enables
authorization at the tool level — something no external proxy can do today.
A `tool-policy` plugin reads the parsed MCP method from `ctx.Values` and
checks it against the caller's claims:

```yaml
pipeline:
  inbound:
    - jwt-validation
    - mcp-parser:
        body_access: true
    - tool-policy:
        rules:
          - tool: "execute_sql"
            require_scope: "sql:write"
          - tool: "read_file"
            deny_trust_domain: "external.example.com"
          - tool: "*"
            allow: all
```

The `tool-policy` plugin:
1. Reads `ctx.Values["mcp.tool_name"]` (set by `mcp-parser`)
2. Reads `ctx.Claims.Scopes` (set by `jwt-validation`)
3. Reads `ctx.Agent.TrustDomain` (always available)
4. Matches against rules and returns `Continue` or `Reject`

This is a three-layer composition — identity, protocol, policy — where
each layer is an independent plugin that communicates through `ctx`. No
plugin needs to understand the others. The same pattern works for A2A
task-level authorization.

## Praxis as Envoy Replacement

AuthBridge currently depends on Envoy for traffic interception in two of its
three modes (envoy-sidecar via ext_proc, waypoint via ext_authz). This
creates constraints: body access requires Envoy-specific configuration,
ext_authz cannot forward body data at all, and AI protocol awareness
(MCP, A2A) must be implemented entirely inside AuthBridge because Envoy
has no concept of agent protocols.

[Praxis](https://github.com/praxis-proxy/praxis) is a security-first proxy
framework designed for AI workloads. It is building native support for the
protocols AuthBridge cares about:

- [MCP Protocol Support](https://github.com/praxis-proxy/praxis/issues/25)
- [A2A Protocol Support](https://github.com/praxis-proxy/praxis/issues/26)
- [Agent Sessions](https://github.com/praxis-proxy/praxis/issues/27)

If Praxis replaces Envoy as the traffic interception layer, AuthBridge
benefits indirectly:

- **Body access becomes universal** — no more ext_proc `BUFFERED` mode
  configuration or ext_authz header-only limitation. Praxis gives full
  body access natively.
- **Protocol parsing moves to the proxy** — Praxis plugins for MCP/A2A
  parsing run at the proxy level, so AuthBridge receives pre-parsed
  protocol metadata instead of raw bytes. The `mcp-parser` plugin in this
  proposal becomes unnecessary; `ctx.Values["mcp.tool_name"]` arrives
  already populated.
- **Simpler adapter layer** — instead of three adapters (ext_proc, ext_authz,
  HTTP proxy), AuthBridge needs one adapter for Praxis's extension API.
- **Tighten-only at the proxy** — Praxis shares the tighten-only principle
  ([#63](https://github.com/praxis-proxy/praxis/issues/63)), so the
  security model composes naturally.

AuthBridge's role in a Praxis world narrows to what it does best: identity,
JWT validation, token exchange, and identity-aware policy. The proxy
infrastructure, protocol parsing, and traffic interception move to Praxis.

This is a Phase 3 consideration. The plugin architecture proposed here is
designed to work with or without Praxis — the adapter layer abstracts the
interception mechanism, and plugins are mode-agnostic by design.

## Migration Path

The existing `auth.HandleInbound` and `auth.HandleOutbound` functions become
two built-in plugins: `jwt-validation` and `token-exchange`.

### Concrete before/after

**Before** (ext_proc listener, `listener/extproc/server.go`):
```go
result := s.Auth.HandleInbound(ctx, authHeader, path, "")
if result.Action == auth.ActionDeny {
    return denyResponse(...)
}
return allowResponse()
```

**After**:
```go
pctx := adapter.FromExtProc(req)  // protocol → Context
action := s.pipeline.Run(pctx)    // run all plugins
return adapter.ToExtProc(pctx, action)  // Context → protocol
```

### How existing code maps to built-in plugins

| Current code | Becomes |
|-------------|---------|
| `Auth.verifier.Verify()` | `jwt-validation` plugin (wraps `validation.Verifier`) |
| `Auth.exchanger.Exchange()` + `Auth.cache` | `token-exchange` plugin (wraps `exchange.Client` + `cache.Cache`) |
| `Auth.bypass.Match()` | Runs **before** the pipeline (bypass is infrastructure, not a plugin) |
| `Auth.router.Resolve()` | Runs **before** the pipeline, populates `ctx.Route` |
| `Auth.identity` (atomic pointer) | Populates `ctx.Agent` before pipeline; hot-reload via `UpdateIdentity()` still works |

The `UpdateIdentity` hot-reload path is preserved: the atomic pointer
update flows through to the `jwt-validation` plugin, which reads
`ctx.Agent` for audience validation. No change to the credential resolution
goroutine.

## Example: Minimal Plugin

A complete plugin that logs every rejected inbound request:

```go
package auditreject

import (
    "log/slog"
    "github.com/kagenti/kagenti-extensions/authbridge/pipeline"
)

func init() {
    pipeline.Register("audit-reject", New)
}

func New(cfg map[string]any) (pipeline.Plugin, error) {
    return &AuditReject{}, nil
}

type AuditReject struct{}

func (a *AuditReject) Name() string { return "audit-reject" }

func (a *AuditReject) OnRequest(ctx *pipeline.Context) pipeline.Action {
    return pipeline.Action{Type: pipeline.Continue}
}

func (a *AuditReject) OnResponse(ctx *pipeline.Context) pipeline.Action {
    if ctx.Response != nil && ctx.Response.Status == 401 {
        slog.Info("rejected request",
            "host", ctx.Host,
            "path", ctx.Path,
            "agent", ctx.Agent.ClientID,
            "caller", ctx.Claims.Subject)
    }
    return pipeline.Action{Type: pipeline.Continue}
}
```

## Open Questions

1. **Body buffering budget** — max body size to buffer for body-access
   plugins? LLM prompts can exceed 100KB. Streaming bodies (SSE for MCP
   Streamable HTTP) need a different approach than buffered.
2. **Async plugins** — should a plugin be able to fire-and-forget (e.g.,
   send to an audit queue) without blocking the pipeline? The pipeline
   could offer a `ctx.Async(func())` helper that runs after the response
   is sent.
3. **ext_proc body auto-configuration** — when a body-access plugin is
   declared in envoy-sidecar mode, should the kagenti-operator auto-patch
   the EnvoyFilter to enable `request_body_mode: BUFFERED`?
4. **Plugin compatibility** — how to handle a plugin compiled against an
   older pipeline interface? Semantic versioning of the Plugin interface,
   or a version negotiation handshake at registration time?
5. **Multi-turn agent sessions** — MCP and A2A support stateful sessions.
   Should the plugin context carry session state across requests, or is
   that a plugin's own responsibility (via external storage)?

## Phases

- **Phase 1**: Plugin interface, pipeline runner, config-based ordering,
  registry loading. Refactor jwt-validation and token-exchange as built-in
  plugins. Compiled-in only. Tighten-only enforcement.
- **Phase 2**: Body access with configurable buffering. Protocol parser
  plugin (MCP/A2A). Tool-level authorization. Enable guardrails and PII
  redaction use cases. Auto-configure ext_proc body mode via operator.
- **Phase 3**: Evaluate Praxis as Envoy replacement. Evaluate WASM or
  sidecar loading for non-compiled extensibility. Multi-turn session
  context.
