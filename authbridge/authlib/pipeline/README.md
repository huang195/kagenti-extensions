# pipeline — Plugin Pipeline Specification

The `pipeline` package defines AuthBridge's plugin contract: how plugins are written, how they communicate through shared state, how they compose into ordered chains, and how those chains run inside each of the three listener modes (ext_proc, ext_authz, forward/reverse proxy).

This document is both an **internal reference** for AuthBridge contributors and an **external integration spec** for teams building plugin frameworks that want to run alongside or inside AuthBridge — e.g. a **cpex-bridge** plugin that hosts a CPEX sub-pipeline as a single AuthBridge plugin.

**Audience:**
- Go developers adding plugins to AuthBridge's native chain.
- Platform teams integrating an external plugin runtime with AuthBridge.
- Anyone debugging the plugin flow via `abctl` or the `:9094` session API.

**Scope:**
- The Go surface in `authbridge/authlib/pipeline/` and `authbridge/authlib/session/`.
- The observability contract carried by `SessionEvent` on the `:9094` API.
- What the pipeline *does* and *does not* own at the boundary with the listener.

---

## 1. Mental model

AuthBridge intercepts HTTP traffic in two directions and runs a **separate plugin chain** for each. Each chain has two **phases** — request (headers/body going to the upstream) and response (headers/body coming back).

```
          Inbound (caller → this agent)
          ┌────────────────────────────────────────────────────┐
          │  Request phase  →  jwt-validation                  │
          │                 →  a2a-parser                      │
          │                 →  session-recorder   (implicit)   │
          │  Response phase ←  a2a-parser OnResponse           │
          │                 ←  jwt-validation OnResponse       │
          └────────────────────────────────────────────────────┘

          Outbound (this agent → target service)
          ┌────────────────────────────────────────────────────┐
          │  Request phase  →  route-resolver                  │
          │                 →  token-exchange                  │
          │                 →  mcp-parser / inference-parser   │
          │  Response phase ←  mcp-parser / inference-parser   │
          │                 ←  token-exchange OnResponse       │
          └────────────────────────────────────────────────────┘
```

**Key properties:**
- Plugins execute **sequentially** within a phase.
- Response phase runs plugins in **reverse order** (last plugin sees the response first — LIFO, matches middleware conventions).
- Inbound and outbound are **separate `Pipeline` instances**. A plugin that cares about both directions is registered on both.
- All state shared between plugins within one request/response cycle lives on `*pipeline.Context` (`pctx`).
- Cross-request state (per-session telemetry) lives in the `session.Store`, accessed read-only via `pctx.Session`.

---

## 2. The `Plugin` interface

```go
type Plugin interface {
    Name() string
    Capabilities() PluginCapabilities
    OnRequest(ctx context.Context, pctx *Context) Action
    OnResponse(ctx context.Context, pctx *Context) Action
}
```

### `Name() string`
A stable identifier. Used for logs, metrics, `GetState`/`SetState` keys (by convention), and pipeline introspection (`GET /v1/pipeline`).

### `Capabilities() PluginCapabilities`

```go
type PluginCapabilities struct {
    Reads      []string // extension slot names this plugin reads
    Writes     []string // extension slot names this plugin writes
    BodyAccess bool     // whether this plugin needs request/response body buffered
}
```

Declared once per plugin instance. `pipeline.New` validates that every `Read` is satisfied by an earlier plugin's `Write` — a plugin that depends on `mcp` being populated cannot be registered before `mcp-parser`. A mis-ordered registration fails fast at startup with:

```
plugin "guardrail" reads slot "mcp" but no earlier plugin writes it
```

`BodyAccess: true` on *any* plugin in a chain causes `Pipeline.NeedsBody()` to return true, which the **listener** uses to negotiate Envoy's `ProcessingMode` (BUFFERED vs HEADERS-only). Without this, the gRPC ext_proc server never asks for the body and parsers see `pctx.Body == nil`.

### `OnRequest(ctx, pctx) Action`
Called when a request is entering the pipeline. Plugins typically read request headers / body, mutate one or more extension slots, and return `Continue` or `Reject`.

### `OnResponse(ctx, pctx) Action`
Called after the upstream returns. `pctx.StatusCode`, `pctx.ResponseHeaders`, and `pctx.ResponseBody` are populated. Plugins typically enrich the telemetry extensions with response-side data (completion text, token usage, error code) or apply guardrails on the response content.

Plugins that only care about the request set `OnResponse` to a no-op (`return Action{Type: Continue}`); same for response-only plugins on `OnRequest`.

---

## 3. `pipeline.Context` — the shared state

The entire surface a plugin sees:

```go
type Context struct {
    Direction Direction        // Inbound | Outbound
    Method    string           // HTTP method
    Host      string           // :authority / Host
    Path      string           // :path
    Headers   http.Header
    Body      []byte           // nil unless a plugin declared BodyAccess: true
    StartedAt time.Time        // listener wall-clock at request entry

    Agent   *AgentIdentity     // this workload's SPIFFE / Keycloak identity
    Claims  *validation.Claims // inbound caller's JWT claims after jwt-validation
    Route   *routing.ResolvedRoute // outbound: resolved audience / token scopes
    Session *SessionView      // read-only view of the session bucket

    // Response-phase fields (populated by listener before RunResponse)
    StatusCode      int
    ResponseHeaders http.Header
    ResponseBody    []byte

    Extensions Extensions
}
```

**Ownership rules:**
- Plugins **read** any field they declared in `Capabilities.Reads`.
- Plugins **write** fields they declared in `Capabilities.Writes`. By convention each extension slot has exactly one writer (the parser plugin).
- `Claims` is populated by `jwt-validation` and is read-only afterward.
- `Agent`, `Route`, `Session` are populated by the listener before `Run`. Plugins treat them as read-only.
- `ResponseBody` appears between `Run` and `RunResponse` — plugins must not read it in `OnRequest`.

**Lifetime:** one `*Context` per HTTP transaction. Not reused across requests. Single-threaded — the pipeline guarantees sequential invocation of plugins within a phase, so plugins don't need internal locking for pctx reads/writes.

---

## 4. `Extensions` — typed plugin-to-plugin communication

```go
type Extensions struct {
    MCP        *MCPExtension
    A2A        *A2AExtension
    Security   *SecurityExtension
    Delegation *DelegationExtension
    Inference  *InferenceExtension
    Custom     map[string]any
}
```

Two categories:

### Named slots (telemetry-worthy)
MCP, A2A, Security, Delegation, Inference. These are:
- Part of the **published schema** carried on `SessionEvent` to `:9094` / `abctl`.
- Consumable by multiple downstream plugins.
- Added to the core struct only when the data has a public contract.

Adding a named slot is an authlib-core change: edit `Extensions`, add a `SessionEventWire` field, update `snapshotXXX` helpers in the listener, and add filtering rules in `abctl`.

### `Custom map[string]any` + `GetState[T]`/`SetState[T]` (plugin-private)
For state that's internal to a single plugin or to a bridge's sub-pipeline:

```go
// Plugin's private state type:
type rlState struct {
    TokensAtStart int
    Decision      string
}

// In OnRequest:
pipeline.SetState(pctx, "rate-limiter", &rlState{TokensAtStart: 100})

// In OnResponse:
s := pipeline.GetState[rlState](pctx, "rate-limiter")
if s != nil { /* use s */ }
```

Convention: **key = plugin's Name()** so collisions across plugins don't happen. Storage is lazy (`Custom` is nil-initialized until first write).

`GetState[T]` type-asserts and returns `nil` on mismatch instead of panicking — a plugin whose type evolves across versions degrades gracefully.

### Built-in extension shapes

All at `authbridge/authlib/pipeline/extensions.go`:

```go
type MCPExtension struct {
    Method string          // JSON-RPC method, e.g. "tools/call"
    RPCID  any             // JSON-RPC id (could be int or string)
    Params map[string]any  // request params
    Result map[string]any  // response result (mutually exclusive with Err)
    Err    *MCPError
}

type A2AExtension struct {
    Method      string
    RPCID       any
    SessionID   string  // contextId from the client, or server-assigned on first turn
    MessageID   string
    TaskID      string
    Role        string  // "user" | "agent"
    Parts       []A2APart
    FinalStatus string  // response: "completed" | "failed" | "canceled"
    Artifact    string  // response: assembled artifact text
    ErrorMessage string // response: failure reason
}

type InferenceExtension struct {
    // Request side:
    Model       string
    Messages    []InferenceMessage
    Temperature *float64
    MaxTokens   *int
    TopP        *float64
    Stream      bool
    Tools       []InferenceTool  // full definition incl. parameters schema
    ToolChoice  any
    // Response side:
    Completion       string
    FinishReason     string
    PromptTokens     int
    CompletionTokens int
    TotalTokens      int
    ToolCalls        []InferenceToolCall
}

type SecurityExtension struct {
    Labels      []string // classifier / guardrail output
    Blocked     bool
    BlockReason string
}

type DelegationExtension struct {
    Origin string   // original caller subject
    Actor  string   // current actor subject
    // chain is append-only via AppendHop; reads via Chain()
}
```

Mutability: **always assigned, never mutated in place** after the parser sets the slot. This guarantees that `snapshotXXX` in the listener (shallow-copy for event recording) stays correct even when OnResponse enriches the struct — the response snapshot is taken from the now-enriched pointer, but any earlier request-phase snapshot was taken of a frozen copy.

---

## 5. `Action` — control flow

```go
type Action struct {
    Type      ActionType // Continue | Reject
    Violation *Violation // populated iff Type == Reject
}

type Violation struct {
    // Structured (mirrors CPEX's PluginViolation):
    Code        string         // machine-readable, e.g. "auth.missing-token"
    Reason      string         // short human message
    Description string         // longer explanation; optional
    Details     map[string]any // plugin-arbitrary structured context; optional

    // HTTP rendering hints — all optional; defaults from Code:
    Status   int         // when 0, StatusFromCode(Code) is used
    Body     []byte      // when nil, synthesized JSON
    BodyType string      // Content-Type for Body; defaults to application/json
    Headers  http.Header // merged into the response (e.g. WWW-Authenticate, Retry-After)

    // Framework-populated from Plugin.Name(); plugins leave it empty:
    PluginName string
}
```

Returning `Reject` from `OnRequest` halts the request pipeline; from `OnResponse` halts the response pipeline. The listener calls `Violation.Render()` to produce `(status, headers, body)` and emits that as the HTTP response. The default body when `Body` is nil:

```json
{
  "error":       "auth.missing-token",
  "message":     "Bearer token required",
  "description": "No Authorization header present",
  "plugin":      "jwt-validation",
  "details":     { "realm": "kagenti" }
}
```

Helper constructors cover the common cases so the reject site stays one line:

```go
pipeline.Deny("auth.invalid-token", "token expired")
pipeline.DenyStatus(451, "policy.forbidden", "unavailable for legal reasons")
pipeline.DenyWithDetails("policy.rate-limited", "quota hit", map[string]any{
    "remaining": 0, "window": "1h",
})
pipeline.Challenge("kagenti", "Authorization required")   // 401 + WWW-Authenticate
pipeline.RateLimited(30*time.Second, "", "slow down")     // 429 + Retry-After
```

The `Code` → HTTP-status mapping for well-known codes lives at `codeToStatus` in `action.go`; unknown codes default to 500. Plugins that need a non-default status set `Violation.Status` explicitly or use `DenyStatus`.

There is no "soft error" channel today — a plugin that wants to fail open logs and returns `Continue`. A future iteration may add a per-plugin `on_error` policy (cf. CPEX: `fail | ignore | disable`).

---

## 6. `Pipeline` — composition and execution

```go
func New(plugins []Plugin, opts ...Option) (*Pipeline, error)
func (p *Pipeline) Run(ctx context.Context, pctx *Context) Action           // request phase
func (p *Pipeline) RunResponse(ctx context.Context, pctx *Context) Action   // response phase (reverse)
func (p *Pipeline) Start(ctx context.Context) error                          // invoke Init on Initializer plugins
func (p *Pipeline) Stop(ctx context.Context)                                 // invoke Shutdown on Shutdowner plugins
func (p *Pipeline) Plugins() []Plugin                                        // defensive copy
func (p *Pipeline) NeedsBody() bool                                          // OR over all plugins' BodyAccess
```

`New` validates capability wiring at startup: every `Read` must be satisfied by some earlier plugin's `Write`.

### Plugin lifecycle (`Start` / `Stop`)

Plugins that need one-time setup (load a model, warm a cache, register metrics, spawn a background goroutine) implement the optional `Initializer` interface:

```go
type Initializer interface {
    Init(ctx context.Context) error
}
```

Plugins that need graceful cleanup (flush audit events, close a connection, cancel a goroutine) implement `Shutdowner`:

```go
type Shutdowner interface {
    Shutdown(ctx context.Context) error
}
```

Both are **optional** via Go's type-assertion idiom — a plugin that doesn't need them simply doesn't implement them, and the pipeline skips it. Existing plugins (jwt-validation, a2a-parser, mcp-parser, inference-parser, token-exchange) don't implement these; they keep working unchanged.

The host (e.g. `cmd/authbridge/main.go`) drives the lifecycle:

```go
// After pipeline.New, before listeners accept traffic:
initCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
defer cancel()
if err := inboundPipeline.Start(initCtx); err != nil {
    log.Fatalf("inbound pipeline Start: %v", err) // fail-fast on bad plugin init
}
if err := outboundPipeline.Start(initCtx); err != nil {
    log.Fatalf("outbound pipeline Start: %v", err)
}

// ... serve traffic ...

// After listeners have drained on SIGTERM:
outboundPipeline.Stop(shutdownCtx) // reverse order within each pipeline
inboundPipeline.Stop(shutdownCtx)
```

Semantics:
- `Start` — Init runs **in declaration order**, fails fast on the first error. The returned error names the offending plugin. No Shutdown is invoked on plugins whose Init already ran successfully — the intent is hard-fail on startup, not unwind.
- `Stop` — Shutdown runs **in reverse declaration order (LIFO)** so a plugin that depends on an earlier plugin's resources can still use them while cleaning up. Best-effort: errors from one Shutdown are logged but do not stop the sequence. Bounded by the caller's ctx deadline.

A minimal Init/Shutdown plugin example — a rate-limiter that refreshes its quota store in the background:

```go
type RateLimiter struct {
    store  *quotaStore
    cancel context.CancelFunc
}

func (p *RateLimiter) Name() string { return "rate-limiter" }
func (p *RateLimiter) Capabilities() pipeline.PluginCapabilities { return pipeline.PluginCapabilities{} }

func (p *RateLimiter) Init(ctx context.Context) error {
    p.store = newQuotaStore()
    bg, cancel := context.WithCancel(context.Background())
    p.cancel = cancel
    go p.store.refreshLoop(bg, 10*time.Second) // lives until Shutdown
    return nil
}

func (p *RateLimiter) Shutdown(ctx context.Context) error {
    p.cancel()             // stop the refresh loop
    return p.store.flush(ctx) // best-effort write-back of pending counters
}

func (p *RateLimiter) OnRequest(ctx context.Context, pctx *pipeline.Context) pipeline.Action {
    if !p.store.allow(pctx) {
        return pipeline.RateLimited(30*time.Second, "", "quota exceeded")
    }
    return pipeline.Action{Type: pipeline.Continue}
}

func (p *RateLimiter) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
    return pipeline.Action{Type: pipeline.Continue}
}
```

### Extension slots known to the validator

Built-in: `mcp`, `a2a`, `security`, `delegation`, `inference`, `custom`.

**For bridge plugins that write new slot names:** use the `WithSlots` option:

```go
pipeline, err := pipeline.New(plugins,
    pipeline.WithSlots("cpex-meta", "cpex-framework", "cpex-provenance"))
```

This tells the validator those slot names are legal, so a downstream native plugin can `Capabilities.Reads = []string{"cpex-meta"}` without being rejected as "unknown slot".

### Execution order
- Request phase: `plugins[0].OnRequest → plugins[1].OnRequest → …`
- Response phase: `plugins[N-1].OnResponse → plugins[N-2].OnResponse → …`
- A `Reject` from any plugin halts its phase immediately.
- `ctx.Err() != nil` between plugins also halts with `Reject{Status: 499}`.

### Concurrency model
Always sequential. No priority / mode / fire-and-forget semantics yet. This is the 80% case for auth-and-parse pipelines; richer modes would come with a CPEX-style executor if introduced.

---

## 7. `Session` + `SessionEvent` — the observability side-channel

The pipeline itself is **in-band** (plugins alter request handling). Alongside it runs an **out-of-band** observability layer: the listener snapshots `pctx` into a `SessionEvent` after each phase and appends it to a per-session bucket in the `session.Store`. This store is what powers the `:9094` HTTP API and `abctl`.

```go
type SessionEvent struct {
    SessionID      string              // bucket the event landed in
    At             time.Time
    Direction      Direction           // inbound | outbound
    Phase          SessionPhase        // request | response
    A2A            *A2AExtension       // snapshot of pctx.Extensions.A2A
    MCP            *MCPExtension
    Inference      *InferenceExtension
    Identity       *EventIdentity      // Subject, ClientID, AgentID, Scopes
    StatusCode     int                 // response phase only
    Error          *EventError         // populated on 4xx/5xx
    Host           string              // :authority
    TargetAudience string              // outbound: resolved OAuth audience
    Duration       time.Duration       // response: wall-clock since request entry
}
```

**Plugins do not touch `SessionEvent` directly.** The listener records events; plugins only read `pctx.Session` (a `*SessionView`) when they want to correlate the current request with prior ones in the same conversation — e.g. a rate-limiter that counts a session's inference events.

Wire format (`SessionEvent.MarshalJSON`) translates enums to strings and `Duration` to `DurationMs`. Round-trip stable — `json.Marshal(e) → json.Unmarshal → json.Marshal` is byte-identical. Tested at `pipeline/session_test.go:TestSessionEvent_JSONRoundTrip`.

---

## 8. Boundary: pipeline vs listener

The pipeline **does not own**:

| Concern | Owner | Why |
|---|---|---|
| HTTP wire protocol (ext_proc gRPC, ext_authz, reverse/forward proxy) | `cmd/authbridge/listener/` | Each mode speaks a different wire; pipeline stays protocol-free |
| Body buffering negotiation (`ProcessingMode: BUFFERED`) | Listener reads `Pipeline.NeedsBody()` | Only listener can respond to the ext_proc handshake |
| JWT issuance, client registration, Keycloak admin calls | Outside the pipeline (agent sidecars / kagenti-operator) | Async concerns happening before/after any request flow |
| Session store writes (`Store.Append`) | Listener, called after each phase | Plugins see only the read-only `SessionView` |
| SSE streaming of events to abctl | `authlib/sessionapi` | Observability API, not a plugin concern |

The pipeline **does own**:
- The `Plugin` interface contract.
- `pipeline.Context` structure and invariants.
- Validation of capability wiring at construction.
- Sequential dispatch and reject-short-circuit semantics.
- Typed extension slots and `GetState`/`SetState` helpers.
- The session-event *shape* (the listener uses it but doesn't define it).

---

## 9. Writing a native plugin — a worked example

A minimal outbound plugin that stamps an extra header on any request routed to GitHub:

```go
package myplugins

import (
    "context"
    "github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

type GitHubStamper struct{}

func (GitHubStamper) Name() string { return "github-stamper" }

func (GitHubStamper) Capabilities() pipeline.PluginCapabilities {
    return pipeline.PluginCapabilities{
        // We don't need body buffering, and don't depend on other plugins' data.
    }
}

func (GitHubStamper) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
    if pctx.Route != nil && pctx.Route.Audience == "github-tool" {
        pctx.Headers.Set("x-from-authbridge", "1")
    }
    return pipeline.Action{Type: pipeline.Continue}
}

func (GitHubStamper) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
    return pipeline.Action{Type: pipeline.Continue}
}
```

Registered in the outbound pipeline alongside the built-in plugins at startup in `cmd/authbridge/main.go`.

A plugin that reads the caller's SPIFFE ID from inbound claims and records a per-session counter via `GetState`/`SetState`:

```go
type sessionCounter struct{ N int }

func (p *Counter) Name() string { return "turn-counter" }

func (p *Counter) Capabilities() pipeline.PluginCapabilities {
    return pipeline.PluginCapabilities{Reads: []string{"a2a"}}
}

func (p *Counter) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
    if pctx.Direction != pipeline.Inbound || pctx.Extensions.A2A == nil {
        return pipeline.Action{Type: pipeline.Continue}
    }
    state := pipeline.GetState[sessionCounter](pctx, p.Name())
    if state == nil {
        state = &sessionCounter{}
        pipeline.SetState(pctx, p.Name(), state)
    }
    state.N++
    if state.N > 10 {
        return pipeline.RateLimited(30*time.Second,
            "policy.rate-limited", "per-session turn limit exceeded")
    }
    return pipeline.Action{Type: pipeline.Continue}
}
```

Plugins express rejections through a structured `Violation` carrying a
machine-readable `Code`, a short `Reason`, an optional longer
`Description`, a `Details` map for plugin-arbitrary context, and HTTP-
rendering hints (`Status`, `Body`, `BodyType`, `Headers`). Default JSON
body synthesis covers the 95% case — set the hints only when overriding.
Helper constructors (`Deny`, `DenyStatus`, `DenyWithDetails`,
`Challenge`, `RateLimited`) make the common cases one-liners. See
`action.go` for the full surface and `action_test.go` for worked
examples.

---

## 10. Integration spec for bridge plugins (e.g. cpex-bridge)

A **bridge plugin** is a single AuthBridge plugin that hosts a foreign runtime (CPEX, WASM, gRPC-dispatched, etc.) and delegates its work to plugins written in that runtime. From AuthBridge's perspective, the bridge is one plugin; internally it runs its own sub-pipeline.

### What the bridge plugin must implement

The standard `Plugin` interface. Specifically:

```go
func (b *CpexBridge) Name() string {
    return "cpex-bridge" // or a more specific name per instance
}

func (b *CpexBridge) Capabilities() pipeline.PluginCapabilities {
    return pipeline.PluginCapabilities{
        // Union of what the hosted CPEX plugins want:
        Reads:      []string{"a2a", "mcp", "inference"}, // everything we want to pass in
        Writes:     []string{"security", "cpex-meta"},   // what CPEX plugins produce
        BodyAccess: true,                                 // if any hosted plugin needs body
    }
}

func (b *CpexBridge) OnRequest(ctx context.Context, pctx *pipeline.Context) pipeline.Action {
    return b.runCpexHook(ctx, pctx, "on_request") // or tool_pre_invoke, prompt_pre_fetch, etc.
}

func (b *CpexBridge) OnResponse(ctx context.Context, pctx *pipeline.Context) pipeline.Action {
    return b.runCpexHook(ctx, pctx, "on_response")
}
```

### The data marshaling contract

For each AuthBridge invocation, `runCpexHook` needs to:

1. **Serialize `pctx` into the CPEX `Extensions`** (MessagePack per CPEX spec). The mapping is straightforward because the extension slot names overlap:

| AuthBridge slot | CPEX slot | Notes |
|---|---|---|
| `pctx.Extensions.MCP` | `mcp` | Same shape (method, rpcId, params, result, err) |
| `pctx.Extensions.A2A` | `agent` or `framework` | Not an exact match — the A2A-specific fields (contextId, taskId, artifact) need a CPEX equivalent |
| `pctx.Extensions.Inference` | `llm` / `completion` | Split across two CPEX slots per their conventions |
| `pctx.Extensions.Security` | `security` | Same shape |
| `pctx.Extensions.Delegation` | `delegation` | Same shape |
| `pctx.Headers`, `pctx.Path`, `pctx.Host`, `pctx.Method` | `http` | CPEX's http slot |
| `pctx.Claims`, `pctx.Agent` | `meta` / `identity` | Caller identity |
| `pctx.Body`, `pctx.ResponseBody` | `request.body` / payload arg | Raw bytes |

2. **Pick a hook name** based on AuthBridge's (direction, phase) pair. Because CPEX doesn't model direction, encode it in the hook name:

| AuthBridge | CPEX hook name |
|---|---|
| Inbound request | `inbound_request` |
| Inbound response | `inbound_response` |
| Outbound request | `outbound_request` (or `tool_pre_invoke` when MCP is set) |
| Outbound response | `outbound_response` (or `tool_post_invoke` when MCP is set, `cmf.llm_output` when Inference is set) |

3. **Invoke `manager.InvokeByName(hook, payloadType, payload, extensions, contextTable)`**. Thread `contextTable` across the lifetime of one `pctx` — suggested wiring: store it under `pipeline.SetState(pctx, "cpex-bridge", &cpexState{Table: table})` so the response phase picks it up.

4. **Deserialize the returned `PipelineResult`** back onto pctx:

| CPEX → AuthBridge |
|---|
| `ContinueProcessing: true` → return `pipeline.Action{Type: pipeline.Continue}` |
| `Violation` set → return a `pipeline.Action` whose `Violation` carries `Code`, `Reason`, `Description`, `Details`, plus any HTTP hints. The two violation structs are near-isomorphic; most fields copy 1:1. Use `pipeline.DenyWithDetails(v.Code, v.Reason, v.Details)` for the common case and set `Description`/`Status`/`Headers` on the returned `Violation` pointer as needed. |
| `ModifiedExtensions.security` changed → copy back into `pctx.Extensions.Security` |
| `ModifiedPayload` set → copy into `pctx.Body` or `pctx.ResponseBody` |
| `Errors[]` (non-fatal) → log, continue |

### Declaring new extension slots

If CPEX plugins produce typed output that other native AuthBridge plugins want to consume (e.g. a classifier's labels), the bridge writes them into `pctx.Extensions.Custom["cpex-<slotname>"]` or adds them via `WithSlots`:

```go
pipeline.New(plugins, pipeline.WithSlots("cpex-framework", "cpex-provenance"))
```

Downstream plugins then declare `Capabilities.Reads: []string{"cpex-framework"}` and the validator is happy.

### Capability declaration strategy

Two viable approaches:

**(a) Union-declared capabilities.** The bridge declares `Reads`/`Writes` as the union of what all its hosted plugins need. Simple, but loses per-hosted-plugin granularity in AuthBridge's validator.

**(b) Single-plugin-per-bridge-instance.** Each hosted CPEX plugin gets its own `CpexBridge` instance, with precise capabilities matching that one plugin. AuthBridge pipeline ordering is then granular, at cost of multiple bridge instances.

Recommended for v1: **(a)**. Fold many CPEX plugins into one bridge instance; trust the CPEX manager for internal ordering. Moves the ordering problem to CPEX's `priority`/`mode` config — its strength.

### Observability

The bridge should populate `pctx.Extensions.Security` (or any named slot) when its hosted plugins emit labels/decisions — those fields propagate to `SessionEvent` and show up in `abctl` automatically.

For CPEX-private metadata that shouldn't appear in named slots, either:
- Put it on `pctx.Extensions.Custom["cpex-<name>"]` — not surfaced by default in `abctl` but captured in snapshot.
- Or propose a new named slot (and add it to `Extensions`) if it's valuable to multiple bridge consumers.

### What the bridge MUST NOT do

- **Mutate `pctx` fields it didn't declare in `Capabilities.Writes`.** The validator doesn't enforce at runtime, but downstream plugins will malfunction if invariants are broken.
- **Block the goroutine forever.** `OnRequest`/`OnResponse` are in the request-handling hot path. CPEX `mode: fire_and_forget` needs to be scheduled onto a background goroutine by the bridge, not kept inline.
- **Assume a persistent `pctx` across requests.** Each HTTP transaction gets a fresh pctx. Cross-request state lives in `pctx.Session` (read-only) or in the bridge's own storage keyed by session ID.

---

## 11. Open questions

- **Directional hook mapping.** `inbound_request` / `outbound_response` are proposals; final naming should be agreed with the CPEX team.
- **Execution modes.** AuthBridge is sequential-only today; hosting CPEX's concurrent / fire-and-forget modes inside the bridge requires the bridge to schedule them off-thread and discard their results for response-shaping purposes. Workable; needs contract clarity.
- **Multi-bridge instances.** Running two bridges (e.g. `cpex-bridge-inbound-guardrails` + `cpex-bridge-outbound-observability`) with distinct config is supported by the interface but hasn't been exercised.
- **Priority / on-error policies.** Native AuthBridge plugins don't have these; if they become important, they'd be added to `PluginCapabilities` (or a sibling metadata struct) and interpreted by `Pipeline` — not deferred to the bridge.
- **Body mutation semantics.** Today plugins generally don't rewrite `pctx.Body` or `pctx.ResponseBody`. CPEX's `modify_payload` would need a clear contract about whether downstream native plugins see the modified or original bytes.

---

## 12. Versioning

The plugin interface is **not** semver-stable yet (AuthBridge is pre-1.0). Changes since the initial release:
- Added `BodyAccess` to `PluginCapabilities`.
- Added `WithSlots` to `New` for bridge-plugin slot registration.
- Added `GetState[T]` / `SetState[T]` generic helpers.
- Extended `A2AExtension` with response-side fields (TaskID, FinalStatus, Artifact, ErrorMessage).
- Extended `InferenceExtension` with structured tools + tool calls + TopP / ToolChoice.
- Added `SessionEvent.MarshalJSON`/`UnmarshalJSON` round-trip contract.
- **Breaking**: replaced `Action.Status`/`Action.Reason` with `Action.Violation` (see §5). Migration: use `Deny()`, `DenyStatus()`, `Challenge()`, `RateLimited()` helpers.
- Added optional `Initializer` / `Shutdowner` interfaces + `Pipeline.Start` / `Pipeline.Stop` (see §6). Existing plugins are unaffected because the interfaces are opt-in via type-assertion.

Breaking changes will be announced in `authbridge/CHANGELOG.md` (TBD) before a 1.0 tag.

---

## 13. Cross-references

- `pipeline.go` — `Pipeline` type, `New`, `Run`, `RunResponse`, `Start`, `Stop`, `Plugins`, `NeedsBody`.
- `plugin.go` — `Plugin` interface, `PluginCapabilities`, `Initializer`, `Shutdowner`.
- `action.go` — `Action`, `ActionType`, `Violation`, helper constructors (`Deny`, `DenyStatus`, `DenyWithDetails`, `Challenge`, `RateLimited`), `StatusFromCode`.
- `context.go` — `Context`, `Direction`, `AgentIdentity`.
- `extensions.go` — named extension types + `GetState`/`SetState`.
- `session.go` — `SessionEvent`, `SessionView`, `SessionPhase`, marshalers.
- `authlib/session/` — `Store`, `SessionSummary`, ring buffer, TTL / max-events caps.
- `authlib/sessionapi/` — HTTP API (`/v1/sessions`, `/v1/events`, `/v1/pipeline`) surfacing all of the above.
- `cmd/authbridge/listener/extproc/` — reference usage for all three phases.
- `cmd/abctl/` — TUI consumer of the session API, useful as a reference integrator.
