# Multi-Target Demo

This demo shows AuthBridge performing route-based token exchange to multiple target services.

## Overview

```
Agent Pod                                    Target Pods
+------------------+                         +------------------+
|                  |                         |  target-alpha    |
|   agent          |    token exchange       |  aud: target-alpha
|                  |------------------------>+------------------+
|                  |         |
|   AuthProxy      |         |               +------------------+
|   sidecar        |---------|-------------->|  target-beta     |
|                  |         |               |  aud: target-beta |
+------------------+         |               +------------------+
                             |
                             |               +------------------+
                             +-------------->|  target-gamma    |
                                             |  aud: target-gamma
                                             +------------------+
```

The AuthProxy automatically exchanges tokens based on the destination host:

| Host Pattern | Target Audience | Scope |
|--------------|-----------------|-------|
| `target-alpha-service**` | `target-alpha` | `target-alpha-aud` |
| `target-beta-service**` | `target-beta` | `target-beta-aud` |
| `target-gamma-service**` | `target-gamma` | `target-gamma-aud` |

## Prerequisites

- Kubernetes cluster with SPIRE installed
- Keycloak running in the cluster
- AuthBridge images built and loaded

## Setup

### 1. Build Images

Build the AuthProxy images (includes the ext-proc with route-based exchange):

```bash
cd authproxy

# Build all images
make build-images

# Load into Kind (default cluster name is "kagenti")
make load-images

# Or specify a different cluster name
make load-images KIND_CLUSTER_NAME=<your-cluster-name>
```

This builds:
- `envoy-with-processor` - Envoy + ext-proc (go-processor with TargetResolver)
- `demo-app` - Target service that validates JWT audience
- `auth-proxy` - Auth proxy container
- `proxy-init` - iptables init container

### 2. Sync Routes with Keycloak

**Important:** This step must be done before deploying pods, as the client-registration
init container requires the realm to exist.

Port-forward Keycloak:

```bash
kubectl port-forward service/keycloak-service -n keycloak 8080:8080
```

Use `keycloak_sync.py` to reconcile the routes configuration with Keycloak:

```bash
cd authbridge

# Create virtual environment (if not already done)
python -m venv venv
source venv/bin/activate
pip install -r requirements.txt

# Dry run first to see what would be created
python keycloak_sync.py --config demos/multi-target/routes.yaml --dry-run

# Apply changes (interactive prompts)
# Use --agent-client to pre-create the agent and assign scopes to it
python keycloak_sync.py --config demos/multi-target/routes.yaml \
  --agent-client "spiffe://localtest.me/ns/authbridge/sa/agent"

# Or auto-approve all changes
python keycloak_sync.py --config demos/multi-target/routes.yaml \
  --agent-client "spiffe://localtest.me/ns/authbridge/sa/agent" --yes
```

This reconciles routes.yaml with Keycloak, creating:
- The `kagenti` realm (if it doesn't exist)
- The agent client (if `--agent-client` is specified and it doesn't exist)
- `target-alpha`, `target-beta`, `target-gamma` clients (targets)
- Audience scopes (`target-alpha-aud`, etc.) with audience mappers
- Hostname attributes on each target client
- Assigns scopes to the agent client (so it can request tokens for each audience)

### 3. Deploy the Demo

Deploy everything (agent, targets, routes):

```bash
# With SPIFFE (requires SPIRE)
kubectl apply -f demos/multi-target/k8s/authbridge-deployment.yaml
kubectl apply -f demos/multi-target/k8s/targets.yaml

# OR without SPIFFE
kubectl apply -f demos/multi-target/k8s/authbridge-deployment-no-spiffe.yaml
kubectl apply -f demos/multi-target/k8s/targets.yaml
```

This deploys:
- Agent pod with AuthProxy sidecar
- Routes ConfigMap with multi-target configuration
- Three target service pods (alpha, beta, gamma)

### 4. Wait for Pods

```bash
kubectl wait --for=condition=available --timeout=180s deployment/agent -n authbridge
kubectl wait --for=condition=available --timeout=120s deployment/target-alpha -n authbridge
kubectl wait --for=condition=available --timeout=120s deployment/target-beta -n authbridge
kubectl wait --for=condition=available --timeout=120s deployment/target-gamma -n authbridge
```

## Test the Flow

Run the demo script to see token exchange in action:

```bash
./demos/multi-target/run-demo-commands.sh
```

Expected output:

```
=== Original Token (before exchange) ===
  Client ID:  spiffe://localtest.me/ns/authbridge/sa/agent
  Audience:   (none - client credentials token)
  Scopes:     profile email

=== After Token Exchange (for target-alpha) ===
  Audience:   target-alpha
  Scopes:     openid profile target-alpha-aud email

=== Calling All Targets (AuthBridge exchanges automatically) ===

Target Alpha:
authorized
Target Beta:
authorized
Target Gamma:
authorized

=== AuthBridge Token Exchange Logs ===
[Resolver] Host "target-alpha-service" matched "target-alpha-service**"
[Resolver] Using route target_audience: target-alpha
[Token Exchange] Successfully exchanged token, replacing Authorization header
[Resolver] Host "target-beta-service" matched "target-beta-service**"
[Resolver] Using route target_audience: target-beta
[Token Exchange] Successfully exchanged token, replacing Authorization header
[Resolver] Host "target-gamma-service" matched "target-gamma-service**"
[Resolver] Using route target_audience: target-gamma
[Token Exchange] Successfully exchanged token, replacing Authorization header
```

The output shows:
1. **Original token** has no audience and basic scopes (`profile email`)
2. **After exchange** the token has `target-alpha` as audience and includes `target-alpha-aud` scope
3. **All targets** return "authorized" because AuthBridge exchanges the token automatically
4. **Logs** show each host being matched and the token exchange succeeding

## How It Works

1. Agent obtains a token from Keycloak using client credentials (no specific audience)
2. Agent makes HTTP request to a target service with this token
3. Envoy intercepts the request and sends headers to the ext-proc (go-processor)
4. ext-proc resolves the destination host against `routes.yaml` configuration
5. ext-proc performs OAuth 2.0 Token Exchange (RFC 8693) to get a new token with:
   - The target's audience (e.g., `target-alpha`)
   - The target's required scopes (e.g., `openid target-alpha-aud`)
6. Envoy forwards the request with the exchanged token
7. Target validates the token audience and returns "authorized"

## Routes Configuration

The `routes.yaml` file maps hosts to token exchange parameters. Glob patterns are used to match
both short hostnames and FQDNs:

```yaml
# Target Alpha - matches both short name and FQDN
- host: "target-alpha-service**"
  target_audience: "target-alpha"
  token_scopes: "openid target-alpha-aud"

# Target Beta - matches both short name and FQDN
- host: "target-beta-service**"
  target_audience: "target-beta"
  token_scopes: "openid target-beta-aud"

# Target Gamma - matches both short name and FQDN
- host: "target-gamma-service**"
  target_audience: "target-gamma"
  token_scopes: "openid target-gamma-aud"
```

## Cleanup

Use the teardown script to delete k8s resources and the Keycloak realm:

```bash
./demos/multi-target/teardown-demo.sh
```

The script uses `http://keycloak.localtest.me:8080` by default. Override with `KEYCLOAK_URL` if needed.

Or manually:

```bash
kubectl delete -f demos/multi-target/k8s/authbridge-deployment.yaml
kubectl delete -f demos/multi-target/k8s/targets.yaml
```

To delete the entire namespace:

```bash
kubectl delete namespace authbridge
```
