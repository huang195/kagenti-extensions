# GitHub Issue Agent Demo with AuthBridge (UI Deployment)

This guide walks through deploying the **GitHub Issue Agent** with **AuthBridge**
using the **Kagenti UI** for agent and tool deployment. Infrastructure setup
(webhook, Keycloak, ConfigMaps) is done via CLI, while the agent and tool are
imported and deployed through the Kagenti dashboard.

For a fully manual deployment using only `kubectl`, see [demo-manual.md](demo-manual.md).

This demo extends the [upstream GitHub Issue Agent demo](https://github.com/kagenti/kagenti/blob/main/docs/demos/demo-github-issue.md)
by replacing manual token handling with AuthBridge's automatic token exchange.

## What This Demo Shows

In this demo, we deploy the GitHub Issue Agent and GitHub MCP Tool with AuthBridge
providing end-to-end security:

1. **Agent identity** — The agent automatically registers with Keycloak using its
   SPIFFE ID, with no hardcoded secrets
2. **Inbound validation** — Requests to the agent are validated (JWT signature,
   issuer, and audience) before reaching the agent code
3. **Transparent token exchange** — When the agent calls the GitHub tool, AuthBridge
   automatically exchanges the user's token for one scoped to the tool
4. **Subject preservation** — The end user's identity (`sub` claim) is preserved
   through the exchange, enabling per-user authorization at the tool
5. **Scope-based access** — The tool uses token scopes to determine whether to
   grant public or privileged GitHub API access

## Architecture

```
┌──────────────────────────────────────────────────────────────────────────────────┐
│                              KUBERNETES CLUSTER                                  │
│                                                                                  │
│  ┌───────────────────────────────────────────────────────────────────────────┐   │
│  │                  GIT-ISSUE-AGENT POD (namespace: team1)                   │   │
│  │                                                                           │   │
│  │  ┌─────────────────┐  ┌─────────────┐  ┌──────────────────────────────┐   │   │
│  │  │ git-issue-agent │  │   spiffe-   │  │      client-registration     │   │   │
│  │  │  (A2A agent,    │  │   helper    │  │  (registers with Keycloak    │   │   │
│  │  │   port 8000)    │  │             │  │   using SPIFFE ID)           │   │   │
│  │  └─────────────────┘  └─────────────┘  └──────────────────────────────┘   │   │
│  │                                                                           │   │
│  │  ┌───────────────────────────────────────────────────────────────────┐    │   │
│  │  │                AuthProxy Sidecar (envoy-proxy container)          │    │   │
│  │  │  Envoy + ext_proc (go-processor)                                  │    │   │
│  │  │  Inbound (port 15124):                                            │    │   │
│  │  │    - Validates JWT (signature + issuer + audience via JWKS)       │    │   │
│  │  │    - Returns 401 Unauthorized for invalid/missing tokens          │    │   │
│  │  │  Outbound (port 15123):                                           │    │   │
│  │  │    - HTTP: Exchanges token via Keycloak → aud: github-tool        │    │   │
│  │  │    - HTTPS: TLS passthrough (no interception)                     │    │   │
│  │  └───────────────────────────────────────────────────────────────────┘    │   │
│  └───────────────────────────────────────────────────────────────────────────┘   │
│                                      │                                           │
│                      Exchanged token │(aud: github-tool)                         │
│                                      ▼                                           │
│  ┌───────────────────────────────────────────────────────────────────────────┐   │
│  │                  GITHUB-TOOL POD (namespace: team1)                       │   │
│  │                                                                           │   │
│  │  ┌──────────────────────────────────────────────────────────────────┐     │   │
│  │  │                     github-tool (port 9090)                      │     │   │
│  │  │  - Validates token (aud: github-tool, issuer: Keycloak)          │     │   │
│  │  │  - Token has github-full-access scope? → PRIVILEGED_ACCESS_PAT   │     │   │
│  │  │  - Otherwise → PUBLIC_ACCESS_PAT                                 │     │   │
│  │  └──────────────────────────────────────────────────────────────────┘     │   │
│  └───────────────────────────────────────────────────────────────────────────┘   │
│                                                                                  │
├──────────────────────────────────────────────────────────────────────────────────┤
│                            EXTERNAL SERVICES                                     │
│                                                                                  │
│  ┌──────────────────────┐          ┌──────────────────────┐                      │
│  │   SPIRE (namespace:  │          │ KEYCLOAK (namespace: │                      │
│  │       spire)         │          │     keycloak)        │                      │
│  │                      │          │                      │                      │
│  │  Provides SPIFFE     │          │  - kagenti realm     │                      │
│  │  identities (SVIDs)  │          │  - token exchange    │                      │
│  └──────────────────────┘          └──────────────────────┘                      │
└──────────────────────────────────────────────────────────────────────────────────┘
```

## Key Security Properties

| Property | How It's Achieved |
|----------|-------------------|
| **No hardcoded agent secrets** | Client credentials dynamically generated by client-registration using SPIFFE ID |
| **Identity-based auth** | SPIFFE ID is both the pod identity and the Keycloak client ID |
| **Inbound validation** | [AuthProxy](../../AuthProxy/README.md) validates all incoming requests (JWT signature, issuer, audience) before they reach the agent |
| **Audience-scoped tokens** | Original token scoped to Agent; exchanged token scoped to GitHub tool |
| **User attribution** | `sub` and `preferred_username` preserved through token exchange |
| **Scope-based authorization** | Tool uses token scopes to determine access level (public vs. privileged) |
| **Transparent to agent code** | The agent makes plain HTTP calls; AuthBridge handles all token management |

### Inbound Verification (AuthProxy)

The AuthBridge sidecar includes [AuthProxy](../../AuthProxy/README.md), an Envoy-based
ext_proc that validates **every** inbound request before it reaches the agent. The
ext_proc (port 9090) performs three checks on the `Authorization: Bearer <token>` header:

1. **Signature** — Verifies the JWT signature against Keycloak's JWKS keys
   (auto-refreshed via cache). Rejects tampered or forged tokens.
2. **Issuer** — Confirms the `iss` claim matches the expected Keycloak realm
   (`ISSUER` in `authbridge-config`). Rejects tokens from other identity providers.
3. **Audience** — If `EXPECTED_AUDIENCE` is set, confirms the `aud` claim includes
   the agent's SPIFFE ID. Rejects tokens intended for a different service.

Requests that fail any check receive an immediate `401 Unauthorized` response from
Envoy — the agent application never sees them. This is tested in
[Step 9b–9c](#step-9-test-via-cli-optional).

---

## Prerequisites

Ensure you have completed the Kagenti platform setup as described in the
[Installation Guide](https://github.com/kagenti/kagenti/blob/main/docs/install.md),
including the Kagenti UI.

You should also have:
- The [kagenti-extensions](https://github.com/kagenti/kagenti-extensions) repo cloned
- The Kagenti UI running at `http://kagenti-ui.localtest.me:8080`
- Python 3.9+ with `venv` support
- **Ollama running** with the `ibm/granite4:latest` model (or another model of your choice)
- Two GitHub Personal Access Tokens (PATs):
  - `<PUBLIC_ACCESS_PAT>` — access to public repositories only
  - `<PRIVILEGED_ACCESS_PAT>` — access to all repositories

See the [upstream demo](https://github.com/kagenti/kagenti/blob/main/docs/demos/demo-github-issue.md#required-github-pat-tokens) for instructions on creating GitHub PAT tokens.

---

## Step 1: Configure Keycloak

Keycloak needs to be configured with the correct clients, scopes, and users for the
token exchange flow between the agent and the GitHub tool.

### Port-forward Keycloak (if needed)

The setup script connects to Keycloak at `http://keycloak.localtest.me:8080`.
If Keycloak is not already reachable at that address (e.g., via an ingress),
start a port-forward in a separate terminal:

```bash
kubectl port-forward service/keycloak-service -n keycloak 8080:8080
```

### Run the setup script

```bash
cd AuthBridge

# Create virtual environment (if not already done)
python -m venv venv
source venv/bin/activate
pip install --upgrade pip
pip install -r requirements.txt

# Run the Keycloak setup for this demo
python demos/github-issue/setup_keycloak.py
```

This creates:

| Resource | Name | Purpose |
|----------|------|---------|
| **Realm** | `kagenti` | Keycloak realm for the demo |
| **Client** | `github-tool` | Target audience for token exchange |
| **Scope** | `agent-team1-git-issue-agent-aud` | Realm DEFAULT — auto-adds Agent's SPIFFE ID to all tokens |
| **Scope** | `github-tool-aud` | Realm OPTIONAL — for exchanged tokens targeting the tool |
| **Scope** | `github-full-access` | Realm OPTIONAL — for privileged GitHub API access |
| **User** | `alice` (password: `alice123`) | Regular user — public access |
| **User** | `bob` (password: `bob123`) | Privileged user — full access |

---

## Step 2: Create Keycloak Admin Secret and Apply Demo ConfigMaps

The Kagenti installer creates default ConfigMaps (`environments`,
`spiffe-helper-config`, `envoy-config`, `authbridge-config`) with the correct
`kagenti` realm settings and 300s Envoy timeouts.

The client-registration sidecar needs Keycloak admin credentials to register
agents as OAuth clients. These are stored in a Kubernetes Secret (not a
ConfigMap) to follow security best practices:

```bash
kubectl create secret generic keycloak-admin-secret -n team1 \
  --from-literal=KEYCLOAK_ADMIN_USERNAME=admin \
  --from-literal=KEYCLOAK_ADMIN_PASSWORD=admin \
  --dry-run=client -o yaml | kubectl apply -f -
```

Then apply the demo-specific `authbridge-config` override — the token exchange
target audience (`github-tool`), scopes, and the agent's SPIFFE ID for inbound
audience validation:

```bash
cd AuthBridge

# Override authbridge-config for this demo (sets TARGET_AUDIENCE=github-tool)
kubectl apply -f demos/github-issue/k8s/configmaps.yaml
```

---

## Step 3: Create the GitHub Tool Secrets

The GitHub tool needs PAT tokens to access the GitHub API. Create a Kubernetes secret
with your tokens before importing the tool:

```bash
export PRIVILEGED_ACCESS_PAT=<your-privileged-pat>
export PUBLIC_ACCESS_PAT=<your-public-pat>
```

Provide your actual GitHub Personal Access Tokens.

```bash
kubectl create secret generic github-tool-secrets -n team1 \
  --from-literal=INIT_AUTH_HEADER="Bearer $PRIVILEGED_ACCESS_PAT" \
  --from-literal=UPSTREAM_HEADER_TO_USE_IF_IN_AUDIENCE="Bearer $PRIVILEGED_ACCESS_PAT" \
  --from-literal=UPSTREAM_HEADER_TO_USE_IF_NOT_IN_AUDIENCE="Bearer $PUBLIC_ACCESS_PAT"
```

---

## Step 4: Import the GitHub Tool via Kagenti UI

1. Navigate to [Import Tool](http://kagenti-ui.localtest.me:8080/tools/import)
   in the Kagenti UI.

2. In the **Namespace** drop-down, choose `team1`.

3. Select **Build from Source** as the deployment method.

4. Under **Source Code** select:
   - **Git Repository URL**: `https://github.com/kagenti/agent-examples`
   - **Branch or Tag**: `main`
   - **Example Tools**: `GitHub Tool`
   - **Source Subfolder**: `mcp/github_tool`

5. **Workload Type** select `Deployment`

6. Set **MCP Transport Protocol** to `streamable HTTP`

7. Make sure **Enable AuthBridge sidecar injection** is **unchecked**.

8. Make sure **Enable SPIRE identity (spiffe-helper sidecar)** is **unchecked**.

   > The GitHub tool does not need AuthBridge sidecars — it validates incoming tokens
   > directly using its own JWKS logic. Injecting sidecars would cause a port 9090
   > conflict between the tool's MCP broker and the go-processor gRPC server.

9. Under **Port Configuration**, set **Service Port** to `9090` and **Target Port** to `9090`

   > The tool binary listens on port 9090. The agent's `MCP_URL` connects to
   > `http://github-tool-mcp:9090/mcp`, so both the service port and target port
   > must be 9090 to match.

10. Under **Environment Variables**, click **Import from File/URL**,
    Select **From URL** and provide the `.env` file from this repo:
    - **URL** `https://raw.githubusercontent.com/kagenti/agent-examples/refs/heads/main/mcp/github_tool/.env.authbridge`
    - Click **Fetch & Parse** — this populates all environment variables, including
      Secret references for the PAT tokens and direct values for Keycloak settings.
    - Click **Import** to set all the env. variables.

    The imported variables will show three **Secret** type entries referencing
    `github-tool-secrets` and three **Direct Value** entries for Keycloak configuration.
    No manual editing is needed.

    > **Tip:** You can also upload the file directly from your local system.

11. Click **Build & Deploy New Tool**.

You will be redirected to a **Build Progress** page where you can monitor the
Shipwright build. Wait for it to complete.

### Verify the tool is reachable

Confirm the tool service port is correct and the tool responds:

```bash
kubectl run test-mcp --image=curlimages/curl -n team1 --restart=Never --rm -it -- \
  curl -s -o /dev/null -w "%{http_code}" --max-time 5 http://github-tool-mcp:9090/mcp
# Expected: 200 (SSE connection, may timeout after 5s — that's OK)
```

---

## Step 5: Import the GitHub Issue Agent via Kagenti UI

1. Navigate to [Import Agent](http://kagenti-ui.localtest.me:8080/agents/import)
   in the Kagenti UI.

2. In the **Namespace** drop-down, choose `team1`.

3. Select **Build from Source** as the deployment method.

4. Under **Source Repository** select:
   - **Git Repository URL**: `https://github.com/kagenti/agent-examples`
   - **Git Branch**: `main`
   - **Select Example**: `Git Issue Agent`
   - **Source Path**: `a2a/git_issue_agent`

5. **Protocol**: `A2A`

6. **Framework**: `LangGraph`

7. **Workload Type** select `Deployment`.

8. Make sure **Enable AuthBridge sidecar injection** is checked.

9. Make sure **Enable SPIRE identity (spiffe-helper sidecar)** is checked.

10. Under **Port Configuration**, set **Service Port** to `8080` and **Target Port** to `8000`

11. Under **Environment Variables**, click **Import from File/URL**,
   Select **From URL** and provide the **URL** from this repo:
    - For Ollama: `https://raw.githubusercontent.com/kagenti/agent-examples/refs/heads/main/a2a/git_issue_agent/.env.ollama`
    - For OpenAI: `https://raw.githubusercontent.com/kagenti/agent-examples/refs/heads/main/a2a/git_issue_agent/.env.openai`
    - Click **Fetch & Parse** — this populates all environment variables including
     LLM settings, `MCP_URL`, and `JWKS_URI`. No manual editing is needed.
    - Click **Import** to set all the env. variables.

   The Ollama variant sets all direct values. The OpenAI variant includes
   **Secret** type entries referencing `openai-secret` for `LLM_API_KEY`
   and `OPENAI_API_KEY`.

   > **Tip:** You can also upload the file directly from your local system.
   > **OpenAI prerequisite:** If using OpenAI, create the secret first:
   > ```bash
   > kubectl create secret generic openai-secret -n team1 \
   >   --from-literal=apikey="<YOUR_OPENAI_API_KEY>"
   > ```

12. Click **Build & Deploy Agent**.

Wait for the Shipwright build to complete and the deployment to become ready.

---

## Step 6: Verify the Deployment

### Check pod status

```bash
kubectl get pods -n team1
```

Expected output:

```
NAME                               READY   STATUS    RESTARTS   AGE
git-issue-agent-58768bdb67-xxxxx   4/4     Running   0          2m
github-tool-7f8c9d6b44-yyyyy      1/1     Running   0          5m
```

> **Note:** The agent pod should show **4/4** containers — the agent itself plus
> three AuthBridge sidecars (spiffe-helper, kagenti-client-registration, envoy-proxy)
> injected by the webhook.

### Verify injected containers

```bash
kubectl get pod -n team1 -l app.kubernetes.io/name=git-issue-agent -o jsonpath='{.items[0].spec.containers[*].name}'
```

Expected:

```
agent kagenti-client-registration envoy-proxy spiffe-helper
```

> **Note:** Both the UI and manual deployments use the same naming conventions:
> container name `agent`, labels `app.kubernetes.io/name: git-issue-agent`,
> and service `git-issue-agent:8080`.

### Check client registration

```bash
kubectl logs deployment/git-issue-agent -n team1 -c kagenti-client-registration
```

Expected:

```
SPIFFE credentials ready!
Client ID (SPIFFE ID): spiffe://localtest.me/ns/team1/sa/git-issue-agent
Created Keycloak client "spiffe://localtest.me/ns/team1/sa/git-issue-agent"
Client registration complete!
```

### Check agent logs

```bash
kubectl logs deployment/git-issue-agent -n team1 -c agent
```

Expected:

```
SVID JWT file /opt/jwt_svid.token not found.
SVID JWT file /opt/jwt_svid.token not found.
CLIENT_SECRET file not found at /shared/secret.txt
INFO: JWKS_URI is set - using JWT Validation middleware
INFO:     Started server process [17]
INFO:     Waiting for application startup.
INFO:     Application startup complete.
INFO:     Uvicorn running on http://0.0.0.0:8000 (Press CTRL+C to quit)
```

<!-- WORKAROUND: Remove this warning note once kagenti/agent-examples#129 is fixed. -->

> **These warnings are expected and harmless.** The agent's built-in auth code
> probes for SVID and client-secret files at startup. With AuthBridge, these files
> are used by the sidecars (spiffe-helper, client-registration, Envoy), not by the
> agent container directly. The agent falls back to JWKS-based JWT validation
> (`JWKS_URI is set`), which is the correct behavior — AuthBridge's Envoy sidecar
> handles inbound JWT validation and outbound token exchange on behalf of the agent.
> These warnings will be removed once the agent's built-in auth logic is cleaned up
> ([kagenti/agent-examples#129](https://github.com/kagenti/agent-examples/issues/129)).

### Check the service endpoint

```bash
kubectl get svc -n team1 | grep git-issue-agent
```

Expected:

```
git-issue-agent   ClusterIP   10.96.x.x   <none>   8080/TCP   5m
```

The service maps **port 8080** to the agent's internal port 8000 (same for both
UI and manual deployments).

---

## Step 7: Verify Ollama is Running

The agent uses an LLM for inference. If using Ollama, verify it is running:

```bash
ollama list
```

You should see `ibm/granite4:latest` (or whichever model you configured) on the list.
If Ollama is not running, start it in a separate terminal (`ollama serve`) and ensure the
model is pulled (`ollama pull ibm/granite4:latest`).

> **Note:** The `.env.ollama` file defaults to `LLM_API_BASE=http://host.docker.internal:11434`,
> which reaches Ollama running on your host machine via the Kind/Docker Desktop gateway.
> If you deploy Ollama inside the cluster instead, patch the agent:
> ```bash
> kubectl set env deployment/git-issue-agent -n team1 -c agent \
>   LLM_API_BASE="http://ollama.ollama.svc:11434"
> ```

---

## Step 8: Chat via Kagenti UI

With the platform-wide migration to the `kagenti` realm
([kagenti#764](https://github.com/kagenti/kagenti/pull/764)), both the Kagenti UI
and AuthBridge demos now use the same Keycloak realm. This resolves the previous
realm mismatch issue
([kagenti-extensions#147](https://github.com/kagenti/kagenti-extensions/issues/147)).

1. Navigate to the **Agent Catalog** in the Kagenti UI.
2. Select the `team1` namespace.
3. Under **Available Agents**, select `git-issue-agent` and click **View Details**.
4. Verify the **Agent Card** is visible (this confirms the agent is running and
   the `/.well-known/*` bypass is working).
5. Use the **Chat** panel to send a message, e.g. "List issues in kagenti/kagenti repo".
6. The agent should respond with a list of GitHub issues.

> **Troubleshooting:** If UI chat returns a `401`, verify that both the UI and
> AuthBridge are configured against the same `kagenti` realm. You can also use
> [Step 9: Test via CLI](#step-9-test-via-cli) to test the full AuthBridge flow
> independently.

---

## Step 9: Test via CLI

Test the AuthBridge flow from the command line to verify inbound validation and
token exchange using a `kagenti`-realm token.

> **Note:** The CLI test commands below use the same service name and port
> (`git-issue-agent:8080`) as both the UI and manual deployments.

### Setup

```bash
# Start a test client pod
kubectl run test-client --image=nicolaka/netshoot -n team1 --restart=Never -- sleep 3600
kubectl wait --for=condition=ready pod/test-client -n team1 --timeout=30s
```

### 9a. Agent Card - Public Endpoint (No Token Required)

The `/.well-known/agent.json` endpoint is publicly accessible — AuthBridge's
go-processor [bypasses JWT validation](https://github.com/kagenti/kagenti-extensions/pull/133)
for `/.well-known/*`, `/healthz`, `/readyz`, and `/livez` by default:

```bash
kubectl exec test-client -n team1 -- curl -s \
  http://git-issue-agent:8080/.well-known/agent.json | jq .name
# Expected: "Github issue agent"
```

### 9b. Inbound Rejection - No Token

Non-public endpoints require a valid JWT:

```bash
kubectl exec test-client -n team1 -- curl -s \
  http://git-issue-agent:8080/
# Expected: {"error":"unauthorized","message":"missing Authorization header"}
```

### 9c. Inbound Rejection - Invalid Token (Signature Check)

A malformed or tampered token fails the JWKS signature check:

```bash
kubectl exec test-client -n team1 -- curl -s \
  -H "Authorization: Bearer invalid-token" \
  http://git-issue-agent:8080/
# Expected: {"error":"unauthorized","message":"token validation failed: failed to parse/validate token: ..."}
```

### 9d. End-to-End Test with Valid Token

Open a shell inside the test-client pod to avoid JWT shell expansion issues:

```bash
kubectl exec -it test-client -n team1 -- sh
```

Inside the pod, get credentials and send a request:

```bash
# Get a Keycloak admin token from the kagenti realm
ADMIN_TOKEN=$(curl -s http://keycloak-service.keycloak.svc:8080/realms/kagenti/protocol/openid-connect/token \
  -d "grant_type=password" \
  -d "client_id=admin-cli" \
  -d "username=admin" \
  -d "password=admin" | jq -r ".access_token")

echo "Admin token length: ${#ADMIN_TOKEN}"

# Look up the agent's client in the kagenti realm.
# The client ID is the SPIFFE ID (URL-encoded in the query parameter).
SPIFFE_ID="spiffe://localtest.me/ns/team1/sa/git-issue-agent"
CLIENTS=$(curl -s -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://keycloak-service.keycloak.svc:8080/admin/realms/kagenti/clients" \
  --data-urlencode "clientId=$SPIFFE_ID" --get)
INTERNAL_ID=$(echo "$CLIENTS" | jq -r ".[0].id")
CLIENT_ID=$(echo "$CLIENTS" | jq -r ".[0].clientId")

echo "Internal ID:   $INTERNAL_ID"
echo "Client ID:     $CLIENT_ID"

# Get the client secret (extract directly from the client listing;
# the /client-secret endpoint may return null for auto-registered clients)
CLIENT_SECRET=$(echo "$CLIENTS" | jq -r ".[0].secret")

echo "Secret length: ${#CLIENT_SECRET}"

# Get an OAuth token for the agent
TOKEN=$(curl -s -X POST \
  "http://keycloak-service.keycloak.svc:8080/realms/kagenti/protocol/openid-connect/token" \
  -d "grant_type=client_credentials" \
  --data-urlencode "client_id=$CLIENT_ID" \
  --data-urlencode "client_secret=$CLIENT_SECRET" | jq -r ".access_token")

echo "Token length:  ${#TOKEN}"

# Send a prompt to the agent (A2A v0.3.0)
curl -s --max-time 300 \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -X POST http://git-issue-agent:8080/ \
  -d '{
    "jsonrpc": "2.0",
    "id": "test-1",
    "method": "message/send",
    "params": {
      "message": {
        "role": "user",
        "messageId": "msg-001",
        "parts": [{"type": "text", "text": "List issues in kagenti/kagenti repo"}]
      }
    }
  }' | jq
```

Exit the pod when done:

```bash
exit
```

### 9e. Verify AuthProxy Logs (Inbound + Outbound)

Check the ext_proc logs to confirm both inbound validation and outbound token
exchange are working:

**Inbound validation logs:**

```bash
kubectl logs deployment/git-issue-agent -n team1 -c envoy-proxy 2>&1 | grep "\[Inbound\]"
```

Expected:

```
[Inbound] Token validated - issuer: http://keycloak.localtest.me:8080/realms/kagenti, audience: [spiffe://localtest.me/ns/team1/sa/git-issue-agent ...]
[Inbound] JWT validation succeeded, forwarding request
```

**Outbound token exchange logs:**

```bash
kubectl logs deployment/git-issue-agent -n team1 -c envoy-proxy 2>&1 | grep "^2026/" | grep "\[Token Exchange\]"
```

Expected:

```
[Token Exchange] Token URL: http://keycloak-service.keycloak.svc:8080/realms/kagenti/protocol/openid-connect/token
[Token Exchange] Client ID: spiffe://localtest.me/ns/team1/sa/git-issue-agent
[Token Exchange] Audience: github-tool
[Token Exchange] Scopes: openid github-tool-aud github-full-access
[Token Exchange] Successfully exchanged token
[Token Exchange] Successfully exchanged token, replacing Authorization header
```

### Clean Up Test Client

```bash
kubectl delete pod test-client -n team1 --ignore-not-found
```

---

## Patching Agent Environment (If Needed)

If the agent is missing environment variables after UI deployment (e.g., `MCP_URL`,
`JWKS_URI`, or LLM keys), you can patch the deployment:

```bash
# Set missing env vars on the agent container
kubectl set env deployment/git-issue-agent -n team1 -c agent \
  MCP_URL="http://github-tool-mcp:9090/mcp" \
  JWKS_URI="http://keycloak-service.keycloak.svc:8080/realms/kagenti/protocol/openid-connect/certs"

# If using OpenAI and the key is in a secret:
kubectl patch deployment git-issue-agent -n team1 --type=json -p='[
  {"op":"add","path":"/spec/template/spec/containers/0/env/-","value":{
    "name":"LLM_API_KEY",
    "valueFrom":{"secretKeyRef":{"name":"openai-secret","key":"apikey"}}
  }},
  {"op":"add","path":"/spec/template/spec/containers/0/env/-","value":{
    "name":"OPENAI_API_KEY",
    "valueFrom":{"secretKeyRef":{"name":"openai-secret","key":"apikey"}}
  }}
]'

# Wait for rollout
kubectl rollout status deployment/git-issue-agent -n team1 --timeout=180s
```

---

## How AuthBridge Changes the Original Demo

| Aspect | Original Demo | With AuthBridge |
|--------|--------------|-----------------|
| **Agent secrets** | Manual PAT token configuration | Dynamic credentials via SPIFFE + client-registration |
| **Inbound auth** | No validation | [AuthProxy](../../AuthProxy/README.md) validates JWT (signature, issuer, audience) via ext_proc |
| **Token management** | Agent code handles tokens | Transparent sidecar — agent code unchanged |
| **Token for tool** | Same PAT token passed through | OAuth token exchange (RFC 8693) |
| **User attribution** | No user tracking | `sub` claim preserved through exchange |
| **Access control** | Single PAT for all users | Scope-based: public vs. privileged |

---

## Troubleshooting

### Invalid Client or Invalid Client Credentials

**Symptom:** `{"error":"invalid_client","error_description":"Invalid client or Invalid client credentials"}`

**Cause:** The `keycloak-admin-secret` Secret or `environments` ConfigMap was missing
or incorrect at startup, so the client-registration sidecar couldn't register the client.

**Fix:**

```bash
# 1. Verify the keycloak-admin-secret exists
kubectl get secret keycloak-admin-secret -n team1

# 2. Verify the installer's environments ConfigMap has the correct realm
kubectl get configmap environments -n team1 -o jsonpath='{.data.KEYCLOAK_REALM}'
# Should show: kagenti

# 3. Re-apply the demo ConfigMap and restart
kubectl apply -f demos/github-issue/k8s/configmaps.yaml
kubectl rollout restart deployment/git-issue-agent -n team1
```

### Agent Missing Environment Variables

**Symptom:** Agent returns `JWKS_URI or GITHUB_TOKEN env var must be set` or similar

**Cause:** The UI deployment didn't include all required environment variables.

**Fix:** See the [Patching Agent Environment](#patching-agent-environment-if-needed) section above.

### Service Name Mismatch

**Symptom:** `Couldn't resolve host` when trying to reach the agent

**Fix:** Verify the service exists and check the name/port:

```bash
kubectl get svc -n team1 | grep git-issue-agent
```

Both the UI and manual deployments create `git-issue-agent:8080` (targetPort 8000).

### Upstream Request Timeout

**Symptom:** `upstream request timeout` from Envoy

**Cause:** The LLM inference takes longer than the Envoy route timeout.

**Fix:** The installer's `envoy-config` ConfigMap sets route and ext_proc
timeouts to 300 seconds (5 min). If you still hit timeouts, verify the
ConfigMap has the correct values:

```bash
kubectl get configmap envoy-config -n team1 -o jsonpath='{.data.envoy\.yaml}' | grep "timeout:"
```

If you see `30s` values instead of `300s`, reinstall Kagenti (the installer
creates the correct defaults) and restart the agent:

```bash
kubectl rollout restart deployment/git-issue-agent -n team1
```

### Agent Pod Not Starting (4/4 containers)

**Symptom:** Pod shows 3/4 or less containers ready

**Fix:** Check each container's logs:

```bash
kubectl logs deployment/git-issue-agent -n team1 -c kagenti-client-registration
kubectl logs deployment/git-issue-agent -n team1 -c spiffe-helper
kubectl logs deployment/git-issue-agent -n team1 -c envoy-proxy
kubectl logs deployment/git-issue-agent -n team1 -c agent
```

### Tool MCP Server Unreachable / Connection Reset

**Symptom:** Agent returns `Couldn't connect to the MCP server after 60 seconds`, or
direct curl to the tool gets `Connection reset by peer`.

**Possible causes:**

1. **AuthBridge sidecars injected** — If the webhook injected envoy-proxy into the tool
   pod, the go-processor gRPC server and tool MCP broker both bind to port 9090. Check container count:
   ```bash
   kubectl get pods -n team1 | grep github-tool
   # If you see 3/3 instead of 1/1, sidecars were injected
   ```
   **Fix:** Ensure **Enable AuthBridge sidecar injection** is **unchecked** when importing the tool (Step 4, item 7), then delete and re-import.

2. **Service port mismatch** — Verify the tool service uses port 9090 (matching the agent's `MCP_URL`):
   ```bash
   kubectl get svc github-tool-mcp -n team1 -o jsonpath='{.spec.ports[0].port}:{.spec.ports[0].targetPort}'
   # Should show 9090:9090. If not, patch:
   kubectl patch svc github-tool-mcp -n team1 --type='json' \
     -p='[{"op":"replace","path":"/spec/ports/0/port","value":9090},{"op":"replace","path":"/spec/ports/0/targetPort","value":9090}]'
   ```

### GitHub Tool Returns 401

**Symptom:** Tool rejects the exchanged token

**Fix:** Verify the tool's environment variables match the Keycloak configuration:
- `ISSUER` should be `http://keycloak.localtest.me:8080/realms/kagenti`
- `AUDIENCE` should be `github-tool`

---

## Cleanup

### Via Kagenti UI

1. Go to the **Agent Catalog**, find `git-issue-agent`, and click **Delete**.
2. Go to the **Tool Catalog**, find `github-tool`, and click **Delete**.

### Via CLI

```bash
kubectl delete deployment git-issue-agent -n team1
kubectl delete deployment github-tool -n team1
kubectl delete svc git-issue-agent -n team1
kubectl delete svc github-tool-mcp -n team1
kubectl delete secret github-tool-secrets -n team1
kubectl delete pod test-client -n team1 --ignore-not-found
```

### Delete ConfigMaps

```bash
kubectl delete -f demos/github-issue/k8s/configmaps.yaml
```

### Delete Namespace (removes everything)

```bash
kubectl delete namespace team1
```

### Remove Webhook (optional)

```bash
kubectl delete mutatingwebhookconfiguration kagenti-webhook-authbridge-mutating-webhook-configuration
```

---

## Files Reference

| File | Description |
|------|-------------|
| `demos/github-issue/demo-ui.md` | This guide |
| `demos/github-issue/demo-manual.md` | Fully manual deployment guide |
| `demos/github-issue/setup_keycloak.py` | Keycloak configuration script |
| `demos/github-issue/k8s/configmaps.yaml` | Demo-specific authbridge-config override |
| `demos/github-issue/k8s/git-issue-agent-deployment.yaml` | Agent deployment YAML (manual only) |
| `demos/github-issue/k8s/github-tool-deployment.yaml` | GitHub tool deployment YAML (manual only) |

## Next Steps

- **Manual Deployment**: See [demo-manual.md](demo-manual.md) for deploying everything via `kubectl`
- **AuthProxy Details**: See the [AuthProxy README](../../AuthProxy/README.md) for inbound
  JWT validation and outbound token exchange internals
- **Multi-Target Demo**: See the [multi-target demo](../multi-target/demo.md) for
  route-based token exchange to multiple tool services
- **Access Policies**: See the [access policies proposal](../../PROPOSAL-access-policies.md)
  for role-based delegation control
- **AuthBridge Overview**: See the [AuthBridge README](../../README.md) for architecture details
