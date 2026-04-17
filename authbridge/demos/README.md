# AuthBridge Demos

This directory contains demo scenarios showing AuthBridge providing zero-trust
authentication for Kubernetes agent workloads. Each demo progressively introduces
more AuthBridge capabilities.

> **Note:** These demos use the `authbridge-unified` image with operator-injected
> sidecars. See [`cmd/authbridge/README.md`](../cmd/authbridge/README.md) for details
> on the unified authbridge binary.

## Available Demos

| Demo | Difficulty | What It Shows | Deployment |
|------|:----------:|---------------|:----------:|
| **[Weather Agent](weather-agent/demo-ui.md)** | Beginner | Inbound JWT validation, automatic identity registration, outbound passthrough | UI |
| **[GitHub Issue Agent](github-issue/demo.md)** | Intermediate | Inbound validation + outbound token exchange + scope-based access control | [UI](github-issue/demo-ui.md) or [Manual](github-issue/demo-manual.md) |
| **[Webhook](webhook/README.md)** | Intermediate | Webhook-based sidecar injection with auth-target demo app | Manual |
| **[Single Target](single-target/demo.md)** | Advanced | Manual AuthBridge deployment (no webhook) with SPIFFE identity | Manual |
| **[Multi-Target](multi-target/demo.md)** | Advanced | Route-based token exchange to multiple target services | Manual |

## Recommended Path

**New to AuthBridge?** Start with the demos in this order:

1. **[Weather Agent](weather-agent/demo-ui.md)** — Fastest way to see AuthBridge
   in action. Deploys via the Kagenti UI with inbound JWT validation protecting
   the agent. No token exchange configuration needed; outbound traffic uses the
   default passthrough policy.

2. **[GitHub Issue Agent](github-issue/demo.md)** — Full AuthBridge demo with
   inbound validation *and* outbound token exchange. Shows how AuthBridge
   transparently exchanges tokens when the agent calls the GitHub tool, with
   scope-based access control (Alice vs Bob).

3. **[Multi-Target](multi-target/demo.md)** — Advanced routing with per-host
   token exchange configuration. Shows how a single agent can communicate with
   multiple target services, each requiring different audience tokens.

## What Each Demo Covers

### Weather Agent (Getting Started)
- Deploy agent + tool via **Kagenti UI**
- AuthBridge inbound JWT validation (signature, issuer, audience)
- Automatic SPIFFE identity registration with Keycloak
- Default outbound passthrough — agents work out-of-the-box with any tool or LLM
- CLI testing: public endpoints, token rejection, valid token

### GitHub Issue Agent (Full AuthBridge Flow)
- Deploy agent + tool via **Kagenti UI** or **kubectl**
- Keycloak configuration for token exchange (realm, clients, scopes)
- Inbound JWT validation protecting the agent
- Outbound OAuth 2.0 token exchange (RFC 8693) — agent-scoped token exchanged
  for tool-scoped token
- Subject preservation through exchange (`sub` claim maintained)
- Scope-based access control: Alice (public repos) vs Bob (all repos)
- Comprehensive CLI testing and AuthProxy log verification

### Webhook Demo
- Demonstrates the [kagenti-operator](https://github.com/kagenti/kagenti-operator) sidecar injection mechanism
- Deploys a generic agent + auth-target (not a real-world agent)
- Tests inbound validation and outbound token exchange end-to-end
- Good for understanding the injection labels and ConfigMap requirements

### Single Target
- Manual deployment without the webhook (all sidecars in the YAML)
- SPIFFE-based identity with SPIRE
- Single agent → single target with token exchange
- Good for understanding AuthBridge internals

### Multi-Target
- Route-based token exchange using `authproxy-routes` ConfigMap
- One agent communicating with multiple target services
- Each target gets a token with the correct audience
- Uses `keycloak_sync.py` for declarative scope management

## Prerequisites

All demos require:
- A Kubernetes cluster with the Kagenti platform installed
  ([Installation Guide](https://github.com/kagenti/kagenti/blob/main/docs/install.md))
- Keycloak deployed in the `keycloak` namespace
- SPIRE deployed (for demos using SPIFFE identity)

UI-based demos additionally require:
- The Kagenti UI running at `http://kagenti-ui.localtest.me:8080`

## Common Setup: Keycloak Port-Forward

Most demos need Keycloak accessible at `http://keycloak.localtest.me:8080`.
If not already available via an ingress:

```bash
kubectl port-forward service/keycloak-service -n keycloak 8080:8080
```

## Common Setup: Python Environment

Demos that configure Keycloak need a Python virtual environment:

```bash
cd authbridge

python -m venv venv
source venv/bin/activate
pip install --upgrade pip
pip install -r requirements.txt
```

## Related Documentation

- [AuthBridge Overview](../README.md) — Architecture and design
- [AuthBridge Binary](../cmd/authbridge/README.md) — Unified authbridge binary
  supporting ext_proc, ext_authz, and proxy modes
- [Kagenti Operator](https://github.com/kagenti/kagenti-operator) — Admission webhook for sidecar injection (migrated from this repo)
