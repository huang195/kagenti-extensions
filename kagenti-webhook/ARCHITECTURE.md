# Kagenti Webhook Architecture

This document provides Mermaid diagrams illustrating the webhook architecture.

## ⚠️ Architecture Update

**The Agent CR webhook is deprecated**. The AuthBridge webhook is the recommended approach for new deployments.

## Component Architecture

```mermaid
graph TB
    subgraph "Kubernetes API Server"
        API[API Server]
    end

    subgraph "Webhook Pod (kagenti-system)"
        MAIN[main.go]
        MUTATOR[PodMutator<br/>shared injector]

        subgraph "Active Webhooks"
            AUTH[AuthBridge Webhook<br/>⭐ RECOMMENDED]
        end

        subgraph "Legacy Webhooks (Deprecated)"
            AGENT[Agent Webhook<br/>⚠️ DEPRECATED]
        end

        subgraph "Builders"
            CONT[Container Builder<br/>proxy-init, envoy-proxy, spiffe-helper]
            VOL[Volume Builder]
            NS[Namespace Checker]
        end
    end

    subgraph "Kubernetes Resources"
        subgraph "Standard Workloads (AuthBridge)"
            DEPLOY[Deployments]
            STS[StatefulSets]
            DS[DaemonSets]
            JOB[Jobs/CronJobs]
        end

        subgraph "Custom Resources (Legacy)"
            AGENTCR[Agent CR<br/>agent.kagenti.dev<br/>⚠️ DEPRECATED]
        end

        NAMESPACE[Namespace<br/>with labels/annotations]
    end

    API -->|mutate workloads| AUTH
    API -->|mutate Agent| AGENT

    MAIN -->|creates & shares| MUTATOR
    MAIN -->|registers| AUTH
    MAIN -->|registers| AGENT

    AUTH -->|InjectAuthBridge| MUTATOR
    AGENT -->|MutatePodSpec| MUTATOR

    MUTATOR -->|builds containers| CONT
    MUTATOR -->|builds volumes| VOL
    MUTATOR -->|checks injection| NS

    NS -->|reads| NAMESPACE
    AUTH -.->|modifies| DEPLOY
    AUTH -.->|modifies| STS
    AUTH -.->|modifies| DS
    AUTH -.->|modifies| JOB
    AGENT -.->|modifies| AGENTCR

    style MUTATOR fill:#90EE90
    style AUTH fill:#32CD32,stroke:#006400,stroke-width:3px
    style AGENT fill:#D3D3D3,stroke:#808080,stroke-dasharray: 5 5
    style CONT fill:#FFB6C1
    style VOL fill:#FFB6C1
    style NS fill:#FFB6C1
    style DEPLOY fill:#87CEEB
    style STS fill:#87CEEB
    style DS fill:#87CEEB
    style JOB fill:#87CEEB
    style AGENTCR fill:#D3D3D3,stroke:#808080,stroke-dasharray: 5 5
```

## Container Injection Flow

### AuthBridge Webhook - With SPIRE Integration

When `kagenti.io/spire: enabled` label is set on the pod template:

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

### AuthBridge Webhook - Without SPIRE Integration

When `spiffe-enabled` label is **not** set (default):

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

### Legacy Webhooks - SPIRE Only (Deprecated)

Agent CR webhook always injects SPIRE sidecars:

```mermaid
graph LR
    subgraph "Pod Spec"
        APP[Application<br/>Container<br/>⚠️ DEPRECATED]
    end

    subgraph "Legacy Injection"
        SPIFFE[spiffe-helper<br/>Sidecar]
        CLIENT[client-registration<br/>Sidecar]
    end

    subgraph "External Dependencies"
        SPIRE[SPIRE Agent]
        KC[Keycloak]
    end

    SPIFFE -->|1. Get JWT-SVID| SPIRE
    SPIFFE -->|2. Write token| CLIENT
    CLIENT -->|3. Register| KC
    APP -->|No proxy| KC

    style SPIFFE fill:#D3D3D3,stroke:#808080
    style CLIENT fill:#D3D3D3,stroke:#808080
    style APP fill:#D3D3D3,stroke:#808080,stroke-dasharray: 5 5
```

## Injection Decision Flow

```mermaid
graph TD
    START[Webhook Receives<br/>Admission Request]
    CHECK_TYPE{Resource Type?}

    subgraph "AuthBridge Path (Recommended)"
        WORKLOAD[Standard Workload<br/>Deploy/STS/DS/Job]
        CHECK_NS_AB{Namespace has<br/>kagenti-enabled=true?}
        CHECK_LABEL{Pod has<br/>kagenti.io/inject=enabled}
        CHECK_SPIRE{Pod has<br/>kagenti.io/spire: enabled?}
        INJECT_FULL[Inject: proxy-init<br/>envoy-proxy, spiffe-helper<br/>client-registration]
        INJECT_BASIC[Inject: proxy-init<br/>envoy-proxy only]
    end

    subgraph "Legacy Path (Deprecated)"
        CUSTOM[Custom Resource<br/>Agent CR]
        CHECK_NS_LEG{Namespace has<br/>kagenti-enabled=true?}
        CHECK_ANNO{CR has<br/>kagenti.io/inject=enabled?}
        INJECT_SPIRE[Inject: spiffe-helper<br/>client-registration]
    end

    SKIP[Skip Injection]

    START --> CHECK_TYPE
    CHECK_TYPE -->|Workload| WORKLOAD
    CHECK_TYPE -->|CR| CUSTOM

    WORKLOAD --> CHECK_NS_AB
    CHECK_NS_AB -->|Yes| CHECK_LABEL
    CHECK_NS_AB -->|No| CHECK_LABEL
    CHECK_LABEL -->|true| CHECK_SPIRE
    CHECK_LABEL -->|false| SKIP
    CHECK_LABEL -->|not set & ns=true| CHECK_SPIRE
    CHECK_LABEL -->|not set & ns=false| SKIP

    CHECK_SPIRE -->|Yes| INJECT_FULL
    CHECK_SPIRE -->|No| INJECT_BASIC

    CUSTOM --> CHECK_NS_LEG
    CHECK_NS_LEG -->|Yes| CHECK_ANNO
    CHECK_NS_LEG -->|No| CHECK_ANNO
    CHECK_ANNO -->|true| INJECT_SPIRE
    CHECK_ANNO -->|false| SKIP
    CHECK_ANNO -->|not set & ns=true| INJECT_SPIRE
    CHECK_ANNO -->|not set & ns=false| SKIP

    style INJECT_FULL fill:#32CD32
    style INJECT_BASIC fill:#4169E1
    style INJECT_SPIRE fill:#D3D3D3,stroke:#808080,stroke-dasharray: 5 5
    style WORKLOAD fill:#87CEEB
    style CUSTOM fill:#D3D3D3,stroke:#808080,stroke-dasharray: 5 5
```

## Key Differences

| Aspect | AuthBridge (Recommended) | Legacy (Deprecated) |
|--------|--------------------------|---------------------|
| **Resources** | Standard K8s workloads | Custom Resources |
| **Injection Control** | Pod labels | CR annotations |
| **SPIRE** | Optional (`kagenti.io/spire: enabled` label) | Always enabled |
| **Containers** | Init: proxy-init<br/>Sidecars: envoy-proxy, spiffe-helper*, client-registration* | Sidecars: spiffe-helper, client-registration |
| **Traffic Management** | ✅ Envoy proxy with iptables | ❌ No proxy |
| **Authentication** | Multiple methods (SPIRE, mTLS, JWT, etc.) | SPIRE only |
| **Method** | `InjectAuthBridge()` | `MutatePodSpec()` |

\* Only injected when `kagenti.io/spire: enabled`
```
