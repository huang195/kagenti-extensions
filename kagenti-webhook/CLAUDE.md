# CLAUDE.md - Kagenti Webhook

This file provides context for Claude (AI assistant) when working with the `kagenti-webhook` codebase.
For the full monorepo context (AuthProxy, client-registration, CI/CD, Helm, cross-component relationships), see [`../CLAUDE.md`](../CLAUDE.md).

## Project Overview

**kagenti-webhook** is a Kubernetes mutating admission webhook that automatically injects sidecar containers into workload pods to enable secure service-to-service authentication via Keycloak and optional SPIFFE/SPIRE identity. It is built with the [Kubebuilder](https://book.kubebuilder.io/) framework and uses [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime).

The project lives inside the larger `kagenti-extensions` monorepo. The Helm chart is at `../charts/kagenti-webhook/`. The CI workflow is at `../.github/workflows/build.yaml`.

## Architecture Summary

There is one registered webhook:

| Webhook | Path | Handles |
|---------|------|---------|
| **AuthBridge** | `/mutate-workloads-authbridge` | Pods at CREATE time (works with any workload controller) |

The `PodMutator` instance is created in `cmd/main.go` and passed to the webhook setup function.

### Injection Decision Flow

**AuthBridge uses a two-stage decision process:**

**Stage 1 — PodMutator pre-filters (any "no" skips ALL injection):**

1. `kagenti.io/type` must be `agent` or `tool` — otherwise skip.
2. `featureGates.globalEnabled` must be `true` — kill switch (cluster-wide).
3. For tool workloads: `featureGates.injectTools` must be `true` — tools are not injected by default.
4. `kagenti.io/inject: disabled` on the workload — whole-workload opt-out.

**Stage 2 — PrecedenceEvaluator per-sidecar (independent for each sidecar):**

Each sidecar independently passes through a two-layer chain:

- L1: Per-sidecar feature gate (`featureGates.envoyProxy`, `.spiffeHelper`, `.clientRegistration`)
- L2: Per-sidecar workload label (`kagenti.io/<sidecar>-inject: "false"` on pod template)

`proxy-init` always mirrors the `envoy-proxy` decision and is never independently controlled.

### Injected Containers

#### Separate mode (default: `featureGates.combinedSidecar: false`)

**Injected when envoy-proxy decision passes:**

- `proxy-init` (init container) -- iptables redirect setup. Follows `envoy-proxy` decision exactly.
- `envoy-proxy` (sidecar) -- Envoy service mesh proxy for traffic management.

**Injected by default, per-sidecar opt-out available:**

- `spiffe-helper` (sidecar) -- obtains JWT-SVIDs from SPIRE agent. Opt out with `kagenti.io/spiffe-helper-inject: "false"` or `featureGates.spiffeHelper: false`.
- `kagenti-client-registration` (sidecar) -- registers with Keycloak via SPIFFE identity. Opt out with `kagenti.io/client-registration-inject: "false"` or `featureGates.clientRegistration: false`.

#### Combined mode (`featureGates.combinedSidecar: true`)

- `proxy-init` (init container) -- same as separate mode.
- `authbridge` (sidecar) -- single container combining Envoy + go-processor + spiffe-helper + client-registration. Runs as UID 1337. Per-sidecar feature gates and workload labels are passed as `SPIRE_ENABLED` and `CLIENT_REGISTRATION_ENABLED` env vars to the entrypoint. If envoy-proxy is disabled, no combined container is injected.

## Directory Structure

```
kagenti-webhook/
├── cmd/main.go                              # Entrypoint: flags, manager setup, webhook registration
├── internal/webhook/
│   ├── config/                              # Platform configuration (wired into PodMutator)
│   │   ├── types.go                         #   PlatformConfig struct (images, proxy, resources, etc.)
│   │   ├── defaults.go                      #   CompiledDefaults() hardcoded fallback config
│   │   ├── feature_gates.go                 #   FeatureGates struct (global sidecar enable/disable)
│   │   ├── feature_gate_loader.go           #   File watcher + loader for feature gates
│   │   └── loader.go                        #   File watcher + loader for PlatformConfig
│   ├── injector/                            # Shared mutation logic (the core engine)
│   │   ├── pod_mutator.go                   #   PodMutator: InjectAuthBridge, ensureServiceAccount
│   │   ├── container_builder.go             #   Build* functions for each injected container
│   │   └── volume_builder.go                #   BuildRequiredVolumes / BuildRequiredVolumesNoSpire
│   └── v1alpha1/                            # Webhook handlers
│       ├── authbridge_webhook.go            #   AuthBridge: raw admission.Handler (Pod-level)
│       ├── authbridge_webhook_test.go       #   Webhook handler tests (Ginkgo)
│       └── webhook_suite_test.go            #   ENVTEST-based test setup (Ginkgo)
├── config/                                  # Kustomize manifests (CRDs, RBAC, webhook configs, etc.)
├── test/
│   ├── e2e/                                 # End-to-end tests (Kind cluster, Ginkgo)
│   └── utils/                               # Test helpers (Run, LoadImageToKind, CertManager, etc.)
├── scripts/
│   ├── webhook-rollout.sh                   # Build + deploy to Kind cluster script
│   └── test-precedence.sh                   # Automated end-to-end test runner for the precedence system
├── Makefile                                 # Build, test, deploy targets
├── Dockerfile                               # Multi-stage Go build -> distroless
├── go.mod / go.sum                          # Go 1.26, controller-runtime v0.23
└── PROJECT                                  # Kubebuilder project metadata
```

## Key Packages and Dependencies

| Package | Version | Purpose |
|---------|---------|---------|
| `sigs.k8s.io/controller-runtime` | v0.23.3 | Manager, webhook server, envtest |
| `k8s.io/api` | v0.35.2 | Kubernetes API types |
| `github.com/onsi/ginkgo/v2` | v2.28.1 | BDD test framework |
| `github.com/onsi/gomega` | v1.39.1 | Test matchers |
| `github.com/fsnotify/fsnotify` | v1.9.0 | Config file watching |

**Go version:** 1.26.1, with `godebug default=go1.23`.

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
- **Shared PodMutator**: The `injector.PodMutator` instance is created in `main()` and passed to the webhook setup function. This ensures consistent mutation logic.
- **Single mutation path**: `InjectAuthBridge()` handles all injection decisions. SPIRE integration is optional and controlled by per-sidecar workload labels and feature gates.
- **Idempotency**: `AuthBridgeWebhook.isAlreadyInjected()` checks for existing sidecars before injection.
- **Container existence checks**: `containerExists()` and `volumeExists()` helpers prevent duplicate injection.
- **Kubebuilder markers**: Webhook path markers (e.g., `+kubebuilder:webhook:path=...`) in Go comments generate the webhook manifests. Do not change these without running `make manifests`.

### Runtime Dependencies
Injected sidecars expect these resources to exist in the target namespace:

ConfigMaps:
- `authbridge-config` -- `KEYCLOAK_URL`, `KEYCLOAK_REALM`, `PLATFORM_CLIENT_IDS` (optional), `TOKEN_URL` (optional, derived), `ISSUER` (optional, derived or explicit), `EXPECTED_AUDIENCE` (optional), `DEFAULT_OUTBOUND_POLICY` (optional). Target audience and scopes are configured per-route in the `authproxy-routes` ConfigMap.
- `spiffe-helper-config` -- SPIFFE helper configuration (when SPIRE is enabled)
- `envoy-config` -- Envoy proxy configuration

Secrets:
- `keycloak-admin-secret` -- `KEYCLOAK_ADMIN_USERNAME`, `KEYCLOAK_ADMIN_PASSWORD`

### Security Model
- `proxy-init` runs as an init container with a short lifetime (iptables setup).
- `envoy-proxy` runs as UID 1337.
- `client-registration` runs as UID/GID 1000.
- `spiffe-helper` uses no explicit security context.
- `authbridge` (combined mode) runs as UID 1337 (Envoy UID, excluded from iptables redirect).
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
4. Call the builder in `InjectAuthBridge()` in `pod_mutator.go`.
5. Update `isAlreadyInjected()` in `authbridge_webhook.go` to check for the new container name.
6. Update `internal/webhook/config/types.go` and `defaults.go` with image/resource defaults.

### Webhook Targeting Model
The webhook targets **Pods at CREATE time** (not Deployments/StatefulSets/etc.). This follows the same pattern used by Istio, Linkerd, and Vault Agent Injector. The handler decodes `corev1.Pod` directly — no switch on workload Kind. The `deriveWorkloadName()` helper extracts the workload name from `GenerateName` (trims trailing `-`). The `reinvocationPolicy` is set to `IfNeeded` so our webhook re-runs if other webhooks modify the Pod after our first pass.

### Modifying Injection Logic
- Injection decision logic lives in `pod_mutator.go` in `InjectAuthBridge()`.
- Changes to label/annotation keys require updating the constants at the top of `pod_mutator.go`.

### Updating Container Images
- Default images are defined in `internal/webhook/config/defaults.go` via `CompiledDefaults()`. The `ContainerBuilder` reads from `*config.PlatformConfig` at build time — never hardcode images/ports/resources in the builder.
- The config system is fully wired in: `PodMutator` uses getter functions `func() *config.PlatformConfig` and creates a new `ContainerBuilder` per request with the current config snapshot, enabling hot-reload.
- The GitHub Actions CI builds images defined in `../.github/workflows/build.yaml`.

### Helm Chart
- Located at `../charts/kagenti-webhook/`.
- Key values: `image.repository`, `image.tag`, `webhook.enabled`, `webhook.enableClientRegistration`, `certManager.enabled`.
- AuthBridge webhook configuration template: `templates/authbridge-mutatingwebhook.yaml`.

## Gotchas and Known Issues

1. **Config system is wired in**: `internal/webhook/config/` (PlatformConfig, FeatureGates, loaders) is used by `PodMutator` and `ContainerBuilder`. Feature gates support hot-reload via `FeatureGateLoader`. Platform config (images, ports, resources) is loaded at startup from the `kagenti-webhook-defaults` ConfigMap.

2. **Kubebuilder markers**: The `+kubebuilder:webhook` comments generate webhook manifests. If you change the path, resources, or groups, you must run `make manifests` to regenerate.

3. **AuthBridge uses raw admission.Handler**: Unlike webhooks that use `CustomDefaulter`/`CustomValidator`, the AuthBridge webhook registers directly via `mgr.GetWebhookServer().Register()`. It decodes `corev1.Pod` directly and includes a Kind guard for defense-in-depth against stale webhook configs.

4. **Idempotency check**: `isAlreadyInjected()` checks for all injected components (`envoy-proxy`, `spiffe-helper`, `kagenti-client-registration`, `authbridge` in sidecar containers, `proxy-init` in init containers). If any one is found, re-admission is short-circuited.

5. **ENVTEST binary path**: Tests assume envtest binaries are in `bin/k8s/`. Run `make setup-envtest` to download them before running tests from an IDE.

6. **Helm chart image tag placeholder**: `values.yaml` uses `tag: "__PLACEHOLDER__"` -- this must be overridden at install time.

## DCO Sign-Off (Mandatory)

All commits **must** include a `Signed-off-by` trailer (Developer Certificate of Origin).
Always use the `-s` flag when committing:

```sh
git commit -s -m "fix: Update container builder"
```

PRs without DCO sign-off will fail CI checks.

## Commit Attribution Policy

Do NOT use `Co-Authored-By` trailers for AI attribution. Use `Assisted-By` instead:

    Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>

Never add `Co-authored-by`, `Made-with`, or similar trailers that GitHub parses as co-authorship.
See the [root CLAUDE.md](../CLAUDE.md) for full commit policy details.

## License

Apache License 2.0. Copyright 2025. All Go files include the license header from `hack/boilerplate.go.txt`.
