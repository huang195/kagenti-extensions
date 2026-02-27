# CLAUDE.md - Kagenti Webhook

This file provides context for Claude (AI assistant) when working with the `kagenti-webhook` codebase.
For the full monorepo context (AuthProxy, client-registration, CI/CD, Helm, cross-component relationships), see [`../CLAUDE.md`](../CLAUDE.md).

## Project Overview

**kagenti-webhook** is a Kubernetes mutating admission webhook that automatically injects sidecar containers into workload pods to enable secure service-to-service authentication via Keycloak and optional SPIFFE/SPIRE identity. It is built with the [Kubebuilder](https://book.kubebuilder.io/) framework and uses [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime).

The project lives inside the larger `kagenti-extensions` monorepo. The Helm chart is at `../charts/kagenti-webhook/`. The CI workflow is at `../.github/workflows/build.yaml`.

## Architecture Summary

There are **two** registered webhooks, but only one is actively recommended:

| Webhook | Status | Path | Handles |
|---------|--------|------|---------|
| **AuthBridge** | **Active / Recommended** | `/mutate-workloads-authbridge` | Deployments, StatefulSets, DaemonSets, Jobs, CronJobs |
| Agent | Deprecated | `/mutate-agent-kagenti-dev-v1alpha1-agent` | `Agent` CR (`agent.kagenti.dev/v1alpha1`) |

Both webhooks share a single `PodMutator` instance created in `cmd/main.go`.

### Injection Decision Flow

**AuthBridge** (active path):
1. `kagenti.io/type` label must be `agent` or `tool` -- otherwise skip.
2. `kagenti.io/inject: enabled` label forces injection ON.
3. `kagenti.io/inject: disabled` (or any non-`enabled` value) forces injection OFF.
4. If no pod label, fall back to namespace label `kagenti-enabled: "true"`.

**Legacy Agent** (deprecated path):
1. CR annotation `kagenti.dev/inject: "false"` opts out.
2. CR annotation `kagenti.dev/inject: "true"` opts in.
3. Fall back to namespace label/annotation.

### Injected Containers

**AuthBridge always injects:**
- `proxy-init` (init container) -- iptables redirect setup.
- `envoy-proxy` (sidecar) -- Envoy service mesh proxy for traffic management.

**Conditionally injected:**
- `spiffe-helper` (sidecar) -- gated by `kagenti.io/spire: enabled` pod label. Obtains JWT-SVIDs from SPIRE agent.
- `kagenti-client-registration` (sidecar) -- gated by `--enable-client-registration` flag (default `true`). Registers with Keycloak; uses SPIFFE identity when SPIRE is enabled, otherwise uses static `CLIENT_NAME`.

**Legacy Agent webhook (deprecated) always injects:** `spiffe-helper` + `kagenti-client-registration` + `envoy-proxy` (no `proxy-init` — legacy path does not call `InjectInitContainers`).

## Directory Structure

```
kagenti-webhook/
├── cmd/main.go                              # Entrypoint: flags, manager setup, webhook registration
├── internal/webhook/
│   ├── config/                              # Platform configuration (not yet wired into injector)
│   │   ├── types.go                         #   PlatformConfig struct (images, proxy, resources, etc.)
│   │   ├── defaults.go                      #   CompiledDefaults() hardcoded fallback config
│   │   ├── feature_gates.go                 #   FeatureGates struct (global sidecar enable/disable)
│   │   ├── feature_gate_loader.go           #   File watcher + loader for feature gates
│   │   └── loader.go                        #   File watcher + loader for PlatformConfig
│   ├── injector/                            # Shared mutation logic (the core engine)
│   │   ├── pod_mutator.go                   #   PodMutator: ShouldMutate, NeedsMutation, InjectAuthBridge, etc.
│   │   ├── container_builder.go             #   Build* functions for each injected container
│   │   ├── volume_builder.go                #   BuildRequiredVolumes / BuildRequiredVolumesNoSpire
│   │   └── namespace_checker.go             #   CheckNamespaceInjectionEnabled / IsNamespaceInjectionEnabled
│   └── v1alpha1/                            # Webhook handlers
│       ├── authbridge_webhook.go            #   AuthBridge (recommended): raw admission.Handler
│       ├── agent_webhook.go                 #   Agent (deprecated): CustomDefaulter + CustomValidator
│       └── webhook_suite_test.go            #   ENVTEST-based test setup (Ginkgo)
├── config/                                  # Kustomize manifests (CRDs, RBAC, webhook configs, etc.)
├── test/
│   ├── e2e/                                 # End-to-end tests (Kind cluster, Ginkgo)
│   └── utils/                               # Test helpers (Run, LoadImageToKind, CertManager, etc.)
├── scripts/
│   └── webhook-rollout.sh                   # Build + deploy to Kind cluster script
├── Makefile                                 # Build, test, deploy targets
├── Dockerfile                               # Multi-stage Go build -> distroless
├── go.mod / go.sum                          # Go 1.24, controller-runtime v0.22
└── PROJECT                                  # Kubebuilder project metadata
```

## Key Packages and Dependencies

| Package | Version | Purpose |
|---------|---------|---------|
| `sigs.k8s.io/controller-runtime` | v0.22.1 | Manager, webhook server, envtest |
| `k8s.io/api` | v0.34.1 | Kubernetes API types |
| `github.com/kagenti/operator` | v0.2.0-alpha.12 | Agent CR types (via replace directive) |
| `github.com/onsi/ginkgo/v2` | v2.26.0 | BDD test framework |
| `github.com/onsi/gomega` | v1.38.2 | Test matchers |
| `github.com/fsnotify/fsnotify` | v1.9.0 | Config file watching |

**Go version:** 1.24.4 (toolchain 1.24.8), with `godebug default=go1.23`.

**Replace directive:** `github.com/kagenti/operator` is replaced with `github.com/kagenti/kagenti-operator/kagenti-operator`.

## Build and Test Commands

```bash
# Build binary
make build

# Run unit tests (requires envtest binaries)
make test

# Run e2e tests (requires Kind cluster)
make test-e2e

# Lint
make lint
make lint-fix

# Build Docker image
make docker-build IMG=<image>

# Local development with Kind
make local-dev CLUSTER=<kind-cluster-name>

# Quick rebuild + rollout (uses scripts/webhook-rollout.sh)
./scripts/webhook-rollout.sh

# Generate manifests (CRDs, RBAC, webhook configs)
make manifests

# Generate deepcopy methods
make generate
```

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `ENABLE_WEBHOOKS` | (unset = true) | Set to `"false"` to disable all webhook registration |
| `CLUSTER` | `kagenti` | Kind cluster name for local dev |
| `NAMESPACE` | `kagenti-webhook-system` | Deployment namespace |
| `AUTHBRIDGE_DEMO` | `false` | Enable AuthBridge demo setup in rollout script |
| `DOCKER_IMPL` | (auto-detect) | Force container runtime (`docker` or `podman`) |

### CLI Flags (cmd/main.go)

| Flag | Default | Description |
|------|---------|-------------|
| `--metrics-bind-address` | `0` (disabled) | Metrics endpoint bind address |
| `--health-probe-bind-address` | `:8081` | Health/ready probe address |
| `--leader-elect` | `false` | Enable leader election |
| `--metrics-secure` | `true` | Serve metrics over HTTPS |
| `--enable-client-registration` | `true` | Inject client-registration sidecar |
| `--webhook-cert-path` | `""` | TLS cert directory for webhook server |
| `--enable-http2` | `false` | Enable HTTP/2 (disabled by default for CVE mitigation) |

## Code Conventions and Patterns

### Naming Conventions
- **Constants** follow `CamelCase` (e.g., `SpiffeHelperContainerName`, `DefaultNamespaceLabel`).
- **Logger names** use lowercase-hyphenated format (e.g., `logf.Log.WithName("pod-mutator")`).
- **Webhook handler types** are `{Resource}Webhook`, `{Resource}CustomDefaulter`, `{Resource}CustomValidator`.
- **Builder functions** are `Build{Component}Container()` or `Build{Component}ContainerWithSpireOption()`.
- Container name constants must match what is checked in `isAlreadyInjected()` for idempotency.

### Architecture Patterns
- **Shared PodMutator**: All webhooks share one `injector.PodMutator` instance, created in `main()` and passed to each webhook setup function. This ensures consistent mutation logic.
- **Two mutation paths**: `MutatePodSpec()` (legacy, always enables SPIRE) vs `InjectAuthBridge()` (new, SPIRE is optional). When removing deprecated code, delete `MutatePodSpec`, `ShouldMutate`, and `CheckNamespaceInjectionEnabled`.
- **Idempotency**: `AuthBridgeWebhook.isAlreadyInjected()` checks for existing sidecars before injection.
- **Container existence checks**: `containerExists()` and `volumeExists()` helpers prevent duplicate injection.
- **Kubebuilder markers**: Webhook path markers (e.g., `+kubebuilder:webhook:path=...`) in Go comments generate the webhook manifests. Do not change these without running `make manifests`.

### ConfigMap Dependencies at Runtime
Injected sidecars expect these ConfigMaps to exist in the target namespace:
- `environments` -- `KEYCLOAK_URL`, `KEYCLOAK_REALM`, `KEYCLOAK_ADMIN_USERNAME`, `KEYCLOAK_ADMIN_PASSWORD`
- `authbridge-config` -- `TOKEN_URL`, `ISSUER`, `TARGET_AUDIENCE`, `TARGET_SCOPES`
- `spiffe-helper-config` -- SPIFFE helper configuration (when SPIRE is enabled)
- `envoy-config` -- Envoy proxy configuration

### Security Model
- `proxy-init` runs as an init container with a short lifetime (iptables setup).
- `envoy-proxy` runs as UID 1337.
- `client-registration` runs as UID/GID 1000.
- `spiffe-helper` uses no explicit security context.
- Istio exclusion annotations (`sidecar.istio.io/inject`, `ambient.istio.io/redirection`) are defined as constants but not yet actively applied.

### Test Infrastructure
- **Unit tests**: Use controller-runtime's `envtest` with Ginkgo/Gomega. Test setup is in `webhook_suite_test.go`. Run with `make test`.
- **E2E tests**: Require a Kind cluster with CertManager and Prometheus. Run with `make test-e2e`. Test setup installs CRDs, deploys the controller, and validates pod status + metrics.
- **Test binaries path**: ENVTEST binaries are expected in `bin/k8s/` (auto-discovered by `getFirstFoundEnvTestBinaryDir()`).

## Common Tasks for Code Changes

### Adding a New Injected Sidecar
1. Add container name constant in `injector/pod_mutator.go`.
2. Add `Build{Name}Container()` function in `injector/container_builder.go`.
3. Add any required volumes in `injector/volume_builder.go` (both `BuildRequiredVolumes` and `BuildRequiredVolumesNoSpire` if applicable).
4. Call the builder in `InjectSidecarsWithSpireOption()` in `pod_mutator.go`.
5. Update `isAlreadyInjected()` in `authbridge_webhook.go` to check for the new container name.
6. Update `internal/webhook/config/types.go` and `defaults.go` with image/resource defaults.

### Adding a New Supported Workload Type
1. Add a new `case` in `AuthBridgeWebhook.Handle()` in `authbridge_webhook.go`.
2. Update the kubebuilder webhook marker to include the new resource in the `resources` list.
3. Run `make manifests` to regenerate webhook configuration YAML.
4. Update `scripts/webhook-rollout.sh` to include the new resource in the webhook rules.
5. Update the Helm chart template `charts/kagenti-webhook/templates/authbridge-mutatingwebhook.yaml`.

### Modifying Injection Logic
- Injection decision logic lives in `pod_mutator.go` in `NeedsMutation()` (AuthBridge) and `ShouldMutate()` (legacy).
- Namespace checks are in `namespace_checker.go`.
- Changes to label/annotation keys require updating the constants at the top of `pod_mutator.go`.

### Removing Deprecated Code
When removing the legacy Agent webhook:
1. Remove `agent_webhook.go` and its tests.
2. Remove `ShouldMutate()`, `MutatePodSpec()`, `InjectSidecars()`, `InjectVolumes()` from `pod_mutator.go`.
3. Remove `CheckNamespaceInjectionEnabled()` from `namespace_checker.go`.
4. Remove legacy scheme registration and webhook setup calls from `cmd/main.go`.
5. Remove `github.com/kagenti/operator` from `go.mod`.
6. Remove legacy Helm templates (`agent-*.yaml`).
7. Run `make manifests` and `make generate`.

### Updating Container Images
- Default images are defined as constants in `injector/container_builder.go` (`DefaultEnvoyImage`, `DefaultProxyInitImage`) and inline in `BuildSpiffeHelperContainer()` and `BuildClientRegistrationContainerWithSpireOption()`.
- The `internal/webhook/config/defaults.go` file has a parallel set of defaults in `CompiledDefaults()` -- keep them in sync (or wire the config system into the injector, which is a TODO).
- The GitHub Actions CI builds images defined in `../.github/workflows/build.yaml`.

### Helm Chart
- Located at `../charts/kagenti-webhook/`.
- Key values: `image.repository`, `image.tag`, `webhook.enabled`, `webhook.enableClientRegistration`, `certManager.enabled`.
- AuthBridge webhook configuration template: `templates/authbridge-mutatingwebhook.yaml`.

## Gotchas and Known Issues

1. **Config system not wired in**: `internal/webhook/config/` (PlatformConfig, FeatureGates, loaders) exists but is **not yet used** by the injector. Container builder still uses hardcoded constants. This is a known gap.

2. **Replace directive in go.mod**: `github.com/kagenti/operator` is replaced -- be careful when updating dependencies or adding new imports from the kagenti-operator project.

3. **Kubebuilder markers**: The `+kubebuilder:webhook` comments generate webhook manifests. If you change the path, resources, or groups, you must run `make manifests` to regenerate.

4. **AuthBridge uses raw admission.Handler**: Unlike the legacy webhooks (which use `CustomDefaulter`/`CustomValidator`), the AuthBridge webhook registers directly via `mgr.GetWebhookServer().Register()`. This is because it handles multiple resource types in a single handler.

5. **Idempotency check**: `isAlreadyInjected()` checks for all four injected components (`envoy-proxy`, `spiffe-helper`, `kagenti-client-registration` in sidecar containers, `proxy-init` in init containers). If any one is found, re-admission is short-circuited.

6. **ENVTEST binary path**: Tests assume envtest binaries are in `bin/k8s/`. Run `make setup-envtest` to download them before running tests from an IDE.

7. **Helm chart image tag placeholder**: `values.yaml` uses `tag: "__PLACEHOLDER__"` -- this must be overridden at install time.

## License

Apache License 2.0. Copyright 2025. All Go files include the license header from `hack/boilerplate.go.txt`.
