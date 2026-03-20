# Kagenti Webhook Architecture

This document provides Mermaid diagrams illustrating the webhook architecture.

## Component Architecture

```mermaid
graph TB
    subgraph "Kubernetes API Server"
        API[API Server]
    end

    subgraph "Webhook Pod (kagenti-system)"
        MAIN[main.go]
        MUTATOR[PodMutator<br/>shared injector]

        subgraph "Webhook Handler"
            AUTH[AuthBridge Webhook]
        end

        subgraph "Builders"
            CONT[Container Builder<br/>proxy-init, envoy-proxy, spiffe-helper]
            VOL[Volume Builder]
        end
    end

    subgraph "Kubernetes Resources"
        POD[Pods at CREATE]
    end

    API -->|mutate pods| AUTH

    MAIN -->|creates & shares| MUTATOR
    MAIN -->|registers| AUTH

    AUTH -->|InjectAuthBridge| MUTATOR

    MUTATOR -->|builds containers| CONT
    MUTATOR -->|builds volumes| VOL

    AUTH -.->|modifies| POD

    style MUTATOR fill:#90EE90
    style AUTH fill:#32CD32,stroke:#006400,stroke-width:3px
    style CONT fill:#FFB6C1
    style VOL fill:#FFB6C1
    style POD fill:#87CEEB
```

## Container Injection Flow

### Default (all sidecars injected)

All four sidecars are injected when no opt-out labels are present:

```mermaid
graph LR
    subgraph "Pod Spec"
        APP[Application<br/>Container]
    end

    subgraph "AuthBridge Injection"
        INIT[proxy-init<br/>Init Container]
        ENVOY[envoy-proxy<br/>Sidecar]
        SPIFFE[spiffe-helper<br/>Sidecar]
        CLIENT[client-registration<br/>Sidecar]
    end

    subgraph "External Dependencies"
        SPIRE[SPIRE Agent]
        KC[Keycloak]
    end

    INIT -->|1. Setup iptables| ENVOY
    ENVOY -->|2. Proxy ready| APP
    SPIFFE -->|3. Get JWT-SVID| SPIRE
    SPIFFE -->|4. Write token| CLIENT
    CLIENT -->|5. Register| KC
    APP -->|All traffic via| ENVOY

    style INIT fill:#FFA500
    style ENVOY fill:#4169E1
    style SPIFFE fill:#32CD32
    style CLIENT fill:#9370DB
    style APP fill:#87CEEB
```

### Combined mode (`featureGates.combinedSidecar: true`)

All sidecars are merged into a single `authbridge` container:

```mermaid
graph LR
    subgraph "Pod Spec"
        APP[Application<br/>Container]
    end

    subgraph "AuthBridge Injection"
        INIT[proxy-init<br/>Init Container]
        AB[authbridge<br/>Combined Sidecar]
    end

    subgraph "Inside authbridge container"
        ENVOY_I[Envoy Proxy]
        PROC[go-processor]
        SPIFFE_I[spiffe-helper]
        CREG[client-registration]
    end

    subgraph "External Dependencies"
        SPIRE[SPIRE Agent]
        KC[Keycloak]
    end

    INIT -->|1. Setup iptables| AB
    SPIFFE_I -->|2. Get JWT-SVID| SPIRE
    CREG -->|3. Register| KC
    AB -->|4. Proxy ready| APP
    APP -->|All traffic via| ENVOY_I

    style INIT fill:#FFA500
    style AB fill:#4169E1
    style APP fill:#87CEEB
    style ENVOY_I fill:#4169E1
    style PROC fill:#4169E1
    style SPIFFE_I fill:#32CD32
    style CREG fill:#9370DB
```

### Without SPIRE (opt-out via `kagenti.io/spiffe-helper-inject: "false"`)

When spiffe-helper is disabled, only envoy-proxy and proxy-init are injected:

```mermaid
graph LR
    subgraph "Pod Spec"
        APP[Application<br/>Container]
    end

    subgraph "AuthBridge Injection"
        INIT[proxy-init<br/>Init Container]
        ENVOY[envoy-proxy<br/>Sidecar]
    end

    INIT -->|1. Setup iptables| ENVOY
    ENVOY -->|2. Proxy ready| APP
    APP -->|All traffic via| ENVOY

    style INIT fill:#FFA500
    style ENVOY fill:#4169E1
    style APP fill:#87CEEB
```

## Injection Decision Flow

```mermaid
graph TD
    START[Webhook Receives Admission Request]

    subgraph "Stage 1 — PodMutator pre-filters"
        CHECK_TYPE{kagenti.io/type\n= agent or tool?}
        CHECK_GLOBAL{featureGates\n.globalEnabled?}
        CHECK_TOOLS{type=tool AND\nfeatureGates.injectTools=false?}
        CHECK_OPTOUT{kagenti.io/inject\n= disabled?}
    end

    subgraph "Stage 2 — PrecedenceEvaluator per sidecar"
        EVAL[Per-sidecar 2-layer chain\nL1: per-sidecar feature gate\nL2: workload opt-out label]
        INJECT[Inject sidecars per decision\nproxy-init follows envoy-proxy]
    end

    SKIP[Skip Injection]

    START --> CHECK_TYPE
    CHECK_TYPE -->|No| SKIP
    CHECK_TYPE -->|Yes| CHECK_GLOBAL
    CHECK_GLOBAL -->|false| SKIP
    CHECK_GLOBAL -->|true| CHECK_TOOLS
    CHECK_TOOLS -->|Yes| SKIP
    CHECK_TOOLS -->|No| CHECK_OPTOUT
    CHECK_OPTOUT -->|disabled| SKIP
    CHECK_OPTOUT -->|other/absent| EVAL
    EVAL --> INJECT

    style INJECT fill:#32CD32
    style SKIP fill:#D3D3D3
```

## Sidecar Injection Rules

### Stage 1 pre-filters (all-or-nothing)

| # | Check | Label / Config | Skip condition |
| --- | --- | --- | --- |
| 1 | Workload type | `kagenti.io/type` on workload | Not `agent` or `tool` |
| 2 | Global kill switch | `featureGates.globalEnabled` in Helm values | `false` |
| 3 | Tool gate | `featureGates.injectTools` in Helm values | Type is `tool` AND gate is `false` (default) |
| 4 | Whole-workload opt-out | `kagenti.io/inject: disabled` on workload | Label explicitly set |

### Stage 2 per-sidecar chain (independent per sidecar)

| Layer | Config | Effect |
| --- | --- | --- |
| 1. Per-sidecar feature gate | `featureGates.envoyProxy / .spiffeHelper / .clientRegistration` | Disables sidecar cluster-wide |
| 2. Workload opt-out label | `kagenti.io/envoy-proxy-inject: "false"` etc. on pod template | Disables sidecar for that workload |

`proxy-init` is not independently evaluated — it always mirrors the `envoy-proxy` decision.

### Combined sidecar mode

| Config | Default | Effect |
| --- | --- | --- |
| `featureGates.combinedSidecar` | `false` | When `true`, injects a single `authbridge` container instead of separate `envoy-proxy` + `spiffe-helper` + `kagenti-client-registration` containers. `proxy-init` is still a separate init container. Per-sidecar gates/labels control flags passed to the combined entrypoint. |
