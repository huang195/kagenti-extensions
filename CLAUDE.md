# CLAUDE.md - Kagenti Extensions

This file provides context for Claude (AI assistant) when working with the `kagenti-extensions` monorepo.

## AI Assistant Instructions

- **No attribution** in commits, PR bodies, or issues — do not add "Co-Authored-By: Claude", "Generated with Claude Code", or any AI attribution.

## Repository Overview

**kagenti-extensions** contains Kubernetes security extensions for the [Kagenti](https://github.com/kagenti/kagenti) ecosystem. It provides **zero-trust authentication** for Kubernetes workloads through transparent token exchange and dynamic Keycloak client registration using SPIFFE/SPIRE identities.

The sidecar injection webhook lives in a separate repo: [kagenti/kagenti-operator](https://github.com/kagenti/kagenti-operator).

**GitHub:** `github.com/kagenti/kagenti-extensions`
**Container registry:** `ghcr.io/kagenti/kagenti-extensions/<image-name>`
**License:** Apache 2.0

## Top-Level Directory Structure

```
kagenti-extensions/
├── authbridge/               # Authentication bridge components
│   ├── authproxy/            #   Envoy + ext-proc sidecar (Go) — token validation & exchange
│   │   ├── go-processor/     #     gRPC ext-proc server (inbound JWT validation, outbound token exchange)
│   │   ├── quickstart/       #     Standalone demo (no SPIFFE)
│   │   └── k8s/              #     Standalone K8s manifests
│   ├── client-registration/  #   Keycloak auto-registration (Python)
│   ├── spiffe-helper/        #   SPIFFE helper Dockerfile (fetches JWT-SVIDs from SPIRE)
│   ├── demos/                #   Demo scenarios (weather-agent, github-issue, webhook, single-target, multi-target)
│   └── keycloak_sync.py      #   Declarative Keycloak sync tool
├── tests/                    # Python tests (client-registration, keycloak_sync)
├── .github/
│   ├── workflows/            # CI/CD (ci.yaml, build.yaml, security-scans, scorecard, spellcheck)
│   └── ISSUE_TEMPLATE/       # Bug report, feature request, epic templates
├── .pre-commit-config.yaml   # Pre-commit hooks (trailing whitespace, go fmt/vet, ruff)
└── CLAUDE.md                 # This file
```

## The Two Major Components

### 1. AuthProxy (Go)

An **Envoy proxy with a gRPC external processor** that provides transparent traffic interception for both inbound JWT validation and outbound OAuth 2.0 token exchange (RFC 8693).

**Location:** `authbridge/authproxy/`
**Language:** Go 1.24
**Detailed guide:** [`authbridge/CLAUDE.md`](authbridge/CLAUDE.md)

**Core components:**
- `go-processor/main.go` — gRPC ext-proc server (inbound JWT validation, outbound token exchange)
- `init-iptables.sh` — Traffic interception setup (Istio ambient mesh compatible)
- `Dockerfile.{envoy,init}` — Container images

**Ports:** 15123 (outbound), 15124 (inbound), 9090 (ext-proc), 9901 (admin)

### 2. Client Registration (Python)

A Python script that **automatically registers Kubernetes workloads as Keycloak OAuth2 clients** using their SPIFFE identity.

**Location:** `authbridge/client-registration/`
**Language:** Python 3.12
**Detailed guide:** [`authbridge/CLAUDE.md`](authbridge/CLAUDE.md)

**Flow:** Reads SPIFFE ID from JWT, registers client in Keycloak, writes secret to `/shared/client-secret.txt`

## How the Components Work Together

The kagenti-operator (in a separate repo) injects AuthBridge sidecars into workload pods. Once injected, the sidecars work together:

```
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

## CI/CD Workflows

| Workflow | Trigger | Purpose |
|----------|---------|---------|
| `ci.yaml` | PR to main/release-* | Go fmt, vet, build for AuthProxy; Python tests |
| `build.yaml` | Tag push (`v*`) or manual | Multi-arch Docker builds for: client-registration, auth-proxy, proxy-init, envoy-with-processor, authbridge, demo-app |
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
| `envoy-with-processor` | `authbridge/authproxy/Dockerfile.envoy` | Envoy 1.28 + go-processor ext-proc |
| `proxy-init` | `authbridge/authproxy/Dockerfile.init` | Alpine + iptables init container |
| `client-registration` | `authbridge/client-registration/Dockerfile` | Python Keycloak client registrar |
| `spiffe-helper` | `authbridge/spiffe-helper/Dockerfile` | Fetches SPIFFE credentials from SPIRE |
| `authbridge` | `authbridge/authproxy/Dockerfile.authbridge` | Combined sidecar (Envoy + go-processor + spiffe-helper + client-registration) |
| `auth-proxy` | `authbridge/authproxy/Dockerfile` | Example pass-through proxy (for demos) |
| `demo-app` | `authbridge/authproxy/quickstart/demo-app/Dockerfile` | Demo target service |

## Pre-commit Hooks

Install: `pre-commit install`

Hooks:
- `trailing-whitespace`, `end-of-file-fixer`, `check-added-large-files` (max 1024KB), `check-yaml`, `check-json`, `check-merge-conflict`, `mixed-line-ending`
- `ai-assisted-by-trailer` — Rewrites `Co-Authored-By` to `Assisted-By` (commit-msg stage)
- `ruff`, `ruff-format` — Python linting/formatting on `authbridge/` files
- `go-fmt`, `go-vet` — Runs on `authbridge/authproxy/` Go files

## Languages and Tech Stack

| Area | Technology |
|------|------------|
| AuthProxy ext-proc | Go 1.24, envoy-control-plane, lestrrat-go/jwx |
| Client Registration | Python 3.12, python-keycloak, PyJWT |
| Proxy | Envoy 1.28 |
| Traffic interception | iptables (via init container) |
| Identity | SPIFFE/SPIRE (JWT-SVIDs) |
| Auth provider | Keycloak (OAuth2/OIDC, token exchange RFC 8693) |
| Packaging | Docker |
| CI | GitHub Actions |

## External Dependencies and Services

| Service | Required | Purpose |
|---------|----------|---------|
| Kubernetes | Yes | Target platform (v1.25+ recommended) |
| [kagenti-operator](https://github.com/kagenti/kagenti-operator) | Yes | Injects AuthBridge sidecars into workload pods |
| Keycloak | Yes | OAuth2/OIDC provider, token exchange |
| SPIRE | Optional | SPIFFE identity (JWT-SVIDs) for workloads |

## ConfigMaps and Secrets Expected at Runtime

When the operator injects sidecars, the target namespace needs these resources:

| Resource | Kind | Used by | Keys |
|----------|------|---------|------|
| `authbridge-config` | ConfigMap | client-registration, envoy-proxy (ext-proc) | `KEYCLOAK_URL`, `KEYCLOAK_REALM`, `PLATFORM_CLIENT_IDS` (optional), `TOKEN_URL` (optional, derived from KEYCLOAK_URL+KEYCLOAK_REALM), `ISSUER` (optional, derived or explicit for split-horizon DNS), `DEFAULT_OUTBOUND_POLICY` (optional, defaults to `passthrough`). Inbound audience validation uses `CLIENT_ID` from `/shared/client-id.txt`. Target audience and scopes are configured per-route in `authproxy-routes`. |
| `keycloak-admin-secret` | Secret | client-registration | `KEYCLOAK_ADMIN_USERNAME`, `KEYCLOAK_ADMIN_PASSWORD` |
| `authproxy-routes` | ConfigMap (optional) | envoy-proxy (ext-proc) | `routes.yaml` -- per-host token exchange rules (see authbridge/CLAUDE.md for format) |
| `spiffe-helper-config` | ConfigMap | spiffe-helper | SPIFFE helper configuration file |
| `envoy-config` | ConfigMap | envoy-proxy | Envoy YAML configuration |

**Note:** `authproxy-routes` is optional. Without it, all outbound traffic passes through unchanged (the default policy is `passthrough`). Only create it when the agent needs to call services that require token exchange. Set `DEFAULT_OUTBOUND_POLICY: "exchange"` in `authbridge-config` to restore the legacy behavior.

## Common Development Tasks

### Building Everything Locally

```bash
# AuthProxy images
cd authbridge/authproxy && make build-images

# Client registration (no separate build needed, uses Dockerfile directly)
```

### Running the Full Demo

1. Set up a Kind cluster with SPIRE + Keycloak (use [Kagenti Ansible installer](https://github.com/kagenti/kagenti/blob/main/docs/install.md))
2. Deploy the webhook via [kagenti-operator](https://github.com/kagenti/kagenti-operator)
3. See the [AuthBridge demos index](authbridge/demos/README.md) for a recommended learning path:
   - **Getting started**: `authbridge/demos/weather-agent/demo-ui.md` (inbound validation, UI deployment)
   - **Full flow**: `authbridge/demos/github-issue/demo-ui.md` (token exchange + scope-based access)
   - **Webhook internals**: `authbridge/demos/webhook/README.md`
   - **Manual deployment**: `authbridge/demos/single-target/demo.md`

### Adding a New Component Image to CI

1. Add entry to `.github/workflows/build.yaml` matrix (`image_config` array)
2. Provide `name`, `context`, and `dockerfile` fields
3. Image will be pushed to `ghcr.io/kagenti/kagenti-extensions/<name>`

## Code Style and Conventions

### Go Code (AuthProxy)
- Use `go fmt` (enforced by pre-commit and CI)
- Use `go vet` (enforced by pre-commit and CI)

### Python Code (client-registration)
- Python 3.12+ syntax (type hints with `str | None`)
- Dependencies in `requirements.txt` (version-pinned, e.g. `python-keycloak==5.3.1`)

### Kubernetes Manifests
- Example deployment YAMLs in `authbridge/demos/*/k8s/`

### Shell Scripts
- `set -euo pipefail` (strict mode)
- Extensive inline documentation (especially `init-iptables.sh`)

## Important Cross-Component Relationships

1. **UID/GID Sync:** The `client-registration` Dockerfile creates a user with UID/GID 1000. The operator's webhook sets `runAsUser: 1000` / `runAsGroup: 1000` when injecting the client-registration container. These MUST match. In combined mode (`authbridge` container), everything runs as UID 1337 instead.

2. **Envoy Proxy UID:** Envoy runs as UID 1337. The `proxy-init` iptables rules exclude this UID from redirection to prevent loops. The combined `authbridge` container also runs as UID 1337.

3. **Shared Volume Contract:** The sidecars communicate through shared volumes:
   - `/opt/jwt_svid.token` — spiffe-helper writes, client-registration reads
   - `/shared/client-id.txt` — client-registration writes, envoy-proxy reads
   - `/shared/client-secret.txt` — client-registration writes, envoy-proxy reads

4. **Port Coordination:** Envoy listens on 15123 (outbound) and 15124 (inbound). The ext-proc listens on 9090. The `proxy-init` iptables rules redirect to these ports.

## Gotchas and Known Issues

1. **One Go module:** The repo has a single Go module at `authbridge/authproxy/go.mod` (Go 1.24).

2. **Avoid committing venvs:** Virtual environment directories (e.g. `authbridge/authproxy/quickstart/venv/`) should be gitignored (the repo's `.gitignore` has a `venv` pattern). Do not create and commit new virtual environments under version control.

3. **Envoy config not embedded:** The envoy-proxy sidecar mounts `envoy-config` ConfigMap at `/etc/envoy`. This ConfigMap must exist in the target namespace before workloads are created.

4. **Outbound policy is passthrough by default:** The go-processor defaults to passing outbound traffic through unchanged. Token exchange only happens for hosts explicitly listed in the `authproxy-routes` ConfigMap. Target audience and scopes are configured per-route in `authproxy-routes`.

5. **Route host patterns use short service names:** The `host` field in `authproxy-routes` matches against the HTTP `Host` header, which is typically just the short Kubernetes service name (e.g., `github-tool-mcp`), not the FQDN. Glob patterns (`*`) are supported but the most common case is a plain service name.

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
