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
exchanges tokens. This proposal adds an extensible filter pipeline so
additional processing — observability, guardrails, authorization, credential
privacy, egress policy — can be added without modifying the core binary.

## Design Principles

1. **Simple first version** — one interface, one context, compiled-in plugins
2. **Mutations on context** — filters modify headers/body directly on the
   context, no separate mutation API
3. **Mode-agnostic filters** — filters never see protocol details (ext_proc,
   HTTP proxy, ext_authz); a thin adapter per mode converts to/from the
   shared context
4. **Tighten-only policy** — a filter can add validation or reject requests
   but cannot bypass built-in security (e.g., cannot disable JWT validation)
5. **Declare what you need** — body access is opt-in per filter; the pipeline
   only buffers the body when at least one filter requests it

## Filter Interface

```go
// Filter is the only interface a plugin implements.
type Filter interface {
    Name() string
    OnRequest(ctx *Context) Action
    OnResponse(ctx *Context) Action
}
```

### Context

```go
// Context is what every filter receives.
type Context struct {
    Direction Direction       // Inbound or Outbound
    Method    string          // HTTP method
    Host      string          // target host (port stripped)
    Path      string          // request path
    Headers   http.Header     // read/write — mutations are applied downstream
    Body      []byte          // nil unless filter declared body_access: true
    Claims    *Claims         // populated after jwt-validation filter (nil before)
    Values    map[string]any  // shared state between filters in the chain
}
```

`Claims` contains the validated JWT claims (subject, issuer, audience,
client_id, scopes) and is nil until the jwt-validation filter runs. Filters
after jwt-validation can read `ctx.Claims` without re-parsing the token.

`Values` is a generic map for filter-to-filter communication. For example,
a protocol-parsing filter could read `ctx.Body`, parse the MCP/A2A JSON-RPC
envelope, and set `ctx.Values["mcp.tool_name"]` for downstream filters.
This keeps the core interface simple while allowing protocol-aware plugins
without a special interface.

### Action

```go
type Action struct {
    Type   ActionType
    Status int    // only for Reject
    Reason string // only for Reject
}

type ActionType int
const (
    Continue ActionType = iota // pass to next filter with any ctx mutations
    Reject                     // stop pipeline, return error to client
)
```

Mutations happen directly on `ctx.Headers`, `ctx.Body`, or `ctx.Values`.
Returning `Continue` means "proceed with whatever I changed." There is no
separate mutation action — this matches how Envoy filters work.

## Pipeline

The pipeline holds an ordered list of filters and runs them sequentially:

```go
type Pipeline struct {
    filters []Filter
}

func (p *Pipeline) Run(ctx *Context) Action {
    for _, f := range p.filters {
        action := f.OnRequest(ctx)
        if action.Type == Reject {
            return action
        }
    }
    return Action{Type: Continue}
}
```

Response filters run in reverse order (last filter sees the response first),
matching the Envoy convention:

```go
func (p *Pipeline) RunResponse(ctx *Context) Action {
    for i := len(p.filters) - 1; i >= 0; i-- {
        action := p.filters[i].OnResponse(ctx)
        if action.Type == Reject {
            return action
        }
    }
    return Action{Type: Continue}
}
```

## Pipeline Configuration

Filters are declared in `config.yaml` with explicit ordering:

```yaml
pipeline:
  inbound:
    - jwt-validation
    - audit-log:
        destination: "s3://audit-bucket"
    - guardrails:
        body_access: true
        max_prompt_tokens: 4096
        on_error: continue   # fail-open for this filter
  outbound:
    - pii-filter:
        body_access: true
    - token-exchange
    - egress-policy:
        policy_file: /etc/authbridge/egress.rego
```

Properties:
- **Ordering is explicit** — filters run in the order listed
- **`on_error`** — `reject` (default) or `continue` per filter
- **`body_access`** — opt-in; pipeline buffers the body only when needed
- **Filter-specific config** — passed to the factory as `map[string]any`

When no `pipeline` section is present in config, the default pipeline is:

```yaml
pipeline:
  inbound:
    - jwt-validation
  outbound:
    - token-exchange
```

This preserves backward compatibility — existing deployments work unchanged.

## Plugin Loading (v1: Registry)

Plugins register themselves via a factory function:

```go
// In the plugin package
func init() {
    pipeline.Register("pii-filter", NewPIIFilter)
}

func NewPIIFilter(cfg map[string]any) (Filter, error) {
    maxSize := cfg["max_size"].(int)
    return &PIIFilter{maxSize: maxSize}, nil
}
```

```go
// In main.go or a build-tag file — one import per plugin
import _ "github.com/kagenti/authbridge-plugins/pii-filter"
```

At startup the pipeline reads config, looks up each filter name in the
registry, calls the factory with the filter's config, and builds the chain.
Unknown names fail startup with a clear error.

This is compiled-in: adding a plugin means adding an import and rebuilding
the authbridge image. The tradeoff is zero runtime overhead and compile-time
safety.

### Future Loading Mechanisms

These are not part of v1 but are anticipated:

| Mechanism | Tradeoff |
|-----------|----------|
| **Go plugins** (`.so` at runtime) | More modular, but fragile — must match exact Go version and dependencies |
| **Sidecar** (gRPC call to separate container) | Any language, maximum isolation, but adds network hop per filter per request |

## Adapter Layer (Mode Abstraction)

Each deployment mode implements a thin adapter that converts between
protocol-specific types and the shared `Context`:

```
┌─────────────────┐     ┌──────────────────┐     ┌─────────────────┐
│  Mode Adapter    │     │  Filter Pipeline  │     │  Mode Adapter    │
│  (protocol in)   │ ──→ │  (mode-agnostic)  │ ──→ │  (protocol out)  │
│                  │     │                   │     │                  │
│  ext_proc gRPC   │     │  [jwt-validation] │     │  ext_proc resp   │
│  HTTP proxy      │     │  [audit-log]      │     │  HTTP proxy resp │
│  ext_authz gRPC  │     │  [token-exchange] │     │  ext_authz resp  │
└─────────────────┘     └──────────────────┘     └─────────────────┘
```

| Mode | Adapter In | Adapter Out | Body? |
|------|-----------|-------------|-------|
| envoy-sidecar | ext_proc `ProcessingRequest` → `Context` | `Context` → ext_proc `ProcessingResponse` | Only if Envoy config sends body |
| proxy-sidecar | `http.Request` → `Context` | `Context` → modified `http.Request` | Full access |
| waypoint | ext_authz `CheckRequest` → `Context` | `Context` → ext_authz `CheckResponse` | No (headers only) |

Body access depends on the mode. The pipeline validates at startup that
filters requesting `body_access: true` are only used in modes that support
it. This prevents silent failures where a PII filter is configured but never
sees the body.

## Protocol Parsing as a Filter

Higher-level protocol awareness (MCP, A2A, JSON-RPC) is not a special
interface — it's a filter that parses `ctx.Body` and populates `ctx.Values`:

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

Downstream filters (guardrails, audit) read `ctx.Values["mcp.tool_name"]`
without needing to parse JSON-RPC themselves. This keeps the core interface
at the HTTP level while enabling protocol-aware behavior through
composition.

## Praxis Alignment

[Praxis](https://github.com/praxis-proxy/praxis) is a security-first proxy
framework for AI workloads with a mature filter model that shares design
goals with this proposal:

- **Filter-first architecture** — everything is a filter, same trait for
  built-in and custom ([architecture](https://github.com/praxis-proxy/praxis/blob/main/docs/architecture.md))
- **Body access modes** — Stream, Buffer, StreamBuffer with per-filter
  declaration ([filters](https://github.com/praxis-proxy/praxis/blob/main/docs/filters.md))
- **Composable filter chains** — named chains referenced by listeners
- **Tighten-only policy hooks** — plugins can strengthen but never weaken
  security ([#63](https://github.com/praxis-proxy/praxis/issues/63))

Praxis is also building AI-specific capabilities relevant to AuthBridge:
- [MCP Protocol Support](https://github.com/praxis-proxy/praxis/issues/25)
- [A2A Protocol Support](https://github.com/praxis-proxy/praxis/issues/26)
- [Agent Sessions](https://github.com/praxis-proxy/praxis/issues/27)

There is potential for collaboration: AuthBridge provides identity, auth,
and token exchange; Praxis provides proxy infrastructure and protocol
parsing. Whether AuthBridge filters become Praxis filters, or Praxis serves
as the listener layer feeding AuthBridge's pipeline, is an open question.

## Migration Path

The existing `auth.HandleInbound` and `auth.HandleOutbound` functions become
two built-in filters: `jwt-validation` and `token-exchange`. The pipeline
replaces the direct function calls. External behavior is unchanged.

```
Before:  listener → auth.HandleInbound() → response
After:   listener → adapter → pipeline([jwt-validation, ...]) → adapter → response
```

## Example: Minimal Plugin

A complete plugin that logs every rejected inbound request:

```go
package auditreject

import (
    "log/slog"
    "github.com/kagenti/authbridge/pipeline"
)

func init() {
    pipeline.Register("audit-reject", New)
}

func New(cfg map[string]any) (pipeline.Filter, error) {
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
            "method", ctx.Method)
    }
    return pipeline.Action{Type: pipeline.Continue}
}
```

## Open Questions

1. **Body buffering budget** — max body size to buffer for body-access
   filters? LLM prompts can exceed 100KB.
2. **Async plugins** — should a filter be able to fire-and-forget (e.g.,
   send to an audit queue) without blocking the pipeline?
3. **ext_proc body access** — in envoy-sidecar mode, body access requires
   Envoy to be configured to send body chunks. Should the operator
   auto-configure this when a body-access filter is declared?
4. **Plugin compatibility** — how to handle a plugin compiled against an
   older pipeline interface? Semantic versioning of the Filter interface?

## Phases

- **Phase 1**: Filter interface, pipeline runner, config-based ordering,
  registry loading. Refactor jwt-validation and token-exchange as built-in
  filters. Compiled-in only.
- **Phase 2**: Body access with configurable buffering. Protocol parser
  filter (MCP/A2A). Enable guardrails and PII filter use cases.
- **Phase 3**: Evaluate Praxis integration for proxy infrastructure.
  Evaluate Go plugins or sidecar loading for non-compiled extensibility.
