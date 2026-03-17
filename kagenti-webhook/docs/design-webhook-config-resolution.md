# Design: Admission-Time Configuration Resolution for AuthBridge Sidecar Injection

**Status**: Implemented (PR #217) — under review

**Authors**: Kagenti Team

**Related**:
- [Consolidated Design Proposal (PR #770)](https://github.com/kagenti/kagenti/pull/770)
- [Externalize Config Epic (#109)](https://github.com/kagenti/kagenti-extensions/issues/109)
- [Pod-Level Webhook (PR #183)](https://github.com/kagenti/kagenti-extensions/pull/183)
- [AgentRuntime CRD (kagenti-operator PR #212)](https://github.com/kagenti/kagenti-operator/pull/212)

---

## Problem Statement

The kagenti-webhook injects AuthBridge sidecars into Pods at CREATE time. The injected containers need configuration from **three independent sources** that are currently disconnected:

| Source | What it provides | How it's consumed today |
|--------|-----------------|------------------------|
| **Platform defaults** (`kagenti-webhook-defaults` ConfigMap) | Container images, proxy ports, resource limits | Loaded at startup, hot-reloaded via file watcher. Used by `ContainerBuilder` to set image, ports, resources. |
| **Namespace ConfigMaps** (4 ConfigMaps + 1 Secret) | Keycloak URL/realm, token exchange params, envoy routing, SPIFFE helper config, admin credentials | Emitted as `ValueFrom` references in container env vars. Resolved by kubelet at Pod startup — **the webhook never reads these values**. |
| **AgentRuntime CR** (planned) | Per-workload identity and token exchange overrides | **Does not exist yet.** Will be defined in kagenti-operator. |

### Why this is a problem

1. **No merge path for AgentRuntime overrides.** Since the webhook emits `ValueFrom` references (not literal values), there is no point in the pipeline where AgentRuntime overrides can be merged with namespace ConfigMap values. The webhook would need to emit both a `ValueFrom` and a literal `Value` for the same env var — Kubernetes uses `Value` if both are set, but this is fragile and undocumented behavior.

2. **Envoy config is a static blob.** The `envoy-config` ConfigMap contains a ~150-line Envoy YAML with hardcoded port numbers (15123, 15124, 9901). If platform defaults change these ports, the envoy config doesn't adapt. If AgentRuntime wants to add per-host routing rules, there's no mechanism.

3. **Pod startup depends on namespace ConfigMaps existing.** If any referenced ConfigMap is missing, the Pod fails to start with `CreateContainerConfigError`. The webhook has no visibility into this — it succeeds, but the Pod is broken. Reading at admission time lets the webhook validate and provide meaningful error messages.

4. **No single view of resolved configuration.** Debugging injection issues requires checking three separate sources. A resolved config computed at admission time can be logged once, making troubleshooting straightforward.

## Goals

1. Unify all configuration sources into a single admission-time resolution pipeline
2. Enable AgentRuntime CR overrides for identity and token exchange settings
3. Template envoy.yaml from resolved config so port changes propagate automatically
4. Maintain backward compatibility with existing namespace ConfigMap conventions
5. Graceful degradation when ConfigMaps or AgentRuntime CR are missing

## Non-Goals

1. AgentRuntime overrides for container images, ports, resources, or token exchange fields (not in `agent.kagenti.dev/v1alpha1` CRD)
2. Dynamic reconfiguration of running sidecars (that's the operator's concern, not the webhook's)
3. Replacing the `kagenti-webhook-defaults` ConfigMap or feature gates mechanisms (those stay as-is)
4. Making ConfigMap names configurable (they remain well-known constants)

---

## Design

### Configuration Sources and Merge Precedence

The webhook resolves configuration at admission time by reading all sources and merging them with a clear precedence order. **Highest priority wins** — a non-empty value at a higher layer replaces the value from a lower layer.

```
Layer 1 (lowest)    CompiledDefaults()
                         ↓ overlaid by
Layer 2             kagenti-webhook-defaults ConfigMap (PlatformConfig)
                         ↓ provides images/ports/resources
Layer 3             Namespace ConfigMaps + Secrets
                         ↓ overlaid by
Layer 4 (highest)   AgentRuntime CR .spec.identity + .spec.trace
```

#### What each layer provides

| Field | L1: Compiled | L2: Platform CM | L3: Namespace CMs | L4: AgentRuntime |
|-------|:---:|:---:|:---:|:---:|
| Container images | x | x | | |
| Proxy ports | x | x | | |
| Resource limits | x | x | | |
| Sidecar enable/disable | x | x | | |
| Keycloak URL | | | x | |
| Keycloak realm | | | x | x (via clientRegistration.realm) |
| Keycloak admin creds | | | x | x (via clientRegistration.adminCredentialsSecret) |
| Token URL | | x | x | |
| Issuer | | | x | |
| Expected audience | | | x | |
| Target audience | | x | x | |
| Target scopes | | x | x | |
| Default outbound policy | | | x | |
| SPIFFE trust domain | x | x | | x (via spiffe.trustDomain) |
| SPIFFE helper config | | | x | |
| Envoy YAML | | | x (optional) | |
| Authproxy routes | | | x (optional) | |
| Platform client IDs | | | x | |
| Trace endpoint | | | | x (via trace.endpoint) |
| Trace protocol | | | | x (via trace.protocol) |
| Trace sampling rate | | | | x (via trace.sampling.rate) |

### Namespace ConfigMaps Contract

The webhook reads these resources from the **target namespace** (the namespace where the Pod is being created). All names are well-known constants.

| Resource | Kind | Keys read by webhook |
|----------|------|---------------------|
| `authbridge-config` | ConfigMap | `KEYCLOAK_URL`, `KEYCLOAK_REALM`, `SPIRE_ENABLED`, `PLATFORM_CLIENT_IDS`, `TOKEN_URL`, `ISSUER`, `EXPECTED_AUDIENCE`, `TARGET_AUDIENCE`, `TARGET_SCOPES`, `DEFAULT_OUTBOUND_POLICY` |
| `keycloak-admin-secret` | Secret | `KEYCLOAK_ADMIN_USERNAME`, `KEYCLOAK_ADMIN_PASSWORD` |
| `spiffe-helper-config` | ConfigMap | `helper.conf` (full file content) |
| `envoy-config` | ConfigMap | `envoy.yaml` (full file content, optional — if absent, webhook templates it) |
| `authproxy-routes` | ConfigMap | `routes.yaml` (full file content, optional — per-host routing rules for go-processor) |

**Missing ConfigMaps are not fatal.** If a ConfigMap doesn't exist, the webhook uses empty values for those fields and falls through to lower-priority layers. This enables gradual rollout — namespaces can start with just platform defaults.

### AgentRuntime CR Integration

The AgentRuntime CRD (`agent.kagenti.dev/v1alpha1`) is defined in the kagenti-operator repository ([PR #212](https://github.com/kagenti/kagenti-operator/pull/212)). The webhook reads it using an **unstructured client** to avoid a Go type dependency on the operator module.

**Lookup**: At admission time, the webhook **lists** all AgentRuntime CRs in the Pod's namespace and finds one whose `spec.targetRef.name` matches the workload name (derived from `GenerateName`). This supports the CRD's duck-typing binding model where the CR name can differ from the workload name.

**Override scope (v1alpha1)**: Identity (SPIFFE trust domain, client registration realm, admin credentials secret) and observability (trace endpoint, protocol, sampling rate) are overridable. Token exchange fields (tokenUrl, targetAudience, targetScopes) are **not** part of the AgentRuntime CRD and remain controlled by namespace ConfigMaps.

```yaml
apiVersion: agent.kagenti.dev/v1alpha1
kind: AgentRuntime
metadata:
  name: weather-agent-runtime
  namespace: team1
spec:
  type: agent
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: weather-agent

  identity:
    spiffe:
      trustDomain: "prod.cluster.local"         # overrides platform default

    clientRegistration:
      provider: "keycloak"
      realm: "production"                        # overrides namespace CM KEYCLOAK_REALM
      adminCredentialsSecret:
        name: "prod-keycloak-admin"              # overrides namespace keycloak-admin-secret
        namespace: "team1"

  trace:
    endpoint: "http://otel-collector:4317"       # OTEL collector endpoint
    protocol: grpc                               # grpc or http
    sampling:
      rate: 0.5                                  # 0.0–1.0
```

**Fallback behavior**: If no AgentRuntime CR exists (CRD not installed, or no CR targeting this workload), the webhook falls back to namespace ConfigMaps + platform defaults. This preserves backward compatibility with the current label-based model where no CR is required.

### Envoy Configuration Templating

Today the `envoy-config` ConfigMap is a static YAML blob. This design introduces an **embedded Go template** that generates envoy.yaml from resolved config at admission time.

**Decision logic**:
1. If the namespace has an `envoy-config` ConfigMap with `envoy.yaml` key → use it verbatim (backward compat)
2. If no `envoy-config` ConfigMap exists → render from the embedded Go template using resolved ports and settings

**Template parameters** (from `ResolvedConfig`):

| Parameter | Source | Default |
|-----------|--------|---------|
| `OutboundPort` | `PlatformConfig.Proxy.Port` | 15123 |
| `InboundPort` | `PlatformConfig.Proxy.InboundProxyPort` | 15124 |
| `AdminPort` | `PlatformConfig.Proxy.AdminPort` | 9901 |
| `ExtProcPort` | hardcoded | 9090 |

The template produces the same structure as the current `envoy-config` ConfigMap: outbound listener (TLS passthrough + HTTP with ext_proc), inbound listener (Lua header + ext_proc), original_destination cluster, ext_proc_cluster.

**Delivery**: The rendered envoy.yaml is available via `RenderEnvoyConfig()` for creating or validating ConfigMaps. The Pod volume continues to reference the namespace `envoy-config` ConfigMap. The template rendering capability is useful for programmatic ConfigMap creation and for future per-workload envoy configs.

### Container Builder Changes

The `ContainerBuilder` currently emits `ValueFrom` references for env vars. After this change, it emits **literal `Value` fields** from the resolved config.

**Before** (envoy-proxy container, simplified):
```go
Env: []corev1.EnvVar{
    {
        Name: "TOKEN_URL",
        ValueFrom: &corev1.EnvVarSource{
            ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
                LocalObjectReference: corev1.LocalObjectReference{Name: "authbridge-config"},
                Key: "TOKEN_URL",
            },
        },
    },
}
```

**After**:
```go
Env: []corev1.EnvVar{
    {
        Name:  "TOKEN_URL",
        Value: resolved.TokenURL,  // literal value from merged config
    },
}
```

This change applies to:
- `BuildEnvoyProxyContainer*()` — TOKEN_URL, ISSUER, EXPECTED_AUDIENCE, TARGET_AUDIENCE, TARGET_SCOPES, CLIENT_ID_FILE, CLIENT_SECRET_FILE
- `BuildClientRegistrationContainer*()` — SPIRE_ENABLED, KEYCLOAK_URL, KEYCLOAK_REALM, KEYCLOAK_ADMIN_USERNAME, KEYCLOAK_ADMIN_PASSWORD, CLIENT_NAME, SECRET_FILE_PATH, PLATFORM_CLIENT_IDS
- `BuildSpiffeHelperContainer()` — no env var changes (config via volume mount)

### Volume Builder Changes

Volumes continue to reference namespace ConfigMaps (Kubernetes does not support truly inline file data in pod specs without an external resource). The volume builder adds a `BuildResolvedVolumes()` function that supports specifying a custom envoy ConfigMap name for future per-workload envoy configs.

The envoy template rendering is available for creating or validating ConfigMaps programmatically, but the volume mount mechanism is unchanged — existing namespace ConfigMaps (`envoy-config`, `spiffe-helper-config`, `authproxy-routes`) continue to work as before.

---

## Data Flow

```
Pod CREATE admission request
         │
         ▼
┌─────────────────────────────────────────────────────────┐
│ AuthBridgeWebhook.Handle()                               │
│                                                          │
│  1. Decode Pod, derive workload name, check idempotency │
│                                                          │
│  2. PodMutator.InjectAuthBridge()                        │
│     ├── Pre-filters (type label, global gate, inject     │
│     │   label, tool gate)                                │
│     ├── Precedence evaluation (per-sidecar decisions)    │
│     │                                                    │
│     ├── NEW: Config Resolution (perWorkloadConfigResolution) │
│     │   ├── ReadNamespaceConfig()                        │
│     │   │   ├── GET configmap/authbridge-config          │
│     │   │   ├── GET configmap/spiffe-helper-config       │
│     │   │   ├── GET configmap/envoy-config (optional)    │
│     │   │   └── GET configmap/authproxy-routes (optional)│
│     │   │                                                │
│     │   ├── ReadAgentRuntimeOverrides(ctx, client, ns)   │
│     │   │   └── LIST agentruntimes.agent.kagenti.dev     │
│     │   │       (filter by spec.targetRef.name)          │
│     │   │                                                │
│     │   └── ResolveConfig(platform, namespace, runtime)  │
│     │       → ResolvedConfig (all fields merged)         │
│     │                                                    │
│     ├── ContainerBuilder(resolved)                       │
│     │   ├── BuildEnvoyProxy (literal env vars)           │
│     │   ├── BuildProxyInit (unchanged)                   │
│     │   ├── BuildSpiffeHelper (unchanged)                │
│     │   └── BuildClientRegistration (literal env vars)   │
│     │                                                    │
│     └── VolumeBuilder(resolved)                          │
│         ├── envoy-config (ConfigMap ref)                 │
│         ├── spiffe-helper-config (ConfigMap ref)         │
│         └── shared-data, spire-agent-socket, svid-output │
│                                                          │
│     Note: RenderEnvoyConfig() exists for programmatic     │
│     ConfigMap creation but is not yet called in the      │
│     admission pipeline — volumes reference namespace     │
│     ConfigMaps by name. See envoy_template.go TODO.      │
│                                                          │
│  3. Marshal mutated Pod, return JSON patch               │
└─────────────────────────────────────────────────────────┘
```

---

## RBAC

The webhook's ClusterRole already has permissions for ConfigMaps, Secrets, and ServiceAccounts. No changes needed for the current implementation.

When the AgentRuntime CRD lands in kagenti-operator, the following RBAC will be added:

```yaml
# Deferred: AgentRuntime CR read access (added when CRD is merged)
- apiGroups: ["agent.kagenti.dev"]
  resources: ["agentruntimes"]
  verbs: ["get", "list", "watch"]
```

---

## New Types

### NamespaceConfig

Holds values extracted from namespace ConfigMaps/Secrets at admission time.

```go
type NamespaceConfig struct {
    // From "authbridge-config" ConfigMap
    KeycloakURL           string
    KeycloakRealm         string
    SpireEnabled          string
    PlatformClientIDs     string
    TokenURL              string
    Issuer                string
    ExpectedAudience      string
    TargetAudience        string
    TargetScopes          string
    DefaultOutboundPolicy string

    // Note: keycloak-admin-secret is NOT read into this struct.
    // Credentials stay as SecretKeyRef in the container spec, keeping
    // them out of the webhook's memory and Pod environment literals.

    // From "spiffe-helper-config" ConfigMap
    SpiffeHelperConf string   // raw helper.conf content

    // From "envoy-config" ConfigMap
    EnvoyYAML string          // raw envoy.yaml (empty if CM not found)
}
```

### AgentRuntimeOverrides

Holds the subset of AgentRuntime CR fields (`agent.kagenti.dev/v1alpha1`) that the webhook can override. Uses pointer types so `nil` means "no override" (distinct from empty string). Aligned with [kagenti-operator PR #212](https://github.com/kagenti/kagenti-operator/pull/212).

```go
type AgentRuntimeOverrides struct {
    // Identity — from .spec.identity.spiffe
    SpiffeTrustDomain *string

    // Identity — from .spec.identity.clientRegistration
    ClientRegistrationProvider      *string
    ClientRegistrationRealm         *string
    AdminCredentialsSecretName      *string
    AdminCredentialsSecretNamespace *string

    // Observability — from .spec.trace
    TraceEndpoint     *string
    TraceProtocol     *string  // "grpc" or "http"
    TraceSamplingRate *float64 // 0.0–1.0
}
```

### ResolvedConfig

The fully-merged configuration for a single workload injection. This is the input to `ContainerBuilder` and `RenderEnvoyConfig`.

```go
type ResolvedConfig struct {
    // Platform config (images, ports, resources) — from PlatformConfig layers 1+2
    Platform *config.PlatformConfig

    // Identity — merged from namespace CMs + AgentRuntime overrides
    KeycloakURL       string
    KeycloakRealm     string
    // Admin credentials referenced via SecretKeyRef — not stored here
    AdminCredentialsSecretName string // defaults to "keycloak-admin-secret"
    SpireEnabled      string
    SpiffeTrustDomain string
    PlatformClientIDs string

    // Token exchange — from namespace CMs (not overridable by AgentRuntime v1alpha1)
    TokenURL              string
    Issuer                string
    ExpectedAudience      string
    TargetAudience        string
    TargetScopes          string
    DefaultOutboundPolicy string

    // Sidecar configs — from namespace CMs (not overridable by AgentRuntime v1alpha1)
    SpiffeHelperConf    string
    EnvoyYAML           string   // non-empty = use verbatim; empty = render from template
    AuthproxyRoutesYAML string

    // Observability — from AgentRuntime .spec.trace (optional)
    TraceEndpoint     string
    TraceProtocol     string   // "grpc" or "http"
    TraceSamplingRate *float64 // nil = not set
}
```

---

## Files to Create and Modify

| File | Action | Description |
|------|--------|-------------|
| `internal/webhook/injector/agentruntime_types.go` | Create | Stub constants for AgentRuntime CRD (GVR, Kind) |
| `internal/webhook/injector/namespace_config.go` | Create | `ReadNamespaceConfig()` — reads well-known CMs/Secrets from target namespace |
| `internal/webhook/injector/namespace_config_test.go` | Create | Unit tests: CM present, CM missing, partial CMs, Secret missing |
| `internal/webhook/injector/agentruntime_config.go` | Create | `ReadAgentRuntimeOverrides()` — unstructured read of AgentRuntime CR |
| `internal/webhook/injector/agentruntime_config_test.go` | Create | Unit tests: CR found, CR not found, CRD not installed, partial overrides |
| `internal/webhook/injector/resolved_config.go` | Create | `ResolveConfig()` — merge logic with precedence rules |
| `internal/webhook/injector/resolved_config_test.go` | Create | Unit tests: all layers present, missing layers, override precedence |
| `internal/webhook/injector/envoy_template.go` | Create | `RenderEnvoyConfig()` — Go template rendering with `//go:embed` |
| `internal/webhook/injector/envoy_template_test.go` | Create | Unit tests: default ports, custom ports, verbatim passthrough |
| `internal/webhook/injector/envoy.yaml.tmpl` | Create | Embedded Go template for envoy.yaml |
| `internal/webhook/injector/container_builder.go` | Modify | Add `NewResolvedContainerBuilder()`, dual-mode env vars (literal or ValueFrom) |
| `internal/webhook/injector/volume_builder.go` | Modify | Add `BuildResolvedVolumes()` for future per-workload envoy configs |
| `internal/webhook/injector/pod_mutator.go` | Modify | Add config resolution pipeline with feature gate check (false=legacy ValueFrom, true=resolved reads) |
| `internal/webhook/config/feature_gates.go` | Modify | Add `PerWorkloadConfigResolution` field |
| `internal/webhook/config/feature_gate_loader.go` | Modify | Log new gate in banner |
| `charts/kagenti-webhook/values.yaml` | Modify | Add `perWorkloadConfigResolution: false` to `featureGates` |
| `charts/kagenti-webhook/templates/clusterrole.yaml` | Deferred | AgentRuntime RBAC deferred until CRD lands in kagenti-operator |

---

## Backward Compatibility

| Scenario | Behavior |
|----------|----------|
| No AgentRuntime CRD installed | Unstructured list returns empty → namespace CMs + platform defaults used |
| No AgentRuntime CR for workload | Same as above |
| Namespace CMs exist (current setup) | Read at admission time, values injected as literals. Identical end result. |
| Namespace CMs partially missing | Missing values fall through to platform defaults where applicable; others work normally |
| All namespace CMs missing | Platform defaults only. Pod may fail if identity config is incomplete (same as today). |
| `envoy-config` CM exists in namespace | Used verbatim (no templating). Existing deployments unchanged. |
| `envoy-config` CM absent | Webhook renders envoy.yaml from template using platform defaults |

---

## Performance Considerations

The config resolution adds **up to 5 API server reads** per Pod admission (4 ConfigMaps + 1 AgentRuntime list). Webhook admission must complete within 10 seconds.

### Feature Gate Behavior

The `perWorkloadConfigResolution` feature gate controls which injection path the webhook uses:

| Gate value | Behavior | Use case |
|------------|----------|----------|
| `false` (default) | **Legacy path** — env vars use `ValueFrom` ConfigMapKeyRef/SecretKeyRef references. Kubernetes kubelet resolves values at container start. | Production — no additional API calls at admission time. |
| `true` | **Resolved path** — webhook reads namespace ConfigMaps at admission time and injects literal env var values. Enables AgentRuntime CR overrides. | When per-workload config resolution or AgentRuntime overrides are needed. |

When the resolved path is enabled, ConfigMaps are read on every admission request. This is acceptable because:
- The webhook already makes API calls (ServiceAccount creation) so the client infrastructure exists
- ConfigMap reads are small (< 1KB each)
- AgentRuntime list is namespace-scoped and filtered

**Note**: A `NamespaceConfigCache` was originally designed but removed during implementation review. Per-namespace caching may be added in a future iteration if API call volume becomes a concern at scale.

---

## Security Considerations

1. **Credentials stay as SecretKeyRef**: The webhook does NOT read `keycloak-admin-secret` into memory. Instead, the resolved container builder uses `SecretKeyRef` references (same as the legacy path), keeping credentials out of the webhook's memory and out of Pod spec literal env vars. Non-sensitive values (URLs, realm names, etc.) are injected as literals.

2. **AgentRuntime RBAC**: The webhook gains read access to `agentruntimes.agent.kagenti.dev`. This is a low-risk addition — it's read-only and scoped to the Kagenti API group.

3. **Inline config in Pod spec**: The rendered envoy.yaml and spiffe-helper.conf are visible in the Pod spec (`kubectl get pod -o yaml`). This is equivalent to today's ConfigMap volume mounts. No new information exposure.

---

## Testing Strategy

1. **Unit tests** (per new file):
   - `namespace_config_test.go`: ConfigMap present/absent/partial
   - `agentruntime_config_test.go`: targetRef match, no match, partial overrides, CRD not installed
   - `resolved_config_test.go`: All-layers merge, missing layers, realm override, trace overrides, token exchange not overridable
   - `envoy_template_test.go`: Default ports, custom ports, verbatim passthrough

2. **Integration tests** (envtest):
   - Update `authbridge_webhook_test.go`: Verify injected env vars are literal values
   - Test with/without namespace ConfigMaps
   - Test envoy.yaml inlining in Pod spec

3. **End-to-end** (Kind cluster):
   - Deploy with `scripts/webhook-rollout.sh`
   - Create Pod in namespace with existing ConfigMaps → verify literal env vars
   - Create Pod in namespace without ConfigMaps → verify graceful degradation
   - Inspect Pod spec → verify inline envoy.yaml

---

## Implementation Phases

This work can be delivered incrementally. Each phase is independently useful and testable.

### Phase 1: Namespace Config Reader + Literal Env Vars
- Create `namespace_config.go` and `resolved_config.go`
- Refactor `container_builder.go` to accept `ResolvedConfig` and emit literal values
- Update `pod_mutator.go` to call the resolution pipeline
- **Result**: Webhook reads CMs at admission time. No AgentRuntime support yet. Functionally equivalent to today.

### Phase 2: Envoy Templating
- Create `envoy_template.go` and `envoy.yaml.tmpl`
- Update `volume_builder.go` to inline rendered envoy.yaml
- **Result**: Envoy config adapts to port changes. Namespace `envoy-config` CM becomes optional.

### Phase 3: AgentRuntime CR Integration
- Create `agentruntime_types.go` (stub constants for `agent.kagenti.dev/v1alpha1`)
- Create `agentruntime_config.go` (List + targetRef.name matching, aligned with [kagenti-operator PR #212](https://github.com/kagenti/kagenti-operator/pull/212))
- Update `ResolveConfig()` to merge AgentRuntime overrides (realm, SPIFFE trust domain, trace)
- RBAC deferred until CRD is merged in kagenti-operator
- **Result**: Full config resolution pipeline with all four layers.
