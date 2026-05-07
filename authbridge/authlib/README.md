# authlib — Shared Auth Building Blocks

A pure Go library providing reusable building blocks for JWT validation, OAuth 2.0 token exchange, and SPIFFE-based authentication. No protocol dependencies (no gRPC, no Envoy).

## Packages

| Package | Purpose |
|---------|---------|
| `validation/` | JWKS-backed JWT verifier (`lestrrat-go/jwx`) with required audience parameter |
| `exchange/` | RFC 8693 token exchange + client credentials grant with pluggable auth |
| `cache/` | SHA-256 keyed token cache with TTL eviction |
| `bypass/` | Path pattern matcher for public endpoints (health, agent card) |
| `spiffe/` | SPIFFE credential sources (file-based JWT-SVID) |
| `routing/` | Host-to-audience router with glob pattern matching |
| `auth/` | Composition layer: `HandleInbound` + `HandleOutbound` — used internally by `jwt-validation` and `token-exchange` plugins |
| `config/` | YAML config loader, mode presets, credential-file waiters, top-level validation |
| `pipeline/` | Plugin pipeline + lifecycle (`Configurable`, `Initializer`, `Shutdowner`) — see [pipeline/README.md](./pipeline/README.md) |
| `session/` | In-memory session store + `SessionSummary` aggregation, backing the `:9094` API |
| `sessionapi/` | HTTP API (`/v1/sessions`, `/v1/events` SSE, `/v1/pipeline`) exposing the session store |
| `plugins/` | Built-in plugins: `jwt-validation`, `token-exchange`, `a2a-parser`, `mcp-parser`, `inference-parser`. See [plugins/CONVENTIONS.md](./plugins/CONVENTIONS.md) for the per-plugin config convention |
| `observe/` | OTEL + metrics helpers |

## Usage

Plugins own their own configuration and construct any `auth.Auth` they need internally from their per-plugin config (see [plugins/CONVENTIONS.md](./plugins/CONVENTIONS.md)). Host processes (cmd/authbridge) just load the YAML, call `plugins.Build`, run the pipelines, and let each plugin handle its own dependencies.

```go
import (
    "github.com/kagenti/kagenti-extensions/authbridge/authlib/config"
    "github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins"
)

cfg, _ := config.Load("config.yaml")
config.ApplyPreset(cfg)
_ = config.Validate(cfg) // mode + listener combo only; plugins self-validate

inbound,  _ := plugins.Build(cfg.Pipeline.Inbound.Plugins)
outbound, _ := plugins.Build(cfg.Pipeline.Outbound.Plugins)

_ = inbound.Start(ctx)   // invokes Init on plugins that implement pipeline.Initializer
_ = outbound.Start(ctx)

// Listeners drive the pipelines — see cmd/authbridge/listener/*
```

For direct use of the `auth/` composition layer (outside of plugins), see `auth/auth.go` — `auth.New(cfg)` takes an `auth.Config` containing the specific building blocks a caller needs.

## Go Module

```
module github.com/kagenti/kagenti-extensions/authbridge/authlib
```

Direct dependencies: `lestrrat-go/jwx/v2`, `gobwas/glob`, `gopkg.in/yaml.v3`. No gRPC or Envoy deps.
