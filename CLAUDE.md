# CLAUDE.md - Kagenti Extensions

This file provides context for Claude (AI assistant) when working with the `kagenti-extensions` monorepo.

## AI Assistant Instructions

- **No attribution** in commits, PR bodies, or issues — do not add "Co-Authored-By: Claude", "Generated with Claude Code", or any AI attribution.

## Repository Overview

**kagenti-extensions** is a monorepo containing Kubernetes security extensions for the [Kagenti](https://github.com/kagenti/kagenti) ecosystem. It provides **zero-trust authentication** for Kubernetes workloads through automatic sidecar injection, transparent token exchange, and dynamic Keycloak client registration using SPIFFE/SPIRE identities.

**GitHub:** `github.com/kagenti/kagenti-extensions`
**Container registry:** `ghcr.io/kagenti/kagenti-extensions/<image-name>`
**License:** Apache 2.0

## Top-Level Directory Structure

```
kagenti-extensions/
├── kagenti-webhook/          # Kubernetes admission webhook (Go, Kubebuilder)
├── AuthBridge/               # Authentication bridge components
│   ├── AuthProxy/            #   Envoy + ext-proc sidecar (Go) — token validation & exchange
│   │   ├── go-processor/     #     gRPC ext-proc server (inbound JWT validation, outbound token exchange)
│   │   └── quickstart/       #     Standalone demo (no SPIFFE)
│   ├── client-registration/  #   Keycloak auto-registration (Python)
│   ├── demos/                #   Demo scenarios (weather-agent, github-issue, webhook, single-target, multi-target)
│   └── keycloak_sync.py      #   Declarative Keycloak sync tool
├── charts/
│   └── kagenti-webhook/      # Helm chart for the webhook
├── .github/
│   ├── workflows/            # CI/CD (ci.yaml, build.yaml, goreleaser.yml, e2e-kind.yaml, spellcheck, security-scans)
│   └── ISSUE_TEMPLATE/       # Bug report, feature request, epic templates
├── .goreleaser.yaml          # GoReleaser config (webhook binary + ko image + Helm chart)
├── .pre-commit-config.yaml   # Pre-commit hooks (trailing whitespace, go fmt/vet, helmlint)
└── CLAUDE.md                 # This file
```

## The Three Major Components

### 1. kagenti-webhook (Go / Kubebuilder)

A Kubernetes **mutating admission webhook** that intercepts workload creation (Deployments, StatefulSets, DaemonSets, Jobs, CronJobs) and automatically injects AuthBridge sidecar containers.

**Location:** `kagenti-webhook/`
**Language:** Go 1.26, controller-runtime v0.23, Kubebuilder v4
**Detailed guide:** [`kagenti-webhook/CLAUDE.md`](kagenti-webhook/CLAUDE.md)

**Key facts:**
- Webhook: **AuthBridge** at `/mutate-workloads-authbridge`
- Injection controlled via pod labels (`kagenti.io/type`, `kagenti.io/inject`) and per-sidecar opt-out labels (`kagenti.io/envoy-proxy-inject`, `kagenti.io/spiffe-helper-inject`, `kagenti.io/client-registration-inject`)
- Shared `PodMutator` instance (in `internal/webhook/injector/`)
- Injects: `proxy-init` (init), `envoy-proxy`, `spiffe-helper`, `kagenti-client-registration` — all opt-out via workload labels or feature gates. When `featureGates.combinedSidecar=true`, sidecars are merged into a single `authbridge` container.
- Build: `cd kagenti-webhook && make build` / `make test` / `make docker-build`
- Local dev: `cd kagenti-webhook && make local-dev CLUSTER=<kind-cluster>`

### 2. AuthProxy (Go)

An **Envoy proxy with a gRPC external processor** that provides transparent traffic interception for both inbound JWT validation and outbound OAuth 2.0 token exchange (RFC 8693).

**Location:** `AuthBridge/AuthProxy/`
**Language:** Go 1.24
**Detailed guide:** [`AuthBridge/CLAUDE.md`](AuthBridge/CLAUDE.md)

**Core components:**
- `go-processor/main.go` — gRPC ext-proc server (inbound JWT validation, outbound token exchange)
- `init-iptables.sh` — Traffic interception setup (Istio ambient mesh compatible)
- `Dockerfile.{envoy,init}` — Container images

**Ports:** 15123 (outbound), 15124 (inbound), 9090 (ext-proc), 9901 (admin)

### 3. Client Registration (Python)

A Python script that **automatically registers Kubernetes workloads as Keycloak OAuth2 clients** using their SPIFFE identity.

**Location:** `AuthBridge/client-registration/`
**Language:** Python 3.12
**Detailed guide:** [`AuthBridge/CLAUDE.md`](AuthBridge/CLAUDE.md)

**Flow:** Reads SPIFFE ID from JWT, registers client in Keycloak, writes secret to `/shared/client-secret.txt`

## How the Components Work Together

```
                    Workload Creation
                          │
                          ▼
               ┌─────────────────────┐
               │  kagenti-webhook    │  Intercepts CREATE/UPDATE
               │  (admission webhook)│  via MutatingWebhookConfiguration
               └──────────┬──────────┘
                          │ Injects sidecars
                          ▼
         ┌────────────────────────────────────┐
         │            WORKLOAD POD            │
         │                                    │
         │  proxy-init (init) ─► iptables     │
         │                                    │
         │  spiffe-helper ──► SPIRE Agent     │
         │       │ writes JWT SVID            │
         │       ▼                            │
         │  client-registration ──► Keycloak  │
         │       │ writes client secret       │
         │       ▼                            │
         │  envoy-proxy (+ go-processor)      │
         │    - Inbound: JWT validation       │
         │    - Outbound: token exchange       │
         │       │                            │
         │  Your Application                  │
         └────────────────────────────────────┘
```

When `featureGates.combinedSidecar=true`, the three long-running sidecars are merged into a single `authbridge` container (proxy-init remains separate):

```
         ┌────────────────────────────────────┐
         │            WORKLOAD POD            │
         │                                    │
         │  proxy-init (init) ─► iptables     │
         │                                    │
         │  authbridge (combined sidecar)     │
         │    spiffe-helper ──► SPIRE Agent   │
         │    client-registration ──► Keycloak│
         │    envoy-proxy + go-processor      │
         │       │                            │
         │  Your Application                  │
         └────────────────────────────────────┘
```

## CI/CD Workflows

| Workflow | Trigger | Purpose |
|----------|---------|---------|
| `ci.yaml` | PR to main/release-* | Go fmt, vet, build across all Go modules; Python tests |
| `build.yaml` | Tag push (`v*`) or manual | Multi-arch Docker builds for: client-registration, auth-proxy, proxy-init, envoy-with-processor, authbridge, demo-app |
| `goreleaser.yml` | Tag push (`v*`) | GoReleaser binary + ko image for webhook, Helm chart package + push |
| `e2e-kind.yaml` | PR to main/release-* | End-to-end tests on a Kind cluster (webhook injection) |
| `security-scans.yaml` | PR to main | Dependency review, shellcheck, YAML lint, Hadolint, Bandit, Trivy, CodeQL |
| `scorecard.yaml` | Weekly / push to main | OpenSSF Scorecard security health metrics |
| `spellcheck_action.yml` | PR | Spellcheck on markdown files |

### PR Title Convention

PRs must follow **conventional commits** format:

```
<type>: <Subject starting with uppercase>
```

Types: `feat`, `fix`, `docs`, `style`, `refactor`, `perf`, `test`, `build`, `ci`, `chore`, `revert`

## Container Images

All images are pushed to `ghcr.io/kagenti/kagenti-extensions/`:

| Image | Source | Description |
|-------|--------|-------------|
| `kagenti-webhook` | `kagenti-webhook/Dockerfile` | Admission webhook manager (Go binary in distroless) |
| `envoy-with-processor` | `AuthBridge/AuthProxy/Dockerfile.envoy` | Envoy 1.28 + go-processor ext-proc |
| `proxy-init` | `AuthBridge/AuthProxy/Dockerfile.init` | Alpine + iptables init container |
| `client-registration` | `AuthBridge/client-registration/Dockerfile` | Python Keycloak client registrar |
| `authbridge` | `AuthBridge/AuthProxy/Dockerfile.authbridge` | Combined sidecar (Envoy + go-processor + spiffe-helper + client-registration) |
| `auth-proxy` | `AuthBridge/AuthProxy/Dockerfile` | Example pass-through proxy (for demos) |
| `demo-app` | `AuthBridge/AuthProxy/quickstart/demo-app/Dockerfile` | Demo target service |

## Helm Chart

**Location:** `charts/kagenti-webhook/`
**Published to:** `oci://ghcr.io/kagenti/kagenti-extensions/kagenti-webhook-chart`

Key values:
- `image.repository` / `image.tag` — Webhook manager image (tag is `__PLACEHOLDER__` in source, replaced at release time)
- `webhook.enableClientRegistration` — Controls `--enable-client-registration` flag
- `certManager.enabled` — Uses cert-manager for webhook TLS certificates
- Templates include: Deployment, Service, ServiceAccount, RBAC, CertManager Certificate/Issuer, MutatingWebhookConfigurations (authbridge, agent)

Install:
```bash
helm install kagenti-webhook oci://ghcr.io/kagenti/kagenti-extensions/kagenti-webhook-chart \
  --version <version> \
  --namespace kagenti-webhook-system \
  --create-namespace
```

## Pre-commit Hooks

Install: `pre-commit install`

Hooks:
- `trailing-whitespace`, `end-of-file-fixer`, `check-added-large-files` (max 1024KB), `check-yaml`, `check-json`, `check-merge-conflict`, `mixed-line-ending`
- `helmlint` — Runs on `charts/` directory
- `ai-assisted-by-trailer` — Rewrites `Co-Authored-By` to `Assisted-By` (commit-msg stage)
- `ruff`, `ruff-format` — Python linting/formatting on `AuthBridge/` files
- `go-fmt`, `go-vet`, `go-mod-tidy` — Runs on both `kagenti-webhook/` and `AuthBridge/AuthProxy/` Go files

## Languages and Tech Stack

| Area | Technology |
|------|------------|
| Webhook | Go 1.26, controller-runtime v0.23, Kubebuilder v4 |
| AuthProxy ext-proc | Go 1.24, envoy-control-plane, lestrrat-go/jwx |
| Client Registration | Python 3.12, python-keycloak, PyJWT |
| Proxy | Envoy 1.28 |
| Traffic interception | iptables (via init container) |
| Identity | SPIFFE/SPIRE (JWT-SVIDs) |
| Auth provider | Keycloak (OAuth2/OIDC, token exchange RFC 8693) |
| Packaging | Docker, ko, GoReleaser, Helm 3 |
| Testing | Ginkgo/Gomega (Go), envtest (controller-runtime) |
| CI | GitHub Actions |

## External Dependencies and Services

| Service | Required | Purpose |
|---------|----------|---------|
| Kubernetes | Yes | Target platform (v1.25+ recommended) |
| cert-manager | Yes (for webhook) | TLS certificates for webhook server |
| Keycloak | Yes (for AuthBridge) | OAuth2/OIDC provider, token exchange |
| SPIRE | Optional | SPIFFE identity (JWT-SVIDs) for workloads |
| Prometheus | Optional | Metrics collection (ServiceMonitor) |

## ConfigMaps and Secrets Expected at Runtime

When the webhook injects sidecars, the target namespace needs these resources:

| Resource | Kind | Used by | Keys |
|----------|------|---------|------|
| `authbridge-config` | ConfigMap | client-registration, envoy-proxy (ext-proc) | `KEYCLOAK_URL`, `KEYCLOAK_REALM`, `PLATFORM_CLIENT_IDS` (optional), `TOKEN_URL` (optional, derived from KEYCLOAK_URL+KEYCLOAK_REALM), `ISSUER` (optional, derived or explicit for split-horizon DNS), `EXPECTED_AUDIENCE` (optional), `DEFAULT_OUTBOUND_POLICY` (optional, defaults to `passthrough`). Target audience and scopes are configured per-route in `authproxy-routes`. |
| `keycloak-admin-secret` | Secret | client-registration | `KEYCLOAK_ADMIN_USERNAME`, `KEYCLOAK_ADMIN_PASSWORD` |
| `authproxy-routes` | ConfigMap (optional) | envoy-proxy (ext-proc) | `routes.yaml` -- per-host token exchange rules (see AuthBridge/CLAUDE.md for format) |
| `spiffe-helper-config` | ConfigMap | spiffe-helper | SPIFFE helper configuration file |
| `envoy-config` | ConfigMap | envoy-proxy | Envoy YAML configuration |

**Note:** `authproxy-routes` is optional. Without it, all outbound traffic passes through unchanged (the default policy is `passthrough`). Only create it when the agent needs to call services that require token exchange. Set `DEFAULT_OUTBOUND_POLICY: "exchange"` in `authbridge-config` to restore the legacy behavior.

## Common Development Tasks

### Building Everything Locally

```bash
# Webhook
cd kagenti-webhook && make build && make test

# AuthProxy images
cd AuthBridge/AuthProxy && make build-images

# Client registration (no separate build needed, uses Dockerfile directly)
```

### Running the Full Demo

1. Set up a Kind cluster with SPIRE + Keycloak (use [Kagenti Ansible installer](https://github.com/kagenti/kagenti/blob/main/docs/install.md))
2. Deploy the webhook: `cd kagenti-webhook && make local-dev CLUSTER=<name>`
3. See the [AuthBridge demos index](AuthBridge/demos/README.md) for a recommended learning path:
   - **Getting started**: `AuthBridge/demos/weather-agent/demo-ui.md` (inbound validation, UI deployment)
   - **Full flow**: `AuthBridge/demos/github-issue/demo-ui.md` (token exchange + scope-based access)
   - **Webhook internals**: `AuthBridge/demos/webhook/README.md`
   - **Manual deployment**: `AuthBridge/demos/single-target/demo.md`

### Quick Webhook Iteration

```bash
cd kagenti-webhook
./scripts/webhook-rollout.sh           # Build + deploy to Kind
# or with AuthBridge demo setup:
AUTHBRIDGE_DEMO=true ./scripts/webhook-rollout.sh
```

### Adding a New Component Image to CI

1. Add entry to `.github/workflows/build.yaml` matrix (`image_config` array)
2. Provide `name`, `context`, and `dockerfile` fields
3. Image will be pushed to `ghcr.io/kagenti/kagenti-extensions/<name>`

## Code Style and Conventions

### Go Code (webhook, AuthProxy)
- Use `go fmt` (enforced by pre-commit and CI)
- Use `go vet` (enforced by pre-commit and CI)
- Kubebuilder markers (`+kubebuilder:webhook:...`) generate webhook manifests -- run `make manifests` after changes
- Logger names: lowercase-hyphenated (e.g., `logf.Log.WithName("pod-mutator")`)
- Apache 2.0 license header in all Go files (template at `kagenti-webhook/hack/boilerplate.go.txt`)

### Python Code (client-registration)
- Python 3.12+ syntax (type hints with `str | None`)
- Dependencies in `requirements.txt` (version-pinned, e.g. `python-keycloak==5.3.1`)
- UID/GID 1000 in Dockerfile must match `ClientRegistrationUID`/`ClientRegistrationGID` in webhook's `container_builder.go`

### Kubernetes Manifests
- Example deployment YAMLs in `AuthBridge/demos/*/k8s/`
- Helm templates in `charts/kagenti-webhook/templates/`
- Helm templates excluded from YAML check in pre-commit (they contain Go template syntax)

### Shell Scripts
- `set -euo pipefail` (strict mode)
- Extensive inline documentation (especially `init-iptables.sh`)

## Important Cross-Component Relationships

1. **UID/GID Sync:** The `client-registration` Dockerfile creates a user with UID/GID 1000. The webhook's `container_builder.go` sets `runAsUser: 1000` / `runAsGroup: 1000`. These MUST match. In combined mode (`authbridge` container), everything runs as UID 1337 instead.

2. **Envoy Proxy UID:** Envoy runs as UID 1337. The `proxy-init` iptables rules exclude this UID from redirection to prevent loops. Both `container_builder.go` and `init-iptables.sh` use this value. The combined `authbridge` container also runs as UID 1337.

3. **Shared Volume Contract:** The sidecars communicate through shared volumes:
   - `/opt/jwt_svid.token` — spiffe-helper writes, client-registration reads
   - `/shared/client-id.txt` — client-registration writes, envoy-proxy reads
   - `/shared/client-secret.txt` — client-registration writes, envoy-proxy reads

4. **Port Coordination:** Envoy listens on 15123 (outbound) and 15124 (inbound). The ext-proc listens on 9090. The `proxy-init` iptables rules redirect to these ports. The webhook's `container_builder.go` exposes these ports on the container spec.

5. **Image References:** Default image tags are defined in `kagenti-webhook/internal/webhook/config/defaults.go` (via `CompiledDefaults()`). The `ContainerBuilder` reads from `PlatformConfig` at runtime. The CI in `build.yaml` builds the images. Both must stay in sync.

## Gotchas and Known Issues

1. **Config system is wired in:** `kagenti-webhook/internal/webhook/config/` (PlatformConfig, FeatureGates, loaders) is used by `PodMutator` and `ContainerBuilder`. Feature gates support hot-reload via `FeatureGateLoader`. Platform config (images, ports, resources) is loaded at startup from the `kagenti-webhook-defaults` ConfigMap, with `CompiledDefaults()` as the fallback.

2. **Two Go modules:** The repo has two independent Go modules (`kagenti-webhook/go.mod` and `AuthBridge/AuthProxy/go.mod`) with different Go versions (1.26 vs 1.24). They do not share code.

3. **Helm chart tag placeholder:** `charts/kagenti-webhook/values.yaml` uses `tag: "__PLACEHOLDER__"`. The goreleaser workflow replaces this at release time. For local dev, override with `--set image.tag=<tag>`.

4. **Avoid committing venvs:** Virtual environment directories (e.g. `AuthBridge/AuthProxy/quickstart/venv/`) should be gitignored (the repo's `.gitignore` has a `venv` pattern). Do not create and commit new virtual environments under version control.

5. **CI Go version alignment:** Ensure the Go version in `ci.yaml` matches the highest Go version required across all modules (currently Go 1.26, matching `kagenti-webhook/go.mod`).

6. **Envoy config not embedded:** The envoy-proxy sidecar mounts `envoy-config` ConfigMap at `/etc/envoy`. This ConfigMap must exist in the target namespace before workloads are created.

7. **Outbound policy is passthrough by default:** The go-processor defaults to passing outbound traffic through unchanged. Token exchange only happens for hosts explicitly listed in the `authproxy-routes` ConfigMap. Target audience and scopes are configured per-route in `authproxy-routes`.

8. **Route host patterns use short service names:** The `host` field in `authproxy-routes` matches against the HTTP `Host` header, which is typically just the short Kubernetes service name (e.g., `github-tool-mcp`), not the FQDN. Glob patterns (`*`) are supported but the most common case is a plain service name.

## DCO Sign-Off (Mandatory)

All commits **must** include a `Signed-off-by` trailer (Developer Certificate of Origin).
Always use the `-s` flag when committing:

```sh
git commit -s -m "feat: Add new feature"
```

This adds a line like `Signed-off-by: Your Name <your@email.com>` to the commit message.
PRs without DCO sign-off will fail CI checks. To retroactively sign-off existing commits:

```sh
git rebase --signoff main
```

## Orchestration

This repo includes orchestrate skills for enhancing related repositories.
Run `/orchestrate <repo-url>` to start.

| Skill | Description |
|-------|-------------|
| `orchestrate` | Entry point — scan, plan, execute phases |
| `orchestrate:scan` | Assess repo structure, CI, security gaps |
| `orchestrate:plan` | Create phased enhancement plan |
| `orchestrate:precommit` | Add pre-commit hooks and linting |
| `orchestrate:tests` | Add test infrastructure |
| `orchestrate:ci` | Add CI/CD workflows |
| `orchestrate:security` | Add security governance files |
| `orchestrate:replicate` | Bootstrap skills into target repo |
| `orchestrate:review` | Review all orchestration PRs before merge |

Skills management:

| Skill | Description |
|-------|-------------|
| `skills` | Skills router — create, validate, scan |
| `skills:write` | Create or edit skills with proper structure |
| `skills:validate` | Validate skill format and naming |
| `skills:scan` | Audit repo for skill gaps |

## Commit Attribution Policy

When creating git commits, do NOT use `Co-Authored-By` trailers for AI attribution.
Instead, use `Assisted-By` to acknowledge AI assistance without inflating contributor stats:

    Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>

Never add `Co-authored-by`, `Made-with`, or similar trailers that GitHub parses as co-authorship.

A `commit-msg` hook in `scripts/hooks/commit-msg` enforces this automatically.
Install it via pre-commit:

```sh
pre-commit install --hook-type pre-commit --hook-type commit-msg
```
